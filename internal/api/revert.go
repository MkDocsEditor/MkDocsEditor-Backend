package api

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/activity"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/collab"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/gitstore"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
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

var errNothingToRevert = errors.New("the change of this revision is not in the document anymore")

// revertError is a revert that conflicts with later edits
type revertError struct {
	conflicts []RevertConflict
	headings  []string
}

func (e *revertError) Error() string {
	msg := "later edits changed the same lines, edit the document by hand"
	if len(e.headings) > 0 {
		msg += " (in " + strings.Join(e.headings, ", ") + ")"
	}
	return msg
}

// revert undoes the change of a commit in the live text of a document and commits that as the
// user. It returns the history entry of the revert commit.
func (rs *RestService) revert(d *wiki.Document, rel string, commit *gitstore.Commit, before, after string, user *auth.User, agent collab.Agent) (*HistoryEntry, error) {
	repo := rs.git.Repo()
	// the revert gets a commit of its own, without the edits pending until now
	rs.git.CommitPending(rel)
	live, liveRevision, err := rs.sync.ReadContent(d)
	if err != nil {
		return nil, err
	}
	merged, conflicts := activity.RevertChange(live, before, after)
	if len(conflicts) > 0 {
		// the change could be made again: it is not in the document anymore
		if again, failed := activity.RevertChange(live, after, before); len(failed) == 0 && again != live {
			return nil, errNothingToRevert
		}
		e := &revertError{conflicts: revertConflicts(live, conflicts), headings: []string{}}
		for _, f := range e.conflicts {
			if f.Heading != nil && !slices.Contains(e.headings, *f.Heading) {
				e.headings = append(e.headings, *f.Heading)
			}
		}
		return nil, e
	}
	if merged == live {
		return nil, errNothingToRevert
	}

	tokenName := agent.TokenName
	// a commit with the revert subject: the edit is committed below, not as an agent snapshot
	agent.TokenName = ""
	if _, err = rs.sync.WriteContent(d, merged, liveRevision, agent); err != nil {
		return nil, err
	}
	if tokenName != "" {
		rs.git.RecordAgent(rel, agent.AccountID, tokenName)
	}
	short := shortRevision(commit.Revision)
	hash := rs.git.CommitTreeChangeWithBody(user, "Revert "+short+" on "+rel, "This reverts commit "+commit.Revision+".", rel)
	if hash == "" {
		return nil, errors.New("the revert was applied but could not be committed")
	}
	commits, err := repo.FileHistory(rel, 1)
	if err == nil && len(commits) == 0 {
		err = errors.New("the revert commit is not in the history")
	}
	if err != nil {
		return nil, err
	}
	summaries, err := rs.commitSummaries(repo, commits)
	if err != nil {
		return nil, err
	}
	entry := rs.historyEntry(commits[0], summaries[0])
	return &entry, nil
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
	entry, err := rs.revert(d, rel, commit, before, after, UserFromContext(c), agentOf(c))
	var conflict *revertError
	switch {
	case errors.As(err, &conflict):
		return apiError(c, http.StatusConflict, CodeRevertConflict, "later edits changed the same lines, edit the document by hand",
			map[string]any{"conflicts": conflict.conflicts, "headings": conflict.headings})
	case errors.Is(err, errNothingToRevert):
		return apiError(c, http.StatusConflict, CodeNothingToRevert, err.Error(), nil)
	case err != nil:
		return rs.contentError(c, err)
	}
	return c.JSONPretty(http.StatusOK, entry, jsonIndent)
}
