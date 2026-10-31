package backend

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	return g.runInput(env, "", args...)
}

// runInput runs git with stdin. User provided values (paths, names, messages) are never passed
// as arguments: paths go through stdin (--pathspec-from-file), authors through the environment
// and messages through a file.
func (g *GitRepo) runInput(env []string, stdin string, args ...string) (string, error) {
	// the repository may belong to another uid (an NFS volume), git would refuse it.
	// Literal pathspecs: a file named ":(glob)*" is just a file.
	full := append([]string{"-c", "safe.directory=*", "-c", "core.quotepath=off", "--literal-pathspecs"}, args...)
	cmd := exec.Command("git", full...) // #nosec G204 -- fixed git subcommands, no shell, no user values
	cmd.Dir = g.dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdin = strings.NewReader(stdin)
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

// Commit commits the given paths (files or folders, existing or deleted, relative to the docs
// folder) and nothing else. Other staged changes in the repository stay staged and uncommitted.
// It returns false if the paths have no changes.
func (g *GitRepo) Commit(paths []string, author GitSignature, coAuthors []GitSignature, subject string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	// only paths git can stage: existing files (tracked or not, not ignored) and deleted tracked files
	out, err := g.run(nil, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return false, err
	}
	known := strings.Split(out, "\x00")
	var pathspecs []string
	for _, p := range paths {
		for _, k := range known {
			if k != "" && (k == p || strings.HasPrefix(k, p+"/")) {
				pathspecs = append(pathspecs, p)
				break
			}
		}
	}
	if len(pathspecs) == 0 {
		return false, nil
	}
	pathspecInput := strings.Join(pathspecs, "\x00")

	if _, err = g.runInput(nil, pathspecInput, "add", "--all", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
		return false, err
	}
	out, err = g.run(nil, "diff", "--cached", "--relative", "--name-only", "-z")
	if err != nil {
		return false, err
	}
	changed := false
	for _, k := range strings.Split(out, "\x00") {
		for _, p := range pathspecs {
			if k != "" && (k == p || strings.HasPrefix(k, p+"/")) {
				changed = true
			}
		}
	}
	if !changed {
		return false, nil
	}

	message := subject
	if len(coAuthors) > 0 {
		message += "\n"
		for _, co := range coAuthors {
			message += "\nCo-authored-by: " + co.String()
		}
	}
	messageFile, err := os.CreateTemp("", "mkdocseditor-commit-")
	if err != nil {
		return false, err
	}
	defer func() { _ = os.Remove(messageFile.Name()) }()
	_, err = messageFile.WriteString(message + "\n")
	if cerr := messageFile.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return false, err
	}

	env := []string{
		"GIT_AUTHOR_NAME=" + author.Name, "GIT_AUTHOR_EMAIL=" + author.Email,
		"GIT_COMMITTER_NAME=" + g.committer.Name, "GIT_COMMITTER_EMAIL=" + g.committer.Email,
	}
	if _, err = g.runInput(env, pathspecInput, "-c", "commit.gpgsign=false", "commit", "--quiet", "--no-verify",
		"--only", "--cleanup=whitespace", "--file="+messageFile.Name(), "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
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
