package gitstore

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Commit is one commit in the history of a file
type Commit struct {
	Revision  string
	Time      time.Time
	Author    Signature
	CoAuthors []Signature
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
func (g *Repo) FileHistory(path string, limit int) ([]Commit, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	commits := []Commit{}
	current, start := path, "HEAD"
	seen := map[string]bool{}
	for len(commits) < limit && !seen[current] {
		seen[current] = true
		out, err := g.run(nil, "-c", "diff.renames=false", "log", "--relative", "--name-status",
			"--max-count="+strconv.Itoa(limit-len(commits)), "--format="+gitLogFormat, start, "--", current)
		if err != nil {
			if strings.Contains(err.Error(), "does not have any commits") {
				return commits, nil
			}
			return nil, err
		}
		added := ""
		for record := range strings.SplitSeq(out, "\x1e") {
			if strings.TrimSpace(record) == "" {
				continue
			}
			c, addsFile, err := parseLogRecord(record, current)
			if err != nil {
				return nil, err
			}
			added = ""
			if addsFile {
				added = c.Revision
			}
			commits = append(commits, c)
		}
		if added == "" || len(commits) >= limit {
			break
		}
		renamedFrom, err := g.renameSource(added, current)
		if err != nil {
			return nil, err
		}
		if renamedFrom == "" {
			break
		}
		current, start = renamedFrom, added+"^"
	}
	return commits, nil
}

// gitLogFormat separates records with 0x1e and fields with 0x00. parseLogRecord reads it.
const gitLogFormat = "%x1e%H%x00%aI%x00%an%x00%ae%x00%s%x00%(trailers:key=Co-authored-by,valueonly,separator=%x01)%x00"

// parseLogRecord parses a record of "git log --name-status --format=gitLogFormat" for the file at
// path. addsFile is true if the commit added the file.
func parseLogRecord(record string, path string) (commit Commit, addsFile bool, err error) {
	fields := strings.Split(record, "\x00")
	if len(fields) < 7 {
		return Commit{}, false, fmt.Errorf("unexpected git log output %q", record)
	}
	t, _ := time.Parse(time.RFC3339, fields[1])
	commit = Commit{
		Revision:  fields[0],
		Time:      t,
		Author:    Signature{Name: fields[2], Email: fields[3]},
		CoAuthors: []Signature{},
		Subject:   fields[4],
		Path:      path,
	}
	for co := range strings.SplitSeq(fields[5], "\x01") {
		if sig, ok := parseSignature(strings.TrimSpace(co)); ok {
			commit.CoAuthors = append(commit.CoAuthors, sig)
		}
	}
	for line := range strings.SplitSeq(fields[6], "\n") {
		if parts := strings.Split(strings.TrimSpace(line), "\t"); len(parts) == 2 && parts[0] == "A" {
			addsFile = true
		}
	}
	return commit, addsFile, nil
}

// renameSource returns the old path if a commit renamed a file to path, else ""
func (g *Repo) renameSource(revision string, path string) (string, error) {
	out, err := g.run(nil, "-c", "diff.renames=true", "show", "--relative", "-M", "--name-status", "--format=", revision)
	if err != nil {
		return "", err
	}
	source := ""
	for line := range strings.SplitSeq(out, "\n") {
		parts := strings.Split(strings.TrimSpace(line), "\t")
		if len(parts) == 3 && strings.HasPrefix(parts[0], "R") && parts[2] == path {
			source = parts[1]
		}
	}
	return source, nil
}

// parseSignature parses "Name <email>"
func parseSignature(s string) (Signature, bool) {
	open := strings.LastIndex(s, "<")
	if open < 0 || !strings.HasSuffix(s, ">") {
		return Signature{}, false
	}
	return Signature{Name: strings.TrimSpace(s[:open]), Email: s[open+1 : len(s)-1]}, true
}

// FileAt returns the content of a file at a revision
func (g *Repo) FileAt(revision string, path string) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	out, err := g.run(nil, "show", revision+":./"+path)
	return []byte(out), err
}

// UnifiedDiff returns the unified diff of two texts, with fromLabel and toLabel as file names
func (g *Repo) UnifiedDiff(from, to, fromLabel, toLabel string) (string, error) {
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

func exitCode(err error) int {
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode()
	}
	return -1
}
