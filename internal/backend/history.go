package backend

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/labstack/echo/v4"
)

// RevisionCurrent names the live text in a diff
const RevisionCurrent = "current"

var revisionPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// ErrUnknownRevision is returned for a revision that is not in the history of the file
var ErrUnknownRevision = errors.New("unknown revision")

// GitCommit is one commit in the history of a file
type GitCommit struct {
	Revision  string
	Time      time.Time
	Author    GitSignature
	CoAuthors []GitSignature
	Subject   string
	// Path of the file in this commit, relative to the docs folder (it changes with renames)
	Path string
}

// FileHistory returns the commits that changed a file, following renames, newest first.
//
// It does not use "git log --follow": that also follows copies, and every new empty page is an
// exact copy of any other empty file, so unrelated pages would show up in the history. Instead
// it follows a file back to the commit that added it, and continues with the old path only if
// that commit renamed the file (status R, the old path is gone in that commit).
func (g *GitRepo) FileHistory(path string, limit int) ([]GitCommit, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	format := "%x1e%H%x00%aI%x00%an%x00%ae%x00%s%x00%(trailers:key=Co-authored-by,valueonly,separator=%x01)%x00"
	commits := []GitCommit{}
	current, start := path, "HEAD"
	seen := map[string]bool{}
	for len(commits) < limit && !seen[current] {
		seen[current] = true
		out, err := g.run(nil, "-c", "diff.renames=false", "log", "--relative", "--name-status",
			"--max-count="+strconv.Itoa(limit-len(commits)), "--format="+format, start, "--", current)
		if err != nil {
			if strings.Contains(err.Error(), "does not have any commits") {
				return commits, nil
			}
			return nil, err
		}
		added := ""
		for _, record := range strings.Split(out, "\x1e") {
			if strings.TrimSpace(record) == "" {
				continue
			}
			fields := strings.Split(record, "\x00")
			if len(fields) < 7 {
				return nil, fmt.Errorf("unexpected git log output %q", record)
			}
			t, _ := time.Parse(time.RFC3339, fields[1])
			c := GitCommit{
				Revision:  fields[0],
				Time:      t,
				Author:    GitSignature{Name: fields[2], Email: fields[3]},
				CoAuthors: []GitSignature{},
				Subject:   fields[4],
				Path:      current,
			}
			for _, co := range strings.Split(fields[5], "\x01") {
				if sig, ok := parseSignature(strings.TrimSpace(co)); ok {
					c.CoAuthors = append(c.CoAuthors, sig)
				}
			}
			added = ""
			for _, line := range strings.Split(fields[6], "\n") {
				if parts := strings.Split(strings.TrimSpace(line), "\t"); len(parts) == 2 && parts[0] == "A" {
					added = c.Revision
				}
			}
			commits = append(commits, c)
		}
		if added == "" || len(commits) >= limit {
			break
		}
		// was the file renamed in the commit that added it?
		out, err = g.run(nil, "-c", "diff.renames=true", "show", "--relative", "-M",
			"--name-status", "--format=", added)
		if err != nil {
			return nil, err
		}
		renamedFrom := ""
		for _, line := range strings.Split(out, "\n") {
			parts := strings.Split(strings.TrimSpace(line), "\t")
			if len(parts) == 3 && strings.HasPrefix(parts[0], "R") && parts[2] == current {
				renamedFrom = parts[1]
			}
		}
		if renamedFrom == "" {
			break
		}
		current, start = renamedFrom, added+"^"
	}
	return commits, nil
}

// parseSignature parses "Name <email>"
func parseSignature(s string) (GitSignature, bool) {
	open := strings.LastIndex(s, "<")
	if open < 0 || !strings.HasSuffix(s, ">") {
		return GitSignature{}, false
	}
	return GitSignature{Name: strings.TrimSpace(s[:open]), Email: s[open+1 : len(s)-1]}, true
}

// FileAt returns the content of a file at a revision
func (g *GitRepo) FileAt(revision string, path string) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	out, err := g.run(nil, "show", revision+":./"+path)
	return []byte(out), err
}

// UnifiedDiff returns the unified diff of two texts, with fromLabel and toLabel as file names
func (g *GitRepo) UnifiedDiff(from, to, fromLabel, toLabel string) (string, error) {
	dir, err := os.MkdirTemp("", "mkdocseditor-diff-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err = os.WriteFile(a, []byte(from), 0o600); err != nil {
		return "", err
	}
	if err = os.WriteFile(b, []byte(to), 0o600); err != nil {
		return "", err
	}
	out, err := g.run(nil, "diff", "--no-index", "--no-color", "--no-ext-diff", "--text", "--", a, b)
	if err != nil && exitCode(err) != 1 {
		return "", err
	}
	// replace git's header (temporary file names) with the labels
	hunks := strings.Index(out, "\n@@")
	if strings.HasPrefix(out, "@@") {
		hunks = -1
	} else if hunks < 0 {
		return "", nil
	}
	return "--- " + fromLabel + "\n+++ " + toLabel + "\n" + out[hunks+1:], nil
}

// HistoryEntry is a commit in the revision history of a document
type HistoryEntry struct {
	Revision  string          `json:"revision"`
	Time      time.Time       `json:"time"`
	Author    HistoryAuthor   `json:"author"`
	CoAuthors []HistoryAuthor `json:"coAuthors"`
	Message   string          `json:"message"`
	Path      string          `json:"path"`
}

// HistoryAuthor is a commit author. Username is empty if the author is not a configured user.
type HistoryAuthor struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Username string `json:"username"`
}

func (rs *RestService) historyAuthor(sig GitSignature) HistoryAuthor {
	a := HistoryAuthor{Name: sig.Name, Email: sig.Email}
	if user, ok := strings.CutSuffix(sig.Email, "@mkdocseditor.invalid"); ok && rs.authenticator != nil && rs.authenticator.User(user) != nil {
		a.Username = user
	} else if rs.authenticator != nil {
		for _, u := range rs.authenticator.Users() {
			if u.Email != "" && strings.EqualFold(u.Email, sig.Email) {
				a.Username = u.Username
				break
			}
		}
	}
	return a
}

// historyContext resolves the document and the repository of a history request
func (rs *RestService) historyContext(c echo.Context) (*Document, string, *GitRepo, error) {
	id := c.Param(urlParamId)
	d := rs.treeManager.GetDocument(id)
	if d == nil {
		return nil, "", nil, rs.ReturnNotFound(c, id)
	}
	if rs.git == nil {
		return nil, "", nil, c.JSONPretty(http.StatusNotFound, &ErrorResult{
			Name:    "Not found",
			Message: "the revision history requires git.enabled",
		}, indentationChar)
	}
	rel, err := filepath.Rel(rs.treeManager.RootPath(), d.Path)
	if err != nil {
		return nil, "", nil, rs.ReturnError(c, err)
	}
	return d, filepath.ToSlash(rel), rs.git.repo, nil
}

// findRevision returns the commit of a revision in the history of a file
func findRevision(repo *GitRepo, rel string, revision string) (*GitCommit, error) {
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
	return c.JSONPretty(http.StatusNotFound, &ErrorResult{
		Name:    "Not found",
		Message: fmt.Sprintf("revision %q is not in the history of this document", revision),
	}, indentationChar)
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
			return c.JSONPretty(http.StatusBadRequest, &ErrorResult{Name: "Bad Request", Message: "limit must be between 1 and 1000"}, indentationChar)
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
	return c.JSONPretty(http.StatusOK, entries, indentationChar)
}

// revisionText returns the text of a document at a revision, or the live text for "current"
func (rs *RestService) revisionText(d *Document, rel string, repo *GitRepo, revision string) (string, string, error) {
	if revision == RevisionCurrent {
		if rs.sync != nil {
			if text, ok := rs.sync.CurrentText(d); ok {
				return text, rel, nil
			}
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
		return c.JSONPretty(http.StatusBadRequest, &ErrorResult{Name: "Bad Request", Message: "missing query parameter 'from'"}, indentationChar)
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
	if rs.sync == nil {
		return echo.ErrNotFound
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
func (s *SyncServer) ReplaceText(d *Document, text string, user *auth.User, message string) error {
	return s.withSession(d, func(ds *docSession) error {
		current, err := ds.text()
		if err != nil {
			return err
		}
		if current == text {
			return nil
		}
		fork, err := ds.doc.Fork()
		if err != nil {
			return err
		}
		actor, err := s.actors.NewServerActor(ActorUserServer)
		if err != nil {
			return err
		}
		if err = fork.SetActorID(actor); err != nil {
			return err
		}
		ds.announceActorLocked(actor, ActorUserServer, nil)
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
			s.changeRecorder.RecordChanges(ds.meta.Path, map[string]int{user.Username: 1})
		}
		ds.broadcastSyncLocked()
		ds.scheduleFlushLocked()
		return nil
	})
}

func exitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
