package api

import (
	"net/http"
	"slices"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/gitstore"
	"github.com/labstack/echo/v5"
)

// Who last changed each line of a document: git blame of the live text, with the authorship of
// the characters for the lines not committed yet. See docs/sync-protocol.md section 11.

// FeatureBlame means the server has GET /document/{id}/blame/
const FeatureBlame = "blame"

type (
	// BlameResponse is the answer of GET /document/{id}/blame/
	BlameResponse struct {
		// Revision is the revision (seenToken) of the text the lines belong to
		Revision string `json:"revision"`
		// Lines is the number of lines of the text
		Lines  int          `json:"lines"`
		Blocks []BlameBlock `json:"blocks"`
	}

	// BlameBlock are consecutive lines last changed by the same commit, or not committed yet
	// and written by the same account
	BlameBlock struct {
		// Start and End are the first and the last line, 1-based
		Start int `json:"start"`
		End   int `json:"end"`
		// Pending is true for lines not committed yet; then Revision, Time and Message are null
		Pending  bool    `json:"pending"`
		Revision *string `json:"revision"`
		Time     *string `json:"time"`
		Message  *string `json:"message"`
		// Author is the commit's author, or who wrote most of the pending lines. Null if unknown.
		Author *Author `json:"author"`
		// Agents are the app tokens of the commit or of the pending lines
		Agents []string `json:"agents"`
	}
)

// GET /document/:id/blame/
func (rs *RestService) getDocumentBlame(c *echo.Context) error {
	d, rel, repo, err := rs.historyContext(c)
	if d == nil {
		return err
	}
	text, revision, lineAuthors, err := rs.sync.LineAuthors(d)
	if err != nil {
		return rs.contentError(c, err)
	}
	commits, err := repo.FileHistory(rel, 100000)
	if err != nil {
		return rs.ReturnError(c, err)
	}
	var lines []gitstore.BlameLine
	if len(commits) > 0 {
		if lines, err = repo.Blame(rel, text); err != nil {
			return rs.ReturnError(c, err)
		}
	}
	byRevision := make(map[string]*gitstore.Commit, len(commits))
	for i := range commits {
		byRevision[commits[i].Revision] = &commits[i]
	}

	resp := &BlameResponse{Revision: revision, Lines: len(lineAuthors), Blocks: []BlameBlock{}}
	var key string // what the last block is about
	for i, la := range lineAuthors {
		b := BlameBlock{Start: i + 1, End: i + 1, Agents: []string{}}
		var k string
		if i < len(lines) && lines[i].Revision != gitstore.NotCommitted {
			l := lines[i]
			k = "commit:" + l.Revision
			rev, at := l.Revision, formatTime(l.Time)
			b.Revision, b.Time = &rev, &at
			sig := l.Author
			if cm := byRevision[l.Revision]; cm != nil {
				sig = cm.Author
				msg := cm.Subject
				b.Message = &msg
				for _, a := range cm.Agents {
					if !slices.Contains(b.Agents, a.Name) {
						b.Agents = append(b.Agents, a.Name)
					}
				}
			}
			ha := rs.historyAuthor(sig)
			b.Author = &Author{AccountID: ha.AccountID, Username: ha.Username, DisplayName: ha.DisplayName, Color: ha.Color, Avatar: ha.Avatar}
		} else {
			b.Pending = true
			k = "pending:" + la.AccountID + "\x00" + la.AppToken
			if la.AccountID != "" {
				a := rs.author(la.AccountID)
				b.Author = &a
			}
			if la.AppToken != "" {
				b.Agents = append(b.Agents, la.AppToken)
			}
		}
		if n := len(resp.Blocks); n > 0 && k == key {
			resp.Blocks[n-1].End = i + 1
			continue
		}
		key = k
		resp.Blocks = append(resp.Blocks, b)
	}
	return c.JSONPretty(http.StatusOK, resp, jsonIndent)
}
