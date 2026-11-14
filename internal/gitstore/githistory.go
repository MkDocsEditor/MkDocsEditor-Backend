package gitstore

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sergi/go-diff/diffmatchpatch"
)

// Commit is one commit in the history of a file
type Commit struct {
	Revision  string
	Time      time.Time
	Author    Signature
	CoAuthors []Signature
	Subject   string
	// AppTokens are the names of the app tokens of the Via-App-Token trailers
	AppTokens []string
	// Agents are the Via-App-Token trailers: the token name and the account that used it
	Agents []Agent
	// Path of the file in this commit, relative to the docs folder (it changes with renames)
	Path string
}

// Agent is an app token that made changes of a commit, and its account
type Agent struct {
	Name    string
	Account Signature
}

// FileHistory returns the commits that changed a file, following renames, newest first.
//
// It does not use "git log --follow": that also follows copies, and every new empty page is an
// exact copy of any other empty file, so unrelated pages would show up in the history. Instead
// it follows a file back to the commit that added it, and continues with the old path only if
// that commit renamed the file (status R, the old path is gone in that commit). Commits of a
// file deleted before the current one was created at the same path are not part of the
// history: that was another document.
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
			commits = append(commits, c)
			if addsFile {
				// the history ends where the file was created: a file deleted earlier at the
				// same path was another document
				added = c.Revision
				break
			}
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
const gitLogFormat = "%x1e%H%x00%aI%x00%an%x00%ae%x00%s%x00%(trailers:key=Co-authored-by,valueonly,separator=%x01)%x00%(trailers:key=Via-App-Token,valueonly,separator=%x01)%x00"

// parseLogRecord parses a record of "git log --name-status --format=gitLogFormat" for the file at
// path. addsFile is true if the commit added the file.
func parseLogRecord(record string, path string) (commit Commit, addsFile bool, err error) {
	fields := strings.Split(record, "\x00")
	if len(fields) < 8 {
		return Commit{}, false, fmt.Errorf("unexpected git log output %q", record)
	}
	t, _ := time.Parse(time.RFC3339, fields[1])
	commit = Commit{
		Revision:  fields[0],
		Time:      t,
		Author:    Signature{Name: fields[2], Email: fields[3]},
		CoAuthors: []Signature{},
		AppTokens: []string{},
		Agents:    []Agent{},
		Subject:   fields[4],
		Path:      path,
	}
	for co := range strings.SplitSeq(fields[5], "\x01") {
		if sig, ok := parseSignature(strings.TrimSpace(co)); ok {
			commit.CoAuthors = append(commit.CoAuthors, sig)
		}
	}
	for v := range strings.SplitSeq(fields[6], "\x01") {
		v = strings.TrimSpace(v)
		if name := appTokenName(v); name != "" {
			commit.AppTokens = append(commit.AppTokens, name)
			commit.Agents = append(commit.Agents, Agent{Name: name, Account: appTokenAccount(v)})
		}
	}
	for line := range strings.SplitSeq(fields[7], "\n") {
		if parts := strings.Split(strings.TrimSpace(line), "\t"); len(parts) == 2 && parts[0] == "A" {
			addsFile = true
		}
	}
	return commit, addsFile, nil
}

// appTokenName returns the token name of a Via-App-Token value "name (Name <email>)"
func appTokenName(v string) string {
	if lt := strings.LastIndex(v, " <"); lt >= 0 && strings.HasSuffix(v, ">)") {
		if open := strings.LastIndex(v[:lt], " ("); open >= 0 {
			return strings.TrimSpace(v[:open])
		}
	}
	return v
}

// appTokenAccount returns the account of a Via-App-Token value "name (Name <email>)"
func appTokenAccount(v string) Signature {
	if lt := strings.LastIndex(v, " <"); lt >= 0 && strings.HasSuffix(v, ">)") {
		if open := strings.LastIndex(v[:lt], " ("); open >= 0 {
			sig, _ := parseSignature(v[open+2 : len(v)-1])
			return sig
		}
	}
	return Signature{}
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

// ErrNotInRevision is returned by FileAt for a revision without the file
var ErrNotInRevision = errors.New("the file is not in this revision")

// FileAt returns the content of a file at a revision, ErrNotInRevision if the revision exists
// but does not have the file (deleted, or not created yet)
func (g *Repo) FileAt(revision string, path string) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	out, err := g.run(nil, "show", revision+":./"+path)
	var gerr *gitError
	if errors.As(err, &gerr) && (strings.Contains(gerr.stderr, "exists on disk, but not in '") ||
		strings.Contains(gerr.stderr, "does not exist in '")) {
		return nil, ErrNotInRevision
	}
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

// NotCommitted is the revision git blame gives lines that are not committed yet
const NotCommitted = "0000000000000000000000000000000000000000"

// BlameLine is the commit that last changed a line
type BlameLine struct {
	// Revision is the full hash, NotCommitted for lines that differ from HEAD
	Revision string
	Author   Signature
	Time     time.Time
}

// blameCacheSize is the number of blames of committed files the repository keeps
const blameCacheSize = 32

// blameCache keeps the blame of files at HEAD, by path and HEAD commit
type blameCache struct {
	mu    sync.Mutex
	order []string
	m     map[string]*committedBlame
}

// committedBlame is the blame of a file as committed
type committedBlame struct {
	text  string
	lines []BlameLine
}

func (c *blameCache) get(key string) *committedBlame {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[key]
}

func (c *blameCache) put(key string, b *committedBlame) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]*committedBlame{}
	}
	if _, ok := c.m[key]; !ok {
		c.order = append(c.order, key)
	}
	c.m[key] = b
	for len(c.order) > blameCacheSize {
		delete(c.m, c.order[0])
		c.order = c.order[1:]
	}
}

// Blame returns for every line of contents, a version of the file (path relative to the docs
// folder) that may have uncommitted changes, the commit that last changed it. Lines that are
// not in the committed file are NotCommitted. It follows renames like the history. The blame
// of the committed file is cached by HEAD, so a text that changes does not run git blame again.
func (g *Repo) Blame(path string, contents string) ([]BlameLine, error) {
	g.mu.Lock()
	head, err := g.run(nil, "rev-parse", "--verify", "HEAD")
	g.mu.Unlock()
	if err != nil {
		return nil, err
	}
	key := strings.TrimSpace(head) + "\x00" + path
	committed := g.blames.get(key)
	if committed == nil {
		if committed, err = g.blameHead(strings.TrimSpace(head), path); err != nil {
			return nil, err
		}
		g.blames.put(key, committed)
	}
	// lines equal in the committed file and in contents keep their commit
	out := make([]BlameLine, 0, strings.Count(contents, "\n")+1)
	oldN := 0
	for _, d := range lineDiff(committed.text, contents) {
		n := len([]rune(d.Text))
		switch d.Type {
		case diffmatchpatch.DiffEqual:
			out = append(out, committed.lines[oldN:oldN+n]...)
			oldN += n
		case diffmatchpatch.DiffDelete:
			oldN += n
		case diffmatchpatch.DiffInsert:
			for range n {
				out = append(out, BlameLine{Revision: NotCommitted})
			}
		}
	}
	return out, nil
}

// lineDiff diffs two texts by lines, every line one rune
func lineDiff(a, b string) []diffmatchpatch.Diff {
	ids := map[string]rune{}
	// private use runes, so every id is a valid rune
	next := rune(0xE000)
	encode := func(text string) []rune {
		if text == "" {
			return nil
		}
		lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
		out := make([]rune, len(lines))
		for i, l := range lines {
			id, ok := ids[l]
			if !ok {
				id = next
				if next++; next == 0xF900 {
					next = 0xF0000
				}
				ids[l] = id
			}
			out[i] = id
		}
		return out
	}
	return diffmatchpatch.New().DiffMainRunes(encode(a), encode(b), false)
}

// blameHead runs git blame on the committed file
func (g *Repo) blameHead(head string, path string) (*committedBlame, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, err := g.run(nil, "cat-file", "-e", head+":./"+path); err != nil {
		// not committed under this path yet: every line is new
		return &committedBlame{}, nil
	}
	text, err := g.run(nil, "show", head+":./"+path)
	if err != nil {
		return nil, err
	}
	out, err := g.run(nil, "blame", "--porcelain", head, "--", "./"+path)
	if err != nil {
		return nil, err
	}
	type info struct {
		author Signature
		time   time.Time
	}
	commits := map[string]*info{}
	var lines []BlameLine
	var current *info
	var revision string
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "\t"):
			lines = append(lines, BlameLine{Revision: revision, Author: current.author, Time: current.time})
		case len(l) > 41 && l[40] == ' ' && isHex(l[:40]):
			revision = l[:40]
			if current = commits[revision]; current == nil {
				current = &info{}
				commits[revision] = current
			}
		case current == nil:
		case strings.HasPrefix(l, "author "):
			current.author.Name = strings.TrimPrefix(l, "author ")
		case strings.HasPrefix(l, "author-mail "):
			current.author.Email = strings.Trim(strings.TrimPrefix(l, "author-mail "), "<>")
		case strings.HasPrefix(l, "author-time "):
			if s, err := strconv.ParseInt(strings.TrimPrefix(l, "author-time "), 10, 64); err == nil {
				current.time = time.Unix(s, 0).UTC()
			}
		}
	}
	if want := len(strings.Split(strings.TrimSuffix(text, "\n"), "\n")); text != "" && len(lines) != want {
		return nil, fmt.Errorf("git blame of %s returned %d lines for %d", path, len(lines), want)
	}
	return &committedBlame{text: text, lines: lines}, nil
}

func isHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
