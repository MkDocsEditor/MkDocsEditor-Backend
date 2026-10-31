package backend

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	mutexSync "sync"
	"testing"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	automerge "github.com/automerge/automerge-go"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/bcrypt"
)

// syncEnv runs a backend (REST + sync server) on an httptest server.
// restart() simulates a backend restart on the same docs and state folders.
type syncEnv struct {
	t        *testing.T
	project  string
	tm       *TreeManager
	ss       *SyncServer
	rs       *RestService
	http     *httptest.Server
	authn    *auth.Service
	stateDir string

	// gitOpts enables the git committer when set
	gitOpts *GitCommitterOptions
	git     *GitCommitter
}

var testPasswords = map[string]string{"alice": "alice-pw", "bob": "bob-pw", "carol": "carol-pw"}

func newSyncEnv(t *testing.T) *syncEnv {
	t.Helper()
	project := newTestWiki(t)
	cost := bcrypt.MinCost
	hash := func(pw string) string {
		h, err := bcrypt.GenerateFromPassword([]byte(pw), cost)
		if err != nil {
			t.Fatal(err)
		}
		return string(h)
	}
	configuration.CurrentConfig.Server.Auth.Users = []configuration.UserConfiguration{
		{Username: "alice", PasswordHash: hash("alice-pw"), Role: "admin"},
		{Username: "bob", PasswordHash: hash("bob-pw"), Role: "editor"},
		{Username: "carol", PasswordHash: hash("carol-pw"), Role: "reader"},
	}
	stateDir := filepath.Join(project, ".mkdocseditor")
	authn := newTestAuthenticator(t, stateDir)
	e := &syncEnv{t: t, project: project, authn: authn, stateDir: stateDir}
	e.start()
	t.Cleanup(e.stop)
	return e
}

func (e *syncEnv) start() {
	e.t.Helper()
	e.tm = NewTreeManager()
	ss, err := NewSyncServer(e.tm, e.authn, e.stateDir, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	ss.FlushDelay = 50 * time.Millisecond
	e.ss = ss
	e.rs = NewRestService(e.tm, ss, e.authn)
	if e.gitOpts != nil {
		repo, err := OpenGitRepo(filepath.Join(e.project, "docs"), GitSignature{Name: "Backend", Email: "backend@example.com"})
		if err != nil {
			e.t.Fatal(err)
		}
		e.git, err = NewGitCommitter(repo, e.authn, e.stateDir, *e.gitOpts)
		if err != nil {
			e.t.Fatal(err)
		}
		e.git.Start(ss)
		e.rs.SetGitCommitter(e.git)
	}
	e.http = httptest.NewServer(e.rs.echoRest)
}

func (e *syncEnv) stop() {
	if e.http != nil {
		e.http.CloseClientConnections()
		e.http.Close()
		e.ss.Close()
		if e.git != nil {
			e.git.Close()
			e.git = nil
		}
		e.http = nil
	}
}

func (e *syncEnv) restart() {
	e.stop()
	e.start()
}

func (e *syncEnv) docID(rel string) string {
	return e.tm.GenerateId(filepath.Join(e.project, "docs", filepath.FromSlash(rel)))
}

func (e *syncEnv) readFile(rel string) string {
	b, err := os.ReadFile(filepath.Join(e.project, "docs", filepath.FromSlash(rel)))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

func (e *syncEnv) writeFile(rel string, content string) {
	if err := os.WriteFile(filepath.Join(e.project, "docs", filepath.FromSlash(rel)), []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// eventually polls cond until it is true or fails the test after 5 seconds
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// testClient is a minimal client of the sync protocol built on automerge-go.
// A goroutine processes incoming messages; all state is guarded by mu.
type testClient struct {
	t       *testing.T
	env     *syncEnv
	user    string
	actorID string

	mu        mutexSync.Mutex
	ws        *websocket.Conn
	closed    chan struct{}
	closeErr  error
	failure   string
	doc       *automerge.Doc
	syncState *automerge.SyncState
	epoch     string
	readOnly  bool
	errors    []string
	presences map[string]map[string]any
	actors    map[string]string
	messages  []map[string]any
}

func newTestClient(t *testing.T, env *syncEnv, user string) *testClient {
	return &testClient{t: t, env: env, user: user, actorID: automerge.NewActorID(),
		presences: map[string]map[string]any{}, actors: map[string]string{}}
}

type connectOptions struct {
	useTicket bool
	noHello   bool
	hello     map[string]any
}

// connect opens the websocket and waits for initial-content
func (c *testClient) connect(docID string, opts ...connectOptions) {
	c.t.Helper()
	if err := c.dial(docID, opts...); err != nil {
		c.t.Fatal(err)
	}
	c.waitFor("initial-content", func() bool { return c.doc != nil && c.syncState != nil })
}

func (c *testClient) dial(docID string, opts ...connectOptions) error {
	var o connectOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	url := "ws" + strings.TrimPrefix(c.env.http.URL, "http") + "/document/" + docID + "/ws/"
	header := http.Header{}
	c.mu.Lock()
	hello := map[string]any{"type": "hello", "documentId": docID, "actorId": c.actorID, "client": "test"}
	if c.epoch != "" {
		hello["epoch"] = c.epoch
	}
	c.mu.Unlock()
	if o.useTicket {
		ticket, _, err := c.env.authn.IssueTicket(c.env.authn.UserByUsername(c.user), "")
		if err != nil {
			return err
		}
		hello["ticket"] = ticket
	} else if c.user != "" {
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		req.SetBasicAuth(c.user, testPasswords[c.user])
		header.Set("Authorization", req.Header.Get("Authorization"))
	}
	for k, v := range o.hello {
		hello[k] = v
	}
	ws, resp, err := websocket.DefaultDialer.Dial(url, header)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return err
	}
	closed := make(chan struct{})
	c.mu.Lock()
	c.ws = ws
	c.syncState = nil
	c.closed = closed
	c.closeErr = nil
	c.messages = nil
	c.mu.Unlock()
	go func() {
		defer close(closed)
		for {
			_, data, err := ws.ReadMessage()
			c.mu.Lock()
			if err != nil {
				c.closeErr = err
				c.mu.Unlock()
				return
			}
			var m map[string]any
			if err := json.Unmarshal(data, &m); err != nil {
				c.failure = err.Error()
			} else {
				c.process(m)
			}
			c.mu.Unlock()
		}
	}()
	if !o.noHello {
		c.send(hello)
	}
	return nil
}

func (c *testClient) send(m any) {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ws.WriteJSON(m); err != nil {
		c.t.Fatalf("%s: send: %v", c.user, err)
	}
}

// process handles one incoming message. Caller holds mu.
func (c *testClient) process(m map[string]any) {
	c.messages = append(c.messages, m)
	switch m["type"] {
	case MsgInitialContent:
		epoch := m["epoch"].(string)
		c.readOnly = m["readOnly"].(bool)
		if c.doc == nil || epoch != c.epoch {
			raw, _ := base64.StdEncoding.DecodeString(m["documentState"].(string))
			doc, err := automerge.Load(raw)
			if err == nil {
				err = doc.SetActorID(c.actorID)
			}
			if err != nil {
				c.failure = err.Error()
				return
			}
			c.doc = doc
		}
		c.epoch = epoch
		for _, a := range m["actors"].([]any) {
			am := a.(map[string]any)
			c.actors[am["actorId"].(string)] = am["username"].(string)
		}
		c.syncState = automerge.NewSyncState(c.doc)
		c.flush()
	case MsgSyncRequest:
		raw, _ := base64.StdEncoding.DecodeString(m["syncMessage"].(string))
		if _, err := c.syncState.ReceiveMessage(raw); err != nil {
			c.failure = "receive: " + err.Error()
			return
		}
		c.flush()
	case MsgPresence:
		c.presences[m["clientId"].(string)] = m
	case MsgPresenceSnapshot:
		for _, p := range m["peers"].([]any) {
			pm := p.(map[string]any)
			c.presences[pm["clientId"].(string)] = pm
		}
	case MsgPresenceLeave:
		delete(c.presences, m["clientId"].(string))
	case MsgActor:
		c.actors[m["actorId"].(string)] = m["username"].(string)
	case MsgError:
		c.errors = append(c.errors, m["code"].(string))
	}
}

// flush sends all pending sync messages. Caller holds mu.
func (c *testClient) flush() {
	if c.syncState == nil || c.ws == nil {
		return
	}
	for {
		msg, valid := c.syncState.GenerateMessage()
		if !valid {
			return
		}
		_ = c.ws.WriteJSON(map[string]any{"type": MsgSyncRequest, "documentId": "", "syncMessage": base64.StdEncoding.EncodeToString(msg.Bytes())})
	}
}

// waitFor waits until cond (called with mu held) is true
func (c *testClient) waitFor(what string, cond func() bool) {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		ok := cond()
		failure := c.failure
		closed := c.closed
		c.mu.Unlock()
		if failure != "" {
			c.t.Fatalf("%s: %s", c.user, failure)
		}
		if ok {
			return
		}
		select {
		case <-closed:
			c.mu.Lock()
			ok = cond()
			err := c.closeErr
			c.mu.Unlock()
			if !ok {
				c.t.Fatalf("%s: connection closed while waiting for %s: %v", c.user, what, err)
			}
			return
		default:
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("%s: timed out waiting for %s", c.user, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// settle lets messages flow for a while
func (c *testClient) settle() {
	time.Sleep(150 * time.Millisecond)
}

// waitClosed waits for the server to close the connection and returns the close code
func (c *testClient) waitClosed() int {
	c.t.Helper()
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		c.t.Fatalf("%s: connection not closed", c.user)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ws = nil
	if ce, ok := c.closeErr.(*websocket.CloseError); ok {
		return ce.Code
	}
	return -1
}

func (c *testClient) disconnect() {
	c.mu.Lock()
	ws, closed := c.ws, c.closed
	c.ws = nil
	c.mu.Unlock()
	if ws != nil {
		_ = ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		_ = ws.Close()
		<-closed
	}
}

func (c *testClient) text() string {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.textLocked()
}

func (c *testClient) textLocked() string {
	s, err := contentText(c.doc)
	if err != nil {
		c.t.Fatal(err)
	}
	return s
}

// edit changes the local document and syncs if connected
func (c *testClient) edit(f func(text *automerge.Text) error) {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := f(c.doc.Path(contentKey).Text()); err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.doc.Commit("edit"); err != nil {
		c.t.Fatal(err)
	}
	c.flush()
}

func (c *testClient) appendText(s string) {
	c.edit(func(text *automerge.Text) error { return text.Append(s) })
}

func (c *testClient) insertText(pos int, s string) {
	c.edit(func(text *automerge.Text) error { return text.Insert(pos, s) })
}

// serverText returns the live text of a document on the server, or the file content if it is not loaded
func (e *syncEnv) serverText(rel string) string {
	d := e.tm.GetDocument(e.docID(rel))
	if d == nil {
		e.t.Fatalf("document %s not in tree", rel)
	}
	if text, ok := e.ss.CurrentText(d); ok {
		return text
	}
	return e.readFile(rel)
}
