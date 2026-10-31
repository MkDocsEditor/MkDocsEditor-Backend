package backend

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newGitEnv runs a backend whose project folder is a git repository with one commit
func newGitEnv(t *testing.T, opts GitCommitterOptions) *syncEnv {
	t.Helper()
	// keep the user's git configuration (signing, hooks, templates) out of the tests
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	e := newSyncEnv(t)
	e.stop()
	e.git0("init", "-q", "-b", "master")
	if err := ExcludeFromGit(e.project, e.stateDir); err != nil {
		t.Fatal(err)
	}
	e.git0("add", "-A")
	e.git0("commit", "-q", "-m", "Initial")
	if opts.CheckInterval == 0 {
		opts.CheckInterval = 20 * time.Millisecond
	}
	for _, d := range []*time.Duration{&opts.AfterLeave, &opts.AfterIdle, &opts.MaxInterval} {
		if *d == 0 {
			*d = time.Hour
		}
	}
	e.gitOpts = &opts
	e.start()
	return e
}

// git0 runs git in the project folder as the test user
func (e *syncEnv) git0(args ...string) string {
	e.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = e.project
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

// commits returns the subjects of all commits, newest first
func (e *syncEnv) commits() []string {
	return strings.Split(strings.TrimSpace(e.git0("log", "--format=%s")), "\n")
}

func (e *syncEnv) doBody(method, path, user, body string) *httpRecorder {
	req, _ := http.NewRequest(method, e.http.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(user, testPasswords[user])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	r := &httpRecorder{Code: resp.StatusCode}
	_, _ = r.Body.ReadFrom(resp.Body)
	return r
}

func TestGitCommitAfterLeave(t *testing.T) {
	e := newGitEnv(t, GitCommitterOptions{AfterLeave: 300 * time.Millisecond})
	id := e.docID("guide/setup.md")

	// unrelated staged work of someone else must stay staged and uncommitted
	if err := os.WriteFile(filepath.Join(e.project, "notes.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.git0("add", "notes.txt")

	a := newTestClient(t, e, "alice")
	a.connect(id)
	b := newTestClient(t, e, "bob")
	b.connect(id)
	a.appendText("one\n")
	a.appendText("two\n")
	b.waitFor("a's edits", func() bool { return strings.HasSuffix(b.textLocked(), "two\n") })
	b.appendText("three\n")
	eventually(t, "server", func() bool { return e.serverText("guide/setup.md") == b.text() })

	time.Sleep(500 * time.Millisecond)
	if got := e.commits(); len(got) != 1 {
		t.Fatalf("committed while clients are connected: %v", got)
	}
	a.disconnect()
	b.disconnect()
	eventually(t, "commit", func() bool { return len(e.commits()) == 2 })

	if got := e.git0("log", "-1", "--format=%s%n%an <%ae>%n%cn <%ce>%n%b"); got != "Update guide/setup.md\n"+
		"alice <alice@mkdocseditor.invalid>\nBackend <backend@example.com>\nCo-authored-by: bob <bob@mkdocseditor.invalid>\n\n" {
		t.Errorf("commit:\n%s", got)
	}
	if got := e.git0("show", "HEAD:docs/guide/setup.md"); got != setupText+"one\ntwo\nthree\n" {
		t.Errorf("committed content %q", got)
	}
	if files := e.git0("show", "--format=", "--name-only", "HEAD"); files != "docs/guide/setup.md\n" {
		t.Errorf("committed files %q", files)
	}
	if staged := e.git0("diff", "--cached", "--name-only"); staged != "notes.txt\n" {
		t.Errorf("staged files %q", staged)
	}
}

func TestGitCommitAfterIdleWhileConnected(t *testing.T) {
	e := newGitEnv(t, GitCommitterOptions{AfterIdle: 300 * time.Millisecond})
	a := newTestClient(t, e, "bob")
	a.connect(e.docID("guide/setup.md"))
	a.appendText("idle\n")
	eventually(t, "commit", func() bool { return len(e.commits()) == 2 })
	if got := e.git0("log", "-1", "--format=%an"); got != "bob\n" {
		t.Errorf("author %q", got)
	}
}

func TestGitCommitMaxInterval(t *testing.T) {
	e := newGitEnv(t, GitCommitterOptions{MaxInterval: 400 * time.Millisecond})
	a := newTestClient(t, e, "alice")
	a.connect(e.docID("guide/setup.md"))
	deadline := time.Now().Add(5 * time.Second)
	for len(e.commits()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("no commit during continuous editing")
		}
		a.appendText("x")
		time.Sleep(50 * time.Millisecond)
	}
}

func TestGitNoCommitForDiskChanges(t *testing.T) {
	e := newGitEnv(t, GitCommitterOptions{AfterLeave: 50 * time.Millisecond, AfterIdle: 50 * time.Millisecond})
	a := newTestClient(t, e, "alice")
	a.connect(e.docID("guide/setup.md"))
	e.writeFile("guide/setup.md", setupText+"from git pull\n")
	e.ss.CheckDisk()
	a.waitFor("disk change", func() bool { return strings.HasSuffix(a.textLocked(), "from git pull\n") })
	a.disconnect()
	time.Sleep(400 * time.Millisecond)
	if got := e.commits(); len(got) != 1 {
		t.Errorf("disk changes committed: %v", got)
	}
}

func TestGitCommitsTreeChanges(t *testing.T) {
	e := newGitEnv(t, GitCommitterOptions{})
	guide := e.tm.GenerateId(filepath.Join(e.project, "docs", "guide"))

	if rec := e.doBody(http.MethodPost, "/document/", "bob", `{"parent":"`+guide+`","name":"new"}`); rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if got := e.commits()[0]; got != "Create guide/new.md" {
		t.Errorf("create commit %q", got)
	}

	// pending edits of a renamed document go into the rename commit
	a := newTestClient(t, e, "alice")
	a.connect(e.docID("guide/setup.md"))
	a.appendText("edit before rename\n")
	eventually(t, "server", func() bool { return e.serverText("guide/setup.md") == a.text() })
	if rec := e.doBody(http.MethodPut, "/document/"+e.docID("guide/setup.md")+"/", "bob", `{"name":"install"}`); rec.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", rec.Code, rec.Body.String())
	}
	if got := e.git0("log", "-1", "--format=%s|%an|%b"); got != "Rename guide/setup.md to guide/install.md|bob|Co-authored-by: alice <alice@mkdocseditor.invalid>\n\n" {
		t.Errorf("rename commit %q", got)
	}
	if got := e.git0("show", "HEAD:docs/guide/install.md"); got != setupText+"edit before rename\n" {
		t.Errorf("renamed content %q", got)
	}
	a.disconnect()

	if rec := e.do(http.MethodDelete, "/document/"+e.docID("guide/install.md")+"/?force=true", "bob"); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if got := e.commits()[0]; got != "Delete guide/install.md" {
		t.Errorf("delete commit %q", got)
	}
	if status := e.git0("status", "--porcelain", "--", "docs"); status != "" {
		t.Errorf("uncommitted: %q", status)
	}
}

func TestGitCommitOnShutdownAndAfterCrash(t *testing.T) {
	e := newGitEnv(t, GitCommitterOptions{})
	a := newTestClient(t, e, "alice")
	a.connect(e.docID("guide/setup.md"))
	a.appendText("before shutdown\n")
	eventually(t, "server", func() bool { return e.serverText("guide/setup.md") == a.text() })
	e.restart()
	if got := e.commits(); len(got) != 2 || got[0] != "Update guide/setup.md" {
		t.Fatalf("no commit on shutdown: %v", got)
	}

	// a crash: the pending changes survive in the state folder and are committed after the restart
	a = newTestClient(t, e, "bob")
	a.connect(e.docID("guide/setup.md"))
	a.appendText("before crash\n")
	eventually(t, "server", func() bool { return e.serverText("guide/setup.md") == a.text() })
	eventually(t, "pending saved", func() bool {
		data, err := os.ReadFile(filepath.Join(e.stateDir, gitPendingFile))
		return err == nil && strings.Contains(string(data), e.authn.UserByUsername("bob").ID)
	})
	a.disconnect()
	close(e.git.stop)
	<-e.git.done
	e.git = nil // skip the commit on shutdown
	e.stop()
	e.gitOpts.AfterLeave = 100 * time.Millisecond
	e.start()
	eventually(t, "commit after restart", func() bool { return len(e.commits()) == 3 })
	if got := e.git0("log", "-1", "--format=%an"); got != "bob\n" {
		t.Errorf("author %q", got)
	}
}

// a pending file written before the account database (keyed by usernames) is still committed
func TestGitPendingFileWithUsernames(t *testing.T) {
	e := newGitEnv(t, GitCommitterOptions{AfterLeave: 100 * time.Millisecond})
	e.stop()
	pending := `{"guide/setup.md": {"changes": {"bob": 3, "alice": 1}, "firstChange": "2026-01-01T00:00:00Z", "lastChange": "2026-01-01T00:00:00Z"}}`
	if err := os.WriteFile(filepath.Join(e.project, "docs", "guide", "setup.md"), []byte("# Setup\n\nchanged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.stateDir, gitPendingFile), []byte(pending), 0o600); err != nil {
		t.Fatal(err)
	}
	e.start()
	eventually(t, "commit", func() bool { return len(e.commits()) == 2 })
	if got := e.git0("log", "-1", "--format=%an <%ae>%n%b"); !strings.Contains(got, "bob <bob@mkdocseditor.invalid>") || !strings.Contains(got, "Co-authored-by: alice <alice@mkdocseditor.invalid>") {
		t.Errorf("commit %q", got)
	}
}

// file names are never interpreted as pathspec magic or options
func TestGitCommitsOddNames(t *testing.T) {
	e := newGitEnv(t, GitCommitterOptions{})
	for _, name := range []string{":(exclude)index", "--all", "*"} {
		rec := e.doBody(http.MethodPost, "/document/", "bob", `{"parent":"root","name":"`+name+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("create %q: %d %s", name, rec.Code, rec.Body.String())
		}
		if files := e.git0("show", "--format=", "--name-only", "HEAD"); files != "docs/"+name+".md\n" {
			t.Errorf("commit of %q has files %q", name, files)
		}
	}
	if got := e.commits(); len(got) != 4 {
		t.Errorf("commits %v", got)
	}
}
