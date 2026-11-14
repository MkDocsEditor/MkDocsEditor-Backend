package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/activity"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/gitstore"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	"github.com/labstack/echo/v5"
)

// RevisionCurrent names the live text in a diff
const RevisionCurrent = "current"

var revisionPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// ErrUnknownRevision is returned for a revision that is not in the history of the file
var ErrUnknownRevision = errors.New("unknown revision")

// HistoryEntry is a commit in the revision history of a document
type HistoryEntry struct {
	Revision  string          `json:"revision"`
	Time      time.Time       `json:"time"`
	Author    HistoryAuthor   `json:"author"`
	CoAuthors []HistoryAuthor `json:"coAuthors"`
	Message   string          `json:"message"`
	Path      string          `json:"path"`
	// AppTokens are the names of the app tokens (agents) the changes were made with
	AppTokens []string `json:"appTokens"`
	// Agents are the app tokens with their accounts
	Agents []HistoryAgent `json:"agents"`
	// Summary compares the text of the commit with the one before
	Summary HistorySummary `json:"summary"`
}

// HistoryDiffResponse is the structured diff of GET /document/{id}/history/diff/?format=json
type HistoryDiffResponse struct {
	From  string          `json:"from"`
	To    string          `json:"to"`
	Hunks []activity.Hunk `json:"hunks"`
}

// HistoryAgent is an app token (an agent such as Claude Code) that made changes of a commit
type HistoryAgent struct {
	Name string `json:"name"`
	// AccountID is the account that used the token, "" if it is unknown
	AccountID string `json:"accountId"`
}

// HistorySummary tells what a commit changed in the document
type HistorySummary struct {
	LinesAdded   int `json:"linesAdded"`
	LinesRemoved int `json:"linesRemoved"`
	// Headings are the headings above the changed lines, at most 5, in document order
	Headings []string `json:"headings"`
}

// HistoryAuthor is a commit author. Username and AccountID are empty if the author is not
// an account. Username is the current username of the account. DisplayName, Color and Avatar
// are those of the account, as in the feed, else the git name and a color derived from it.
type HistoryAuthor struct {
	Name        string  `json:"name"`
	Email       string  `json:"email"`
	Username    string  `json:"username"`
	AccountID   string  `json:"accountId"`
	DisplayName string  `json:"displayName"`
	Color       string  `json:"color"`
	Avatar      *string `json:"avatar"`
}

// summaryCache holds the summaries of commits by revision and path: commits never change
type summaryCache struct {
	mu sync.Mutex
	m  map[string]HistorySummary
}

const maxCachedSummaries = 20000

func (c *summaryCache) get(key string) (HistorySummary, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.m[key]
	return s, ok
}

func (c *summaryCache) put(key string, s HistorySummary) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= maxCachedSummaries {
		c.m = map[string]HistorySummary{}
	}
	c.m[key] = s
}

func (rs *RestService) historyAuthor(sig gitstore.Signature) HistoryAuthor {
	a := HistoryAuthor{Name: sig.Name, Email: sig.Email, DisplayName: sig.Name, Color: auth.ColorForUsername(sig.Name)}
	var user *auth.User
	if name, ok := strings.CutSuffix(sig.Email, "@mkdocseditor.invalid"); ok {
		// current or former username
		user = rs.authenticator.UserByUsername(name)
	} else {
		for _, u := range rs.authenticator.Users() {
			if u.Email != "" && strings.EqualFold(u.Email, sig.Email) {
				user = u
				break
			}
		}
	}
	if user != nil {
		a.Username, a.AccountID = user.Username, user.ID
		a.DisplayName, a.Color, a.Avatar = user.DisplayName, user.Color, user.Avatar
	}
	return a
}

// historyContext resolves the document and the repository of a history request
func (rs *RestService) historyContext(c *echo.Context) (*wiki.Document, string, *gitstore.Repo, error) {
	id := c.Param(urlParamID)
	d := rs.treeManager.GetDocument(id)
	if d == nil {
		return nil, "", nil, rs.ReturnNotFound(c, id)
	}
	if rs.git == nil {
		return nil, "", nil, notFoundError(c, "the revision history requires git.enabled")
	}
	return d, d.RelPath, rs.git.Repo(), nil
}

// findRevision returns the commit of a revision in the history of a file
func findRevision(repo *gitstore.Repo, rel string, revision string) (*gitstore.Commit, error) {
	if !revisionPattern.MatchString(revision) {
		return nil, ErrUnknownRevision
	}
	commits, err := repo.FileHistory(rel, 100000)
	if err != nil {
		return nil, err
	}
	for i := range commits {
		if strings.HasPrefix(commits[i].Revision, revision) {
			return &commits[i], nil
		}
	}
	return nil, ErrUnknownRevision
}

func (rs *RestService) returnUnknownRevision(c *echo.Context, revision string) error {
	return notFoundError(c, fmt.Sprintf("revision %q is not in the history of this document", revision))
}

// GET /document/:id/history/
func (rs *RestService) getDocumentHistory(c *echo.Context) error {
	d, rel, repo, err := rs.historyContext(c)
	if d == nil {
		return err
	}
	limit := 100
	if v := c.QueryParam("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > 1000 {
			return apiError(c, http.StatusBadRequest, CodeBadRequest, "limit must be between 1 and 1000", nil)
		}
	}
	commits, err := repo.FileHistory(rel, limit)
	if err != nil {
		return rs.ReturnError(c, err)
	}
	summaries, err := rs.commitSummaries(repo, commits)
	if err != nil {
		return rs.ReturnError(c, err)
	}
	entries := make([]HistoryEntry, 0, len(commits))
	for i, cm := range commits {
		entries = append(entries, rs.historyEntry(cm, summaries[i]))
	}
	return c.JSONPretty(http.StatusOK, entries, jsonIndent)
}

// historyEntry turns a commit into its JSON
func (rs *RestService) historyEntry(cm gitstore.Commit, summary HistorySummary) HistoryEntry {
	e := HistoryEntry{
		Revision:  cm.Revision,
		Time:      cm.Time,
		Author:    rs.historyAuthor(cm.Author),
		CoAuthors: []HistoryAuthor{},
		Message:   cm.Subject,
		Path:      cm.Path,
		AppTokens: cm.AppTokens,
		Agents:    []HistoryAgent{},
		Summary:   summary,
	}
	for _, co := range cm.CoAuthors {
		e.CoAuthors = append(e.CoAuthors, rs.historyAuthor(co))
	}
	for _, a := range cm.Agents {
		e.Agents = append(e.Agents, HistoryAgent{Name: a.Name, AccountID: rs.historyAuthor(a.Account).AccountID})
	}
	return e
}

// commitSummaries summarizes the commits of a file history (newest first). The file history
// only has commits that changed the file, so the text before commit i is the text of commit
// i+1: one read per commit, and none for cached ones.
func (rs *RestService) commitSummaries(repo *gitstore.Repo, commits []gitstore.Commit) ([]HistorySummary, error) {
	out := make([]HistorySummary, len(commits))
	texts := map[int]string{}
	textOf := func(i int) (string, error) {
		if t, ok := texts[i]; ok {
			return t, nil
		}
		var data []byte
		var err error
		if i < len(commits) {
			data, err = repo.FileAt(commits[i].Revision, commits[i].Path)
		} else {
			// before the oldest listed commit: its parent, if the file was there
			last := commits[len(commits)-1]
			data, err = repo.FileAt(last.Revision+"^", last.Path)
			if err != nil {
				data, err = nil, nil
			}
		}
		if err != nil {
			return "", err
		}
		texts[i] = string(data)
		return texts[i], nil
	}
	for i, cm := range commits {
		key := cm.Revision + "\x00" + cm.Path
		if s, ok := rs.historySummaries.get(key); ok {
			out[i] = s
			continue
		}
		after, err := textOf(i)
		if err != nil {
			return nil, err
		}
		before, err := textOf(i + 1)
		if err != nil {
			return nil, err
		}
		s := activity.Summarize(before, after)
		out[i] = HistorySummary{LinesAdded: s.LinesAdded, LinesRemoved: s.LinesRemoved, Headings: s.Headings}
		rs.historySummaries.put(key, out[i])
	}
	return out, nil
}

// revisionText returns the text of a document at a revision, or the live text for "current"
func (rs *RestService) revisionText(d *wiki.Document, rel string, repo *gitstore.Repo, revision string) (string, string, error) {
	if revision == RevisionCurrent {
		if text, ok := rs.sync.CurrentText(d); ok {
			return text, rel, nil
		}
		data, err := os.ReadFile(d.Path)
		return string(data), rel, err
	}
	commit, err := findRevision(repo, rel, revision)
	if err != nil {
		return "", "", err
	}
	data, err := repo.FileAt(commit.Revision, commit.Path)
	return string(data), commit.Path, err
}

// GET /document/:id/history/:revision/content/
func (rs *RestService) getDocumentRevisionContent(c *echo.Context) error {
	d, rel, repo, err := rs.historyContext(c)
	if d == nil {
		return err
	}
	revision := c.Param(urlParamRevision)
	if revision == RevisionCurrent {
		return rs.returnUnknownRevision(c, revision)
	}
	text, _, err := rs.revisionText(d, rel, repo, revision)
	if errors.Is(err, ErrUnknownRevision) {
		return rs.returnUnknownRevision(c, revision)
	}
	if err != nil {
		return rs.ReturnError(c, err)
	}
	return c.String(http.StatusOK, text)
}

// GET /document/:id/history/diff/?from=&to=
func (rs *RestService) getDocumentRevisionDiff(c *echo.Context) error {
	d, rel, repo, err := rs.historyContext(c)
	if d == nil {
		return err
	}
	from, to := c.QueryParam("from"), c.QueryParam("to")
	if to == "" {
		to = RevisionCurrent
	}
	if from == "" {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "missing query parameter 'from'", nil)
	}
	fromText, fromPath, err := rs.revisionText(d, rel, repo, from)
	if errors.Is(err, ErrUnknownRevision) {
		return rs.returnUnknownRevision(c, from)
	}
	if err != nil {
		return rs.ReturnError(c, err)
	}
	toText, toPath, err := rs.revisionText(d, rel, repo, to)
	if errors.Is(err, ErrUnknownRevision) {
		return rs.returnUnknownRevision(c, to)
	}
	if err != nil {
		return rs.ReturnError(c, err)
	}
	if c.QueryParam("format") == "json" {
		return c.JSONPretty(http.StatusOK, &HistoryDiffResponse{From: from, To: to, Hunks: activity.Hunks(fromText, toText)}, jsonIndent)
	}
	diff, err := repo.UnifiedDiff(fromText, toText, "a/"+fromPath+" ("+shortRevision(from)+")", "b/"+toPath+" ("+shortRevision(to)+")")
	if err != nil {
		return rs.ReturnError(c, err)
	}
	return c.String(http.StatusOK, diff)
}

func shortRevision(r string) string {
	if len(r) > 12 && revisionPattern.MatchString(r) {
		return r[:12]
	}
	return r
}

// POST /document/:id/history/:revision/restore/
func (rs *RestService) restoreDocumentRevision(c *echo.Context) error {
	d, rel, repo, err := rs.historyContext(c)
	if d == nil {
		return err
	}
	revision := c.Param(urlParamRevision)
	if revision == RevisionCurrent {
		return rs.returnUnknownRevision(c, revision)
	}
	text, _, err := rs.revisionText(d, rel, repo, revision)
	if errors.Is(err, ErrUnknownRevision) {
		return rs.returnUnknownRevision(c, revision)
	}
	if err != nil {
		return rs.ReturnError(c, err)
	}
	if err = rs.sync.ReplaceText(d, text, UserFromContext(c), "restore "+shortRevision(revision)); err != nil {
		return rs.ReturnError(c, err)
	}
	// committed right away, with the pending edits of the document
	rs.commitTree(c, "Restore %s to "+shortRevision(revision), d.Path)
	return c.NoContent(http.StatusNoContent)
}
