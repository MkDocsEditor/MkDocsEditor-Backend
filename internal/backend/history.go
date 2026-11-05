package backend

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/gitstore"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	"github.com/labstack/echo/v4"
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
}

// HistoryAuthor is a commit author. Username and AccountID are empty if the author is not
// an account. Username is the current username of the account.
type HistoryAuthor struct {
	Name      string `json:"name"`
	Email     string `json:"email"`
	Username  string `json:"username"`
	AccountID string `json:"accountId"`
}

func (rs *RestService) historyAuthor(sig gitstore.Signature) HistoryAuthor {
	a := HistoryAuthor{Name: sig.Name, Email: sig.Email}
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
	}
	return a
}

// historyContext resolves the document and the repository of a history request
func (rs *RestService) historyContext(c echo.Context) (*wiki.Document, string, *gitstore.Repo, error) {
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

func (rs *RestService) returnUnknownRevision(c echo.Context, revision string) error {
	return notFoundError(c, fmt.Sprintf("revision %q is not in the history of this document", revision))
}

// GET /document/:id/history/
func (rs *RestService) getDocumentHistory(c echo.Context) error {
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
	entries := make([]HistoryEntry, 0, len(commits))
	for _, cm := range commits {
		e := HistoryEntry{
			Revision:  cm.Revision,
			Time:      cm.Time,
			Author:    rs.historyAuthor(cm.Author),
			CoAuthors: []HistoryAuthor{},
			Message:   cm.Subject,
			Path:      cm.Path,
		}
		for _, co := range cm.CoAuthors {
			e.CoAuthors = append(e.CoAuthors, rs.historyAuthor(co))
		}
		entries = append(entries, e)
	}
	return c.JSONPretty(http.StatusOK, entries, jsonIndent)
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
func (rs *RestService) getDocumentRevisionContent(c echo.Context) error {
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
func (rs *RestService) getDocumentRevisionDiff(c echo.Context) error {
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
func (rs *RestService) restoreDocumentRevision(c echo.Context) error {
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

// ReplaceText changes the text of a document to text, as splices of a new server actor, so
// connected clients merge it like any other change. The change counts for user in git.
func (s *SyncServer) ReplaceText(d *wiki.Document, text string, user *auth.User, message string) error {
	return s.withSession(d, func(ds *docSession) error {
		current, err := ds.text()
		if err != nil {
			return err
		}
		if current == text {
			return nil
		}
		fork, err := ds.forkForServerChangeLocked(ActorUserServer)
		if err != nil {
			return err
		}
		if err = spliceDiff(fork.Path(contentKey).Text(), current, text); err != nil {
			return err
		}
		if _, err = fork.Commit(message + " by " + user.Username); err != nil {
			return err
		}
		if _, err = ds.doc.Merge(fork); err != nil {
			return err
		}
		ds.meta.LastChange = time.Now().UTC()
		if s.changeRecorder != nil {
			s.changeRecorder.RecordChanges(ds.meta.Path, map[string]int{user.ID: 1})
		}
		ds.broadcastSyncLocked()
		ds.scheduleFlushLocked()
		return nil
	})
}
