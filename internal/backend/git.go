package backend

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"slices"
	"strings"
	"sync"
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
	dir string
	// top is the root of the work tree, prefix the docs folder relative to it ("docs/" or "")
	top       string
	prefix    string
	committer GitSignature
	mu        sync.Mutex
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
	if out, err = g.run(nil, "rev-parse", "--show-toplevel"); err != nil {
		return nil, err
	}
	g.top = strings.TrimRight(out, "\r\n")
	if out, err = g.run(nil, "rev-parse", "--show-prefix"); err != nil {
		return nil, err
	}
	g.prefix = strings.TrimRight(out, "\r\n")
	return g, nil
}

// run runs git in the docs folder and returns its stdout
func (g *GitRepo) run(env []string, args ...string) (string, error) {
	return g.runInput(env, "", args...)
}

// runInput runs git in the docs folder with stdin
func (g *GitRepo) runInput(env []string, stdin string, args ...string) (string, error) {
	return g.runIn(g.dir, env, stdin, args...)
}

// runIn runs git in dir with stdin. User provided values (paths, names, messages) are never passed
// as arguments: paths go through stdin (--pathspec-from-file), authors through the environment
// and messages through a file.
func (g *GitRepo) runIn(dir string, env []string, stdin string, args ...string) (string, error) {
	// the repository may belong to another uid (an NFS volume), git would refuse it.
	// Literal pathspecs: a file named ":(glob)*" is just a file.
	full := append([]string{"-c", "safe.directory=*", "-c", "core.quotepath=off", "--literal-pathspecs"}, args...)
	cmd := exec.Command("git", full...) // #nosec G204 -- fixed git subcommands, no shell, no user values
	cmd.Dir = dir
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

// Commit commits the given paths (files or folders, existing or deleted, relative to the docs
// folder; "../mkdocs.yml" for a file next to it) and nothing else. Other staged changes in the
// repository stay staged and uncommitted. body is optional text between the subject and the
// trailers. It returns the abbreviated hash of the commit, or "" if the paths have no changes.
func (g *GitRepo) Commit(paths []string, author GitSignature, coAuthors []GitSignature, subject string, body string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	pathspecs, err := g.stageablePaths(paths)
	if err != nil || len(pathspecs) == 0 {
		return "", err
	}
	pathspecInput := strings.Join(pathspecs, "\x00")

	if _, err = g.runIn(g.top, nil, pathspecInput, "add", "--all", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
		return "", err
	}
	out, err := g.runIn(g.top, nil, "", "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return "", err
	}
	if !slices.ContainsFunc(strings.Split(out, "\x00"), func(k string) bool { return k != "" && coveredBy(k, pathspecs) }) {
		return "", nil
	}

	messageFile, err := writeCommitMessage(subject, body, coAuthors)
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(messageFile) }()

	env := []string{
		"GIT_AUTHOR_NAME=" + author.Name, "GIT_AUTHOR_EMAIL=" + author.Email,
		"GIT_COMMITTER_NAME=" + g.committer.Name, "GIT_COMMITTER_EMAIL=" + g.committer.Email,
	}
	if _, err = g.runIn(g.top, env, pathspecInput, "-c", "commit.gpgsign=false", "commit", "--quiet", "--no-verify",
		"--only", "--cleanup=whitespace", "--file="+messageFile, "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
		return "", err
	}
	out, err = g.runIn(g.top, nil, "", "rev-parse", "--short=12", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// stageablePaths maps paths to paths relative to the root of the work tree, so files next to the
// docs folder work too. It keeps the paths git can stage: existing files (tracked or not, not
// ignored) and deleted tracked files.
func (g *GitRepo) stageablePaths(paths []string) ([]string, error) {
	out, err := g.runIn(g.top, nil, "", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	known := strings.Split(out, "\x00")
	var pathspecs []string
	for _, p := range paths {
		f := path.Clean(g.prefix + p)
		if f == ".." || strings.HasPrefix(f, "../") || path.IsAbs(f) {
			continue
		}
		if slices.ContainsFunc(known, func(k string) bool { return k != "" && coveredBy(k, []string{f}) }) {
			pathspecs = append(pathspecs, f)
		}
	}
	return pathspecs, nil
}

// coveredBy reports whether a file is one of the paths or below one of them
func coveredBy(file string, paths []string) bool {
	return slices.ContainsFunc(paths, func(p string) bool {
		return file == p || strings.HasPrefix(file, p+"/")
	})
}

// writeCommitMessage writes the message to a temporary file and returns its name
func writeCommitMessage(subject string, body string, coAuthors []GitSignature) (string, error) {
	var message strings.Builder
	message.WriteString(subject)
	if body != "" {
		message.WriteString("\n\n" + body)
	}
	if len(coAuthors) > 0 {
		message.WriteString("\n")
		for _, co := range coAuthors {
			message.WriteString("\nCo-authored-by: " + co.String())
		}
	}
	message.WriteString("\n")

	file, err := os.CreateTemp("", "mkdocseditor-commit-")
	if err != nil {
		return "", err
	}
	_, err = file.WriteString(message.String())
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(file.Name())
		return "", err
	}
	return file.Name(), nil
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
