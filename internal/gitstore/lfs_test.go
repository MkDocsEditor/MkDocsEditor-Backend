package gitstore

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitIn runs git in dir without the user's configuration
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

// newLFSRepo creates a work tree with docs/, .gitattributes for *.jpg and a bare remote
func newLFSRepo(t *testing.T) (work, remote string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	work, remote = t.TempDir(), t.TempDir()
	gitIn(t, remote, "init", "-q", "--bare", "-b", "master")
	gitIn(t, work, "init", "-q", "-b", "master")
	gitIn(t, work, "remote", "add", "origin", "file://"+remote)
	if err := os.MkdirAll(filepath.Join(work, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, ".gitattributes"), []byte("*.jpg filter=lfs diff=lfs merge=lfs -text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "docs", "index.md"), []byte("# Home\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, work, "add", "-A")
	gitIn(t, work, "commit", "-q", "-m", "Initial")
	gitIn(t, work, "push", "-q", "-u", "origin", "master")
	return work, remote
}

func TestLFSCommitAndPush(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("git-lfs is not installed")
	}
	work, remote := newLFSRepo(t)
	repo, err := Open(filepath.Join(work, "docs"), Signature{Name: "Backend", Email: "backend@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if !repo.LFS() {
		t.Fatal("git-lfs not detected")
	}
	image := strings.Repeat("thermal data ", 1000)
	if err = os.WriteFile(filepath.Join(work, "docs", "P2.jpg"), []byte(image), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.Commit([]string{"P2.jpg"}, Signature{Name: "bob", Email: "bob@example.com"}, nil, "Upload P2.jpg", ""); err != nil {
		t.Fatal(err)
	}
	// the commit holds a pointer, the work tree the file
	if blob := gitIn(t, work, "cat-file", "-p", "HEAD:docs/P2.jpg"); !strings.HasPrefix(blob, "version https://git-lfs.github.com/spec/v1") {
		t.Fatalf("committed blob = %q", blob[:min(len(blob), 80)])
	}
	if err = repo.Push("", "", nil); err != nil {
		t.Fatal(err)
	}
	// the remote has the commit and the object
	if got := gitIn(t, remote, "log", "-1", "--format=%s"); got != "Upload P2.jpg\n" {
		t.Errorf("remote head = %q", got)
	}
	clone := t.TempDir()
	gitIn(t, clone, "clone", "-q", "file://"+remote, ".")
	gitIn(t, clone, append(lfsConfig, "lfs", "pull")...)
	if data, _ := os.ReadFile(filepath.Join(clone, "docs", "P2.jpg")); string(data) != image {
		t.Errorf("cloned file has %d bytes", len(data))
	}
}

func TestLFSRequiredWithoutGitLFS(t *testing.T) {
	work, _ := newLFSRepo(t)
	repo, err := Open(filepath.Join(work, "docs"), Signature{Name: "Backend", Email: "backend@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	// git cannot find git-lfs
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err = os.Symlink(gitPath, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("GIT_EXEC_PATH", strings.TrimSpace(gitIn(t, work, "--exec-path")))
	repo.lfs = false
	if err = os.WriteFile(filepath.Join(work, "docs", "P2.jpg"), []byte("raw"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.Commit([]string{"P2.jpg"}, Signature{Name: "bob", Email: "bob@example.com"}, nil, "Upload P2.jpg", ""); err == nil {
		t.Fatal("committed an LFS file without git-lfs")
	}
}

// a repository without LFS files pushes as before, also with git-lfs installed
func TestPushWithoutLFSFiles(t *testing.T) {
	work, remote := newLFSRepo(t)
	if err := os.Remove(filepath.Join(work, ".gitattributes")); err != nil {
		t.Fatal(err)
	}
	repo, err := Open(filepath.Join(work, "docs"), Signature{Name: "Backend", Email: "backend@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(work, "docs", "a.md"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.Commit([]string{"a.md", "../.gitattributes"}, Signature{Name: "bob", Email: "bob@example.com"}, nil, "Add a.md", ""); err != nil {
		t.Fatal(err)
	}
	if err = repo.Push("", "", nil); err != nil {
		t.Fatal(err)
	}
	if got := gitIn(t, remote, "log", "-1", "--format=%s"); got != "Add a.md\n" {
		t.Errorf("remote head = %q", got)
	}
}
