package backend

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
)

const gitPendingFile = "git-pending.json"

// GitCommitterOptions are the commit triggers, see docs/sync-protocol.md section 10
type GitCommitterOptions struct {
	// AfterLeave commits a document when no client has been connected to it for this long
	AfterLeave time.Duration
	// AfterIdle commits a document when it has not changed for this long
	AfterIdle time.Duration
	// MaxInterval commits a document when its oldest uncommitted change is this old
	MaxInterval time.Duration
	// CheckInterval is how often the triggers are checked (default 15 s)
	CheckInterval time.Duration
	Push          bool
	Remote        string
	Branch        string
}

// pendingEdits are the uncommitted user changes of one document
type pendingEdits struct {
	Changes     map[string]int `json:"changes"`
	FirstChange time.Time      `json:"firstChange"`
	LastChange  time.Time      `json:"lastChange"`
	// leftAt is when the committer first saw the document without connections
	leftAt time.Time
}

// GitCommitter batches the user changes of each document into git commits
type GitCommitter struct {
	repo      *GitRepo
	authn     *auth.Service
	opts      GitCommitterOptions
	statePath string
	sync      *SyncServer

	mu      sync.Mutex
	pending map[string]*pendingEdits
	dirty   bool

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// NewGitCommitter creates the committer and loads the uncommitted changes of the last run
func NewGitCommitter(repo *GitRepo, authn *auth.Service, stateDir string, opts GitCommitterOptions) (*GitCommitter, error) {
	if opts.CheckInterval <= 0 {
		opts.CheckInterval = 15 * time.Second
	}
	c := &GitCommitter{
		repo:      repo,
		authn:     authn,
		opts:      opts,
		statePath: filepath.Join(stateDir, gitPendingFile),
		pending:   map[string]*pendingEdits{},
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	data, err := os.ReadFile(c.statePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err = json.Unmarshal(data, &c.pending); err != nil {
			log.Printf("Ignoring the invalid %s: %v", c.statePath, err)
			c.pending = map[string]*pendingEdits{}
		}
	}
	for p, e := range c.pending {
		if e == nil || len(e.Changes) == 0 {
			delete(c.pending, p)
			continue
		}
		e.Changes = c.migrateSubjects(e.Changes)
	}
	return c, nil
}

// Start registers the committer with the sync server and starts checking the triggers
func (c *GitCommitter) Start(syncServer *SyncServer) {
	c.sync = syncServer
	syncServer.SetChangeRecorder(c)
	go c.loop()
}

// RecordChanges implements ChangeRecorder. Changes of the server's own actors (:disk, :server)
// do not count.
func (c *GitCommitter) RecordChanges(relPath string, countsByUser map[string]int) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for user, n := range countsByUser {
		if strings.HasPrefix(user, ":") || n <= 0 {
			continue
		}
		e := c.pending[relPath]
		if e == nil {
			e = &pendingEdits{Changes: map[string]int{}, FirstChange: now}
			c.pending[relPath] = e
		}
		e.Changes[user] += n
		e.LastChange = now
		c.dirty = true
	}
}

func (c *GitCommitter) loop() {
	defer close(c.done)
	ticker := time.NewTicker(c.opts.CheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			c.check(time.Now())
		}
	}
}

// check commits every document whose trigger fired
func (c *GitCommitter) check(now time.Time) {
	c.mu.Lock()
	paths := make([]string, 0, len(c.pending))
	for p := range c.pending {
		paths = append(paths, p)
	}
	c.mu.Unlock()
	sort.Strings(paths)

	// asking the sync server while holding mu could deadlock with RecordChanges
	connected := make(map[string]bool, len(paths))
	for _, p := range paths {
		connected[p] = c.sync != nil && c.sync.HasConnections(p)
	}

	var due []string
	c.mu.Lock()
	for _, p := range paths {
		e := c.pending[p]
		if e == nil {
			continue
		}
		if connected[p] {
			e.leftAt = time.Time{}
		} else if e.leftAt.IsZero() {
			e.leftAt = now
		}
		if (!connected[p] && now.Sub(e.leftAt) >= c.opts.AfterLeave) ||
			now.Sub(e.LastChange) >= c.opts.AfterIdle ||
			now.Sub(e.FirstChange) >= c.opts.MaxInterval {
			due = append(due, p)
		}
	}
	c.mu.Unlock()

	for _, p := range due {
		c.commitEdits(p)
	}
	c.saveIfDirty()
}

// takePending removes and returns the pending edits of the given paths and everything below them
func (c *GitCommitter) takePending(paths ...string) map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := map[string]int{}
	for p, e := range c.pending {
		for _, q := range paths {
			if _, ok := movePath(p, q, q); ok {
				for user, n := range e.Changes {
					total[user] += n
				}
				delete(c.pending, p)
				c.dirty = true
				break
			}
		}
	}
	return total
}

// restorePending puts back changes whose commit failed
func (c *GitCommitter) restorePending(relPath string, changes map[string]int) {
	if len(changes) == 0 {
		return
	}
	c.RecordChanges(relPath, changes)
}

// commitEdits commits the uncommitted user changes of one document
func (c *GitCommitter) commitEdits(relPath string) {
	if c.sync != nil {
		c.sync.FlushPath(relPath)
	}
	changes := c.takePending(relPath)
	if len(changes) == 0 {
		return
	}
	users := c.rankUsers(changes)
	author := c.signature(users[0])
	var coAuthors []GitSignature
	for _, u := range users[1:] {
		coAuthors = append(coAuthors, c.signature(u))
	}
	if _, err := c.commit([]string{relPath}, author, coAuthors, "Update "+relPath, ""); err != nil {
		c.restorePending(relPath, changes)
	}
}

// CommitTreeChange commits a create, rename, delete or upload over REST right away, together
// with the pending edits below the affected paths. The requesting user is the author.
func (c *GitCommitter) CommitTreeChange(user *auth.User, subject string, paths ...string) {
	c.CommitTreeChangeWithBody(user, subject, "", paths...)
}

// CommitTreeChangeWithBody is CommitTreeChange with a message body. It returns the abbreviated
// hash of the commit, or "" if nothing was committed.
func (c *GitCommitter) CommitTreeChangeWithBody(user *auth.User, subject string, body string, paths ...string) string {
	if c.sync != nil {
		c.sync.FlushAll()
	}
	changes := c.takePending(paths...)
	author := c.signature(user.ID)
	var coAuthors []GitSignature
	for _, u := range c.rankUsers(changes) {
		if u != user.ID {
			coAuthors = append(coAuthors, c.signature(u))
		}
	}
	hash, err := c.commit(paths, author, coAuthors, subject, body)
	if err != nil {
		for _, p := range paths {
			// attribute the edits to the first path, the next edit commit takes them along
			c.restorePending(p, changes)
			break
		}
	}
	c.saveIfDirty()
	return hash
}

func (c *GitCommitter) commit(paths []string, author GitSignature, coAuthors []GitSignature, subject string, body string) (string, error) {
	hash, err := c.repo.Commit(paths, author, coAuthors, subject, body)
	if err != nil {
		log.Printf("Unable to commit %s: %v", strings.Join(paths, ", "), err)
		return "", err
	}
	if hash == "" {
		return "", nil
	}
	log.Printf("Committed %q by %s", subject, author)
	if c.opts.Push {
		if err = c.repo.Push(c.opts.Remote, c.opts.Branch); err != nil {
			// the commit stays, the next push takes it along
			log.Printf("Unable to push: %v", err)
		}
	}
	return hash, nil
}

// rankUsers sorts users (subjects) by their number of changes, most first. Ties go by the
// name in the git signature, so the order does not depend on the random account ids.
func (c *GitCommitter) rankUsers(changes map[string]int) []string {
	users := make([]string, 0, len(changes))
	names := make(map[string]string, len(changes))
	for u := range changes {
		users = append(users, u)
		names[u] = c.signature(u).Name
	}
	sort.Slice(users, func(i, j int) bool {
		a, b := users[i], users[j]
		if changes[a] != changes[b] {
			return changes[a] > changes[b]
		}
		if names[a] != names[b] {
			return names[a] < names[b]
		}
		return a < b
	})
	return users
}

// migrateSubjects maps the usernames of a pending file written before the account database
// to account ids
func (c *GitCommitter) migrateSubjects(changes map[string]int) map[string]int {
	if c.authn == nil {
		return changes
	}
	out := make(map[string]int, len(changes))
	for subject, n := range changes {
		if c.authn.UserByID(subject) == nil {
			if u := c.authn.UserByUsername(subject); u != nil {
				subject = u.ID
			}
		}
		out[subject] += n
	}
	return out
}

// signature returns the git author of a subject (account id)
func (c *GitCommitter) signature(subject string) GitSignature {
	name, email, username := subject, "", subject
	if c.authn != nil {
		if u := c.authn.UserByID(subject); u != nil {
			name, email, username = u.DisplayName, u.Email, u.Username
		}
	}
	if email == "" {
		email = username + "@mkdocseditor.invalid"
	}
	return GitSignature{Name: sanitizeSignature(name), Email: sanitizeSignature(email)}
}

// sanitizeSignature removes the characters git does not allow in names and emails
func sanitizeSignature(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '<', '>', '\n', '\r', 0:
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if s == "" {
		return "unknown"
	}
	return s
}

func (c *GitCommitter) saveIfDirty() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.dirty {
		return
	}
	data, err := json.MarshalIndent(c.pending, "", "  ")
	if err == nil {
		err = WriteFile(c.statePath, data)
	}
	if err != nil {
		log.Printf("Unable to save %s: %v", c.statePath, err)
		return
	}
	c.dirty = false
}

// Close stops the triggers and commits all pending changes. Close the sync server first, so
// every document is on disk.
func (c *GitCommitter) Close() {
	c.stopOnce.Do(func() {
		close(c.stop)
		if c.sync != nil {
			<-c.done
		}
		c.mu.Lock()
		paths := make([]string, 0, len(c.pending))
		for p := range c.pending {
			paths = append(paths, p)
		}
		c.mu.Unlock()
		sort.Strings(paths)
		for _, p := range paths {
			c.commitEdits(p)
		}
		c.saveIfDirty()
	})
}
