package backend

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	mutexSync "sync"
)

// GitSignature is the name and email of a commit author
type GitSignature struct {
	Name  string
	Email string
}

func (s GitSignature) String() string {
	return fmt.Sprintf("%s <%s>", s.Name, s.Email)
}

// GitRepo runs the git CLI in the docs folder. Paths are relative to the docs folder.
type GitRepo struct {
	dir       string
	committer GitSignature
	mu        mutexSync.Mutex
}

// OpenGitRepo checks that dir is inside a git work tree
func OpenGitRepo(dir string, committer GitSignature) (*GitRepo, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, errors.New("git is not installed")
	}
	g := &GitRepo{dir: dir, committer: committer}
	out, err := g.run(nil, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return nil, fmt.Errorf("%s is not inside a git repository: %w", dir, err)
	}
	if strings.TrimSpace(out) != "true" {
		return nil, fmt.Errorf("%s is not inside a git work tree", dir)
	}
	return g, nil
}

// run runs git in the docs folder and returns its stdout
func (g *GitRepo) run(env []string, args ...string) (string, error) {
	// the repository may belong to another uid (an NFS volume), git would refuse it
	full := append([]string{"-c", "safe.directory=*", "-c", "core.quotepath=off"}, args...)
	// no shell is involved; paths always follow "--" and values are passed as option arguments
	cmd := exec.Command("git", full...) // #nosec G204
	cmd.Dir = g.dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	cmd.Env = append(cmd.Env, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return stdout.String(), &gitError{args: args, err: err, stderr: strings.TrimSpace(stderr.String())}
	}
	return stdout.String(), nil
}

type gitError struct {
	args   []string
	err    error
	stderr string
}

func (e *gitError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.args, " "), e.err, e.stderr)
}

func (e *gitError) Unwrap() error {
	return e.err
}

func exitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// Commit commits the given paths (files or folders, existing or deleted) and nothing else.
// Other staged changes in the repository stay staged and uncommitted. It returns false if the
// paths have no changes.
func (g *GitRepo) Commit(paths []string, author GitSignature, coAuthors []GitSignature, subject string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	var pathspecs []string
	for _, p := range paths {
		if _, err := os.Lstat(filepath.Join(g.dir, filepath.FromSlash(p))); err == nil {
			pathspecs = append(pathspecs, p)
			continue
		}
		// deleted: only a path git knows can be staged
		out, err := g.run(nil, "ls-files", "--", p)
		if err != nil {
			return false, err
		}
		if strings.TrimSpace(out) != "" {
			pathspecs = append(pathspecs, p)
		}
	}
	if len(pathspecs) == 0 {
		return false, nil
	}

	if _, err := g.run(nil, append([]string{"add", "--all", "--"}, pathspecs...)...); err != nil {
		return false, err
	}
	_, err := g.run(nil, append([]string{"diff", "--cached", "--quiet", "--"}, pathspecs...)...)
	if err == nil {
		return false, nil
	}
	if exitCode(err) != 1 {
		return false, err
	}

	message := subject
	if len(coAuthors) > 0 {
		message += "\n"
		for _, co := range coAuthors {
			message += "\nCo-authored-by: " + co.String()
		}
	}
	env := []string{"GIT_COMMITTER_NAME=" + g.committer.Name, "GIT_COMMITTER_EMAIL=" + g.committer.Email}
	args := []string{"-c", "commit.gpgsign=false", "commit", "--quiet", "--no-verify", "--only",
		"--author", author.String(), "--message", message, "--"}
	if _, err = g.run(env, append(args, pathspecs...)...); err != nil {
		return false, err
	}
	return true, nil
}

// Push pushes the current branch
func (g *GitRepo) Push(remote string, branch string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	args := []string{"push", "--quiet"}
	if remote != "" || branch != "" {
		if remote == "" {
			remote = "origin"
		}
		args = append(args, remote)
		if branch != "" {
			args = append(args, "HEAD:"+branch)
		}
	}
	_, err := g.run(nil, args...)
	return err
}
