package api

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/activity"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/gitstore"
	"github.com/labstack/echo/v5"
)

// Reverting a single commit of a document: its change is undone in the live text, the edits
// made since stay. See docs/sync-protocol.md section 11.

const (
	CodeRevertConflict  = "revert-conflict"
	CodeNothingToRevert = "nothing-to-revert"
	// FeatureRevert means the server has POST /document/{id}/history/{revision}/revert/
	FeatureRevert = "revert"
)

// errFirstRevision is returned for the commit that created a file: there is nothing before it
var errFirstRevision = errors.New("the revision created the document, delete the document instead")

type (
	// RevertConflict is where a revert conflicts with later edits
	RevertConflict struct {
		// Heading is the nearest heading of the live text above the conflict, nil above the first
		Heading           *string `json:"heading"`
		HeadingOccurrence int     `json:"headingOccurrence"`
		// Line is the 1-based line of the live text where the conflict starts
		Line int `json:"line"`
	}
)

// revisionChange returns the commit of a revision and the texts of the file before and after it
func revisionChange(repo *gitstore.Repo, rel string, revision string) (*gitstore.Commit, string, string, error) {
	if !revisionPattern.MatchString(revision) {
		return nil, "", "", ErrUnknownRevision
	}
	commits, err := repo.FileHistory(rel, 100000)
	if err != nil {
		return nil, "", "", err
	}
	for i := range commits {
		if !strings.HasPrefix(commits[i].Revision, revision) {
			continue
		}
		after, err := repo.FileAt(commits[i].Revision, commits[i].Path)
		if err != nil {
			return nil, "", "", err
		}
		var before []byte
		if i+1 < len(commits) {
			before, err = repo.FileAt(commits[i+1].Revision, commits[i+1].Path)
		} else if before, err = repo.FileAt(commits[i].Revision+"^", commits[i].Path); err != nil {
			err = errFirstRevision
		}
		if err != nil {
			return nil, "", "", err
		}
		return &commits[i], string(before), string(after), nil
	}
	return nil, "", "", ErrUnknownRevision
}

// revertConflicts describes conflicting lines (0-based) of the live text
func revertConflicts(live string, lines []int) []RevertConflict {
	out := make([]RevertConflict, 0, len(lines))
	for _, line := range lines {
		heading, occurrence := activity.HeadingAbove(live, line)
		out = append(out, RevertConflict{Heading: heading, HeadingOccurrence: occurrence, Line: line + 1})
	}
	return out
}

// POST /document/:id/history/:revision/revert/
func (rs *RestService) revertDocumentRevision(c *echo.Context) error {
	d, rel, repo, err := rs.historyContext(c)
	if d == nil {
		return err
	}
	if err = rs.allowContentWrite(c); err != nil {
		return err
	}
	revision := c.Param(urlParamRevision)
	commit, before, after, err := revisionChange(repo, rel, revision)
	switch {
	case errors.Is(err, ErrUnknownRevision):
		return rs.returnUnknownRevision(c, revision)
	case errors.Is(err, errFirstRevision):
		return apiError(c, http.StatusBadRequest, CodeBadRequest, err.Error(), nil)
	case err != nil:
		return rs.ReturnError(c, err)
	}

	// the revert gets a commit of its own, without the edits pending until now
	rs.git.CommitPending(rel)
	live, liveRevision, err := rs.sync.ReadContent(d)
	if err != nil {
		return rs.contentError(c, err)
	}
	merged, conflicts := activity.RevertChange(live, before, after)
	if len(conflicts) > 0 {
		// the change could be made again: it is not in the document anymore
		if again, failed := activity.RevertChange(live, after, before); len(failed) == 0 && again != live {
			return apiError(c, http.StatusConflict, CodeNothingToRevert, "the change of this revision is not in the document anymore", nil)
		}
		found := revertConflicts(live, conflicts)
		headings := []string{}
		for _, f := range found {
			if f.Heading != nil && !slices.Contains(headings, *f.Heading) {
				headings = append(headings, *f.Heading)
			}
		}
		return apiError(c, http.StatusConflict, CodeRevertConflict,
			"later edits changed the same lines, edit the document by hand",
			map[string]any{"conflicts": found, "headings": headings})
	}
	if merged == live {
		return apiError(c, http.StatusConflict, CodeNothingToRevert, "the change of this revision is not in the document anymore", nil)
	}

	agent := agentOf(c)
	tokenName := agent.TokenName
	// a commit with the revert subject: the edit is committed below, not as an agent snapshot
	agent.TokenName = ""
	if _, err = rs.sync.WriteContent(d, merged, liveRevision, agent); err != nil {
		return rs.contentError(c, err)
	}
	if tokenName != "" {
		rs.git.RecordAgent(rel, agent.AccountID, tokenName)
	}
	short := shortRevision(commit.Revision)
	hash := rs.git.CommitTreeChangeWithBody(UserFromContext(c), "Revert "+short+" on "+rel,
		"This reverts commit "+commit.Revision+".", rel)
	if hash == "" {
		return rs.ReturnError(c, errors.New("the revert was applied but could not be committed"))
	}
	commits, err := repo.FileHistory(rel, 1)
	if err == nil && len(commits) == 0 {
		err = errors.New("the revert commit is not in the history")
	}
	if err != nil {
		return rs.ReturnError(c, err)
	}
	summaries, err := rs.commitSummaries(repo, commits)
	if err != nil {
		return rs.ReturnError(c, err)
	}
	return c.JSONPretty(http.StatusOK, rs.historyEntry(commits[0], summaries[0]), jsonIndent)
}
