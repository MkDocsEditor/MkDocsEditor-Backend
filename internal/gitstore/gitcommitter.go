package gitstore

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/metrics"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
)

const (
	PendingFile   = "git-pending.json"
	SnapshotsFile = "git-snapshots.json"
)

// CommitterOptions are the commit triggers, see docs/sync-protocol.md section 10
type CommitterOptions struct {
	// AfterLeave commits a document when no client has been connected to it for this long
	AfterLeave time.Duration
	// AfterIdle commits a document when it has not changed for this long
	AfterIdle time.Duration
	// MaxInterval commits a document when its oldest uncommitted change is this old
	MaxInterval time.Duration
	// AfterAgentIdle commits the edits of an app token when the token has not edited the
	// document for this long (default 1 min, at most AfterIdle)
	AfterAgentIdle time.Duration
	// CheckInterval is how often the triggers are checked (default 15 s)
	CheckInterval time.Duration
	Push          bool
	Remote        string
	Branch        string
}

// pendingEdits are the uncommitted user changes of one document
type pendingEdits struct {
	Changes map[string]int `json:"changes"`
	// Agents are the names of the app tokens each account (by id) changed the document with
	Agents      map[string][]string `json:"agents,omitempty"`
	FirstChange time.Time           `json:"firstChange"`
	LastChange  time.Time           `json:"lastChange"`
	// leftAt is when the committer first saw the document without connections
	leftAt time.Time
}

// snapshot is a text of a document waiting to be committed. The edits of app tokens become
// snapshots, so they get commits of their own: the human edits before an agent edit are
// committed with the text before it, the agent edit with the text after it. Consecutive edits
// of the same token coalesce into one snapshot.
type snapshot struct {
	Text    string              `json:"text"`
	Changes map[string]int      `json:"changes"`
	Agents  map[string][]string `json:"agents,omitempty"`
	// Account and Token are set for the snapshot of an app token's edits
	Account string    `json:"account,omitempty"`
	Token   string    `json:"token,omitempty"`
	Updated time.Time `json:"updated"`
}

// DocumentSync is what the committer needs from the sync server
type DocumentSync interface {
	// FlushPath writes a loaded document (path relative to the docs folder) to disk now
	FlushPath(relPath string)
	// FlushAll writes all loaded documents to disk now
	FlushAll()
	// HasConnections reports whether clients are connected to a document
	HasConnections(relPath string) bool
}

// Committer batches the user changes of each document into git commits
type Committer struct {
	repo      *Repo
	authn     *auth.Service
	opts      CommitterOptions
	statePath string
	sync      DocumentSync

	mu      sync.Mutex
	pending map[string]*pendingEdits
	// snapshots are the texts to commit before the pending edits of a path, oldest first
	snapshots map[string][]*snapshot
	dirty     bool
	// snapshotMu keeps the commits of the snapshots in order
	snapshotMu sync.Mutex

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// NewCommitter creates the committer and loads the uncommitted changes of the last run
func NewCommitter(repo *Repo, authn *auth.Service, stateDir string, opts CommitterOptions) (*Committer, error) {
	if opts.CheckInterval <= 0 {
		opts.CheckInterval = 15 * time.Second
	}
	if opts.AfterAgentIdle <= 0 {
		opts.AfterAgentIdle = time.Minute
		if opts.AfterIdle > 0 {
			opts.AfterAgentIdle = min(opts.AfterAgentIdle, opts.AfterIdle)
		}
	}
	c := &Committer{
		repo:      repo,
		authn:     authn,
		opts:      opts,
		statePath: filepath.Join(stateDir, PendingFile),
		pending:   map[string]*pendingEdits{},
		snapshots: map[string][]*snapshot{},
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
	data, err = os.ReadFile(filepath.Join(stateDir, SnapshotsFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err = json.Unmarshal(data, &c.snapshots); err != nil {
			log.Printf("Ignoring the invalid %s: %v", SnapshotsFile, err)
			c.snapshots = map[string][]*snapshot{}
		}
	}
	return c, nil
}

// Start starts checking the triggers. The committer learns about changes through RecordChanges,
// so register it with the sync server first.
func (c *Committer) Start(documents DocumentSync) {
	c.sync = documents
	go c.loop()
}

// RecordChanges implements ChangeRecorder. Changes of the server's own actors (:disk, :server)
// do not count.
func (c *Committer) RecordChanges(relPath string, countsByUser map[string]int) {
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

// RecordAgent implements ChangeRecorder: the commit of the document gets a Via-App-Token
// trailer for the token. Call it after RecordChanges.
func (c *Committer) RecordAgent(relPath string, accountID string, tokenName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.pending[relPath]
	if e == nil || tokenName == "" {
		return
	}
	if e.Agents == nil {
		e.Agents = map[string][]string{}
	}
	if !slices.Contains(e.Agents[accountID], tokenName) {
		e.Agents[accountID] = append(e.Agents[accountID], tokenName)
		c.dirty = true
	}
}

// RecordAgentEdit implements ChangeRecorder: an account changed a document with an app token
// from before to after. The edit gets a commit of its own, see snapshot.
func (c *Committer) RecordAgentEdit(relPath string, accountID string, tokenName string, before string, after string) {
	if strings.HasPrefix(accountID, ":") {
		return
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dirty = true
	queue := c.snapshots[relPath]
	if e := c.pending[relPath]; e != nil && len(e.Changes) > 0 {
		// the human edits so far, without the agent edit
		queue = append(queue, &snapshot{Text: before, Changes: e.Changes, Agents: e.Agents, Updated: now})
		delete(c.pending, relPath)
	} else if n := len(queue); n > 0 && queue[n-1].Account == accountID && queue[n-1].Token == tokenName {
		last := queue[n-1]
		last.Text = after
		last.Changes[accountID]++
		last.Updated = now
		return
	}
	c.snapshots[relPath] = append(queue, &snapshot{
		Text:    after,
		Changes: map[string]int{accountID: 1},
		Agents:  map[string][]string{accountID: {tokenName}},
		Account: accountID,
		Token:   tokenName,
		Updated: now,
	})
}

// commitSnapshots commits the snapshots of the given paths and everything below them. With
// due set, only those whose last snapshot is AfterAgentIdle old; of the others, all but the
// last one.
func (c *Committer) commitSnapshots(now time.Time, due bool, paths ...string) {
	c.snapshotMu.Lock()
	defer c.snapshotMu.Unlock()
	type job struct {
		path  string
		queue []*snapshot
	}
	var jobs []job
	c.mu.Lock()
	for p, queue := range c.snapshots {
		inside := len(paths) == 0
		for _, q := range paths {
			inside = inside || wiki.IsInside(p, q)
		}
		if !inside || len(queue) == 0 {
			continue
		}
		take := len(queue)
		if due && now.Sub(queue[take-1].Updated) < c.opts.AfterAgentIdle {
			take--
		}
		if take == 0 {
			continue
		}
		jobs = append(jobs, job{p, queue[:take]})
		if take == len(queue) {
			delete(c.snapshots, p)
		} else {
			c.snapshots[p] = slices.Clone(queue[take:])
		}
		c.dirty = true
	}
	c.mu.Unlock()
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].path < jobs[j].path })

	for _, j := range jobs {
		for i, s := range j.queue {
			if err := c.commitSnapshot(j.path, s); err != nil {
				// put back the rest, before anything queued meanwhile
				c.mu.Lock()
				c.snapshots[j.path] = append(slices.Clone(j.queue[i:]), c.snapshots[j.path]...)
				c.mu.Unlock()
				break
			}
		}
	}
}

// commitSnapshot commits one snapshot without touching the work tree
func (c *Committer) commitSnapshot(relPath string, s *snapshot) error {
	users := c.rankUsers(s.Changes)
	if len(users) == 0 {
		return nil
	}
	author := c.signature(users[0])
	var coAuthors []Signature
	for _, u := range users[1:] {
		coAuthors = append(coAuthors, c.signature(u))
	}
	subject := "Update " + relPath
	start := time.Now()
	hash, err := c.repo.CommitText(relPath, s.Text, author, coAuthors, subject, c.agentTrailers(s.Agents)...)
	metrics.GitCommitDuration.Observe(time.Since(start).Seconds())
	if errors.Is(err, ErrNoHead) {
		// nothing to commit on top of: the next commit of the work tree takes the edits along
		c.restorePending(relPath, s.Changes, s.Agents)
		return nil
	}
	return c.committed(subject, author, hash, err, relPath)
}

func (c *Committer) loop() {
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
func (c *Committer) check(now time.Time) {
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

	c.commitSnapshots(now, true)
	for _, p := range due {
		c.commitEdits(p)
	}
	c.saveIfDirty()
}

// takePending removes and returns the pending edits of the given paths and everything below
// them, and the app tokens they were made with
func (c *Committer) takePending(paths ...string) (map[string]int, map[string][]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := map[string]int{}
	agents := map[string][]string{}
	for p, e := range c.pending {
		for _, q := range paths {
			if wiki.IsInside(p, q) {
				for user, n := range e.Changes {
					total[user] += n
				}
				for user, names := range e.Agents {
					for _, name := range names {
						if !slices.Contains(agents[user], name) {
							agents[user] = append(agents[user], name)
						}
					}
				}
				delete(c.pending, p)
				c.dirty = true
				break
			}
		}
	}
	return total, agents
}

// restorePending puts back changes whose commit failed
func (c *Committer) restorePending(relPath string, changes map[string]int, agents map[string][]string) {
	if len(changes) == 0 {
		return
	}
	c.RecordChanges(relPath, changes)
	for user, names := range agents {
		for _, name := range names {
			c.RecordAgent(relPath, user, name)
		}
	}
}

// agentTrailers returns a Via-App-Token trailer per account and app token, for example
// "Via-App-Token: claude (Markus <markus@example.com>)"
func (c *Committer) agentTrailers(agents map[string][]string) []string {
	var out []string
	for user, names := range agents {
		sig := c.signature(user)
		for _, name := range names {
			out = append(out, "Via-App-Token: "+sanitizeSignature(name)+" ("+sig.String()+")")
		}
	}
	sort.Strings(out)
	return out
}

// commitEdits commits the uncommitted user changes of one document
func (c *Committer) commitEdits(relPath string) {
	if c.sync != nil {
		c.sync.FlushPath(relPath)
	}
	c.commitSnapshots(time.Time{}, false, relPath)
	changes, agents := c.takePending(relPath)
	if len(changes) == 0 {
		return
	}
	users := c.rankUsers(changes)
	author := c.signature(users[0])
	var coAuthors []Signature
	for _, u := range users[1:] {
		coAuthors = append(coAuthors, c.signature(u))
	}
	if _, err := c.commit([]string{relPath}, author, coAuthors, "Update "+relPath, "", c.agentTrailers(agents)...); err != nil {
		c.restorePending(relPath, changes, agents)
	}
}

// CommitPending commits the uncommitted edits of a document now, so the next commit of it
// holds only what follows
func (c *Committer) CommitPending(relPath string) {
	c.commitEdits(relPath)
	c.saveIfDirty()
}

// CommitTreeChange commits a create, rename, delete or upload over REST right away, together
// with the pending edits below the affected paths. The requesting user is the author.
func (c *Committer) CommitTreeChange(user *auth.User, subject string, paths ...string) {
	c.CommitTreeChangeWithBody(user, subject, "", paths...)
}

// CommitTreeChangeWithBody is CommitTreeChange with a message body. It returns the abbreviated
// hash of the commit, or "" if nothing was committed.
func (c *Committer) CommitTreeChangeWithBody(user *auth.User, subject string, body string, paths ...string) string {
	if c.sync != nil {
		c.sync.FlushAll()
	}
	c.commitSnapshots(time.Time{}, false, paths...)
	changes, agents := c.takePending(paths...)
	author := c.signature(user.ID)
	var coAuthors []Signature
	for _, u := range c.rankUsers(changes) {
		if u != user.ID {
			coAuthors = append(coAuthors, c.signature(u))
		}
	}
	hash, err := c.commit(paths, author, coAuthors, subject, body, c.agentTrailers(agents)...)
	if err != nil {
		// attribute the edits to the first path, the next edit commit takes them along
		c.restorePending(paths[0], changes, agents)
	}
	c.saveIfDirty()
	return hash
}

func (c *Committer) commit(paths []string, author Signature, coAuthors []Signature, subject string, body string, trailers ...string) (string, error) {
	start := time.Now()
	hash, err := c.repo.Commit(paths, author, coAuthors, subject, body, trailers...)
	metrics.GitCommitDuration.Observe(time.Since(start).Seconds())
	if err = c.committed(subject, author, hash, err, paths...); err != nil {
		return "", err
	}
	return hash, nil
}

// committed counts a commit and pushes it
func (c *Committer) committed(subject string, author Signature, hash string, err error, paths ...string) error {
	if err != nil {
		metrics.GitCommits.WithLabelValues(metrics.ResultError).Inc()
		log.Printf("Unable to commit %s: %v", strings.Join(paths, ", "), err)
		return err
	}
	if hash == "" {
		metrics.GitCommits.WithLabelValues(metrics.ResultEmpty).Inc()
		return nil
	}
	metrics.GitCommits.WithLabelValues(metrics.ResultOK).Inc()
	log.Printf("Committed %q by %s", subject, author)
	if c.opts.Push {
		if err = c.repo.Push(c.opts.Remote, c.opts.Branch); err != nil {
			// the commit stays, the next push takes it along
			log.Printf("Unable to push: %v", err)
			metrics.GitPushes.WithLabelValues(metrics.ResultError).Inc()
		} else {
			metrics.GitPushes.WithLabelValues(metrics.ResultOK).Inc()
		}
	}
	return nil
}

// rankUsers sorts users (subjects) by their number of changes, most first. Ties go by the
// name in the git signature, so the order does not depend on the random account ids.
func (c *Committer) rankUsers(changes map[string]int) []string {
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
func (c *Committer) migrateSubjects(changes map[string]int) map[string]int {
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
func (c *Committer) signature(subject string) Signature {
	name, email, username := subject, "", subject
	if u := c.authn.UserByID(subject); u != nil {
		name, email, username = u.DisplayName, u.Email, u.Username
	}
	if email == "" {
		email = username + "@mkdocseditor.invalid"
	}
	return Signature{Name: sanitizeSignature(name), Email: sanitizeSignature(email)}
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

func (c *Committer) saveIfDirty() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.dirty {
		return
	}
	data, err := json.MarshalIndent(c.pending, "", "  ")
	if err == nil {
		err = wiki.WriteFile(c.statePath, data)
	}
	if err == nil {
		data, err = json.Marshal(c.snapshots)
	}
	if err == nil {
		err = wiki.WriteFile(filepath.Join(filepath.Dir(c.statePath), SnapshotsFile), data)
	}
	if err != nil {
		log.Printf("Unable to save %s: %v", c.statePath, err)
		return
	}
	c.dirty = false
}

// Close stops the triggers and commits all pending changes. Close the sync server first, so
// every document is on disk.
func (c *Committer) Close() {
	c.stopOnce.Do(func() {
		close(c.stop)
		if c.sync != nil {
			<-c.done
		}
		c.commitSnapshots(time.Time{}, false)
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

// Repo returns the repository the committer works on
func (c *Committer) Repo() *Repo {
	return c.repo
}

// PendingStats returns the number of documents with uncommitted changes and the number of those
// changes
func (c *Committer) PendingStats() (documents int, changes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.pending {
		documents++
		for _, n := range e.Changes {
			changes += n
		}
	}
	for p, queue := range c.snapshots {
		if c.pending[p] == nil {
			documents++
		}
		for _, s := range queue {
			for _, n := range s.Changes {
				changes += n
			}
		}
	}
	return documents, changes
}

// PendingAgents returns the app tokens of the uncommitted changes of a document, by account id
func (c *Committer) PendingAgents(relPath string) map[string][]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string][]string{}
	add := func(agents map[string][]string) {
		for account, names := range agents {
			for _, name := range names {
				if !slices.Contains(out[account], name) {
					out[account] = append(out[account], name)
				}
			}
		}
	}
	for _, s := range c.snapshots[relPath] {
		add(s.Agents)
	}
	if e := c.pending[relPath]; e != nil {
		add(e.Agents)
	}
	return out
}

// Abandon stops the committer without committing anything, as a crash would. The pending changes
// stay in the state folder. Do not call Close afterwards.
func (c *Committer) Abandon() {
	close(c.stop)
	<-c.done
}
