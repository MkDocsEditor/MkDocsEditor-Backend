package api

import (
	"errors"
	"log"
	"net/http"
	"slices"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/activity"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/collab"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	"github.com/labstack/echo/v5"
)

// "Changed since you were here": the server keeps what each account last saw of a document
// (PUT /document/{id}/seen/) and diffs it against the live text (GET /document/{id}/changes/).
// See docs/sync-protocol.md section 11.

// sinceSeen is the default of ?since=: what the account last saw
const sinceSeen = "seen"

// maxCommitsForAgents bounds the commits read to find the agents of the changes
const maxCommitsForAgents = 200

type (
	// SeenRequest is the body of PUT /document/{id}/seen/
	SeenRequest struct {
		// SeenToken is the seenToken of a changes answer, or any revision of the content API
		SeenToken string `json:"seenToken"`
	}

	// SeenResponse is what the server stored
	SeenResponse struct {
		SeenAt    string `json:"seenAt"`
		SeenToken string `json:"seenToken"`
	}

	// ChangesSince is the starting point of the changes
	ChangesSince struct {
		// Time is when the account saw the text, or the time of the newest change of the
		// seenToken given in ?since=
		Time      string `json:"time"`
		SeenToken string `json:"seenToken"`
	}

	// ChangeAuthor is an account that changed the document since, with the app tokens
	// (agents) it used
	ChangeAuthor struct {
		Author
		Agents []string `json:"agents"`
	}

	// ChangesResponse is the answer of GET /document/{id}/changes/
	ChangesResponse struct {
		// Since is nil if the account never saw the document: then there are no hunks
		Since *ChangesSince `json:"since"`
		// SeenToken is the revision of the live text the hunks lead to; PUT it to seen/
		// after showing the changes
		SeenToken string          `json:"seenToken"`
		Authors   []ChangeAuthor  `json:"authors"`
		Hunks     []activity.Hunk `json:"hunks"`
		Summary   HistorySummary  `json:"summary"`
	}
)

// document resolves the document of the request, or answers 404
func (rs *RestService) document(c *echo.Context) (*wiki.Document, error) {
	id := c.Param(urlParamID)
	d := rs.treeManager.GetDocument(id)
	if d == nil {
		return nil, rs.ReturnNotFound(c, id)
	}
	return d, nil
}

// PUT /document/{id}/seen/
func (rs *RestService) putDocumentSeen(c *echo.Context) error {
	d, err := rs.document(c)
	if d == nil {
		return err
	}
	var r SeenRequest
	if err = decodeJSON(c, &r); err != nil {
		return err
	}
	text, _, err := rs.sync.ReadContentAt(d, r.SeenToken)
	if err != nil {
		return rs.revisionError(c, err)
	}
	at, err := rs.sync.Activity().SetSeen(UserFromContext(c).ID, d.RelPath, r.SeenToken, text)
	if err != nil {
		return rs.ReturnError(c, err)
	}
	return c.JSONPretty(http.StatusOK, &SeenResponse{SeenAt: formatTime(at), SeenToken: r.SeenToken}, jsonIndent)
}

// revisionError answers an invalid or unknown seenToken
func (rs *RestService) revisionError(c *echo.Context, err error) error {
	switch {
	case errors.Is(err, collab.ErrInvalidRevision):
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "seenToken must be a revision (seenToken of a changes answer)", nil)
	case errors.Is(err, collab.ErrUnknownRevision):
		return apiError(c, http.StatusConflict, CodeUnknownRevision, err.Error()+", get the changes again", nil)
	}
	return rs.ReturnError(c, err)
}

// GET /document/{id}/changes/?since=seen|<seenToken>
func (rs *RestService) getDocumentChanges(c *echo.Context) error {
	d, err := rs.document(c)
	if d == nil {
		return err
	}
	current, token, err := rs.sync.ReadContent(d)
	if err != nil {
		return rs.ReturnError(c, err)
	}
	resp := &ChangesResponse{SeenToken: token, Authors: []ChangeAuthor{}, Hunks: []activity.Hunk{}, Summary: HistorySummary{Headings: []string{}}}
	var base string
	var since time.Time
	switch s := c.QueryParam("since"); s {
	case "", sinceSeen:
		seen, err := rs.sync.Activity().LastSeen(UserFromContext(c).ID, d.RelPath)
		if err != nil {
			return rs.ReturnError(c, err)
		}
		if seen == nil {
			return c.JSONPretty(http.StatusOK, resp, jsonIndent)
		}
		base, since = seen.Text, seen.Time
		resp.Since = &ChangesSince{Time: formatTime(seen.Time), SeenToken: seen.Token}
	default:
		if base, since, err = rs.sync.ReadContentAt(d, s); err != nil {
			return rs.revisionError(c, err)
		}
		resp.Since = &ChangesSince{Time: formatTime(since), SeenToken: s}
	}
	if base == current {
		return c.JSONPretty(http.StatusOK, resp, jsonIndent)
	}
	resp.Hunks = activity.Hunks(base, current)
	sum := activity.Summarize(base, current)
	resp.Summary = HistorySummary{LinesAdded: sum.LinesAdded, LinesRemoved: sum.LinesRemoved, Headings: sum.Headings}
	resp.Authors = rs.changeAuthors(d.RelPath, since)
	return c.JSONPretty(http.StatusOK, resp, jsonIndent)
}

// changeAuthors are the accounts that edited a document after a time (from the feed), with the
// agents of the commits since and of the changes not committed yet
func (rs *RestService) changeAuthors(rel string, since time.Time) []ChangeAuthor {
	ids, err := rs.sync.Activity().EditAuthorsSince(rel, since)
	if err != nil {
		log.Printf("Unable to read the authors of %s: %v", rel, err)
	}
	agents := map[string][]string{}
	add := func(account, name string) {
		if !slices.Contains(agents[account], name) {
			agents[account] = append(agents[account], name)
		}
		if !slices.Contains(ids, account) {
			ids = append(ids, account)
		}
	}
	if rs.git != nil {
		commits, err := rs.git.Repo().FileHistory(rel, maxCommitsForAgents)
		if err != nil {
			log.Printf("Unable to read the history of %s: %v", rel, err)
		}
		for _, cm := range commits {
			if !cm.Time.After(since) {
				break
			}
			for _, a := range cm.Agents {
				if id := rs.historyAuthor(a.Account).AccountID; id != "" {
					add(id, a.Name)
				}
			}
		}
		for account, names := range rs.git.PendingAgents(rel) {
			for _, n := range names {
				add(account, n)
			}
		}
	}
	out := make([]ChangeAuthor, 0, len(ids))
	for _, id := range ids {
		names := agents[id]
		if names == nil {
			names = []string{}
		}
		out = append(out, ChangeAuthor{Author: rs.author(id), Agents: names})
	}
	return out
}
