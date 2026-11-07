package api

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/collab"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/push"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/push/pushtest"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
)

// fakePushService is a push service that records the messages of its subscriptions
type fakePushService struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	devices  map[string]*fakeDevice
	received map[string][]push.Payload
	headers  http.Header
}

type fakeDevice struct {
	key    *ecdh.PrivateKey
	secret []byte
	status int
}

func newFakePushService(t *testing.T) *fakePushService {
	f := &fakePushService{t: t, devices: map[string]*fakeDevice{}, received: map[string][]push.Payload{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePushService) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.devices[r.URL.Path]
	if d == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if d.status != 0 {
		w.WriteHeader(d.status)
		return
	}
	plain, err := pushtest.Decrypt(body, d.key, d.secret)
	if err != nil {
		f.t.Errorf("%s: %v", r.URL.Path, err)
		return
	}
	var p push.Payload
	if err = json.Unmarshal(plain, &p); err != nil {
		f.t.Errorf("%s: %q", r.URL.Path, plain)
	}
	f.received[r.URL.Path] = append(f.received[r.URL.Path], p)
	f.headers = r.Header.Clone()
	w.WriteHeader(http.StatusCreated)
}

// device registers a new device and returns its subscription
func (f *fakePushService) device(name string) map[string]any {
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		f.t.Fatal(err)
	}
	secret := make([]byte, 16)
	_, _ = rand.Read(secret)
	f.mu.Lock()
	f.devices["/up"+name] = &fakeDevice{key: key, secret: secret}
	f.mu.Unlock()
	enc := base64.RawURLEncoding.EncodeToString
	return map[string]any{"endpoint": f.srv.URL + "/up" + name, "keys": map[string]string{"p256dh": enc(key.PublicKey().Bytes()), "auth": enc(secret)}}
}

func (f *fakePushService) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.received["/up"+name])
}

func (f *fakePushService) last(name string) push.Payload {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.received["/up"+name]
	return r[len(r)-1]
}

func (f *fakePushService) setStatus(name string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.devices["/up"+name].status = status
}

// withPush turns push on with a fake push service and a short delay
func (e *syncEnv) withPush() *fakePushService {
	e.t.Helper()
	f := newFakePushService(e.t)
	sender, err := push.NewSender(push.Options{StateDir: e.stateDir, AllowedHosts: []string{f.srv.Listener.Addr().String()}, Subject: "mailto:test@example.com"})
	if err != nil {
		e.t.Fatal(err)
	}
	client := f.srv.Client()
	client.CheckRedirect = sender.Client.CheckRedirect
	sender.Client = client
	n := push.NewNotifier(sender, e.ss.Activity(), e.authn)
	n.Delay = 100 * time.Millisecond
	e.t.Cleanup(n.Close)
	e.rs.SetPush(n)
	return f
}

func (e *syncEnv) subscribe(user string, sub map[string]any, wantStatus int) *PushSubscriptionsResponse {
	e.t.Helper()
	body, _ := json.Marshal(sub)
	rec := e.doBody(http.MethodPost, "/push/subscriptions/", user, string(body))
	if rec.Code != wantStatus {
		e.t.Fatalf("subscribe %s: %d %s", user, rec.Code, rec.Body.String())
	}
	var out PushSubscriptionsResponse
	if wantStatus == http.StatusOK {
		if err := json.Unmarshal(rec.bytes(), &out); err != nil {
			e.t.Fatal(err)
		}
	}
	return &out
}

func TestPushSubscriptions(t *testing.T) {
	e := newSyncEnv(t)
	if rec := e.do(http.MethodGet, "/push/subscriptions/", "bob"); rec.Code != http.StatusNotFound {
		t.Errorf("without push: %d", rec.Code)
	}
	f := e.withPush()

	var info ServerInfoResponse
	resp, err := http.Get(e.http.URL + "/server-info/")
	if err != nil {
		t.Fatal(err)
	}
	err = json.NewDecoder(resp.Body).Decode(&info)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if info.Push == nil || len(info.Push.VAPIDPublicKey) != 87 || !strings.Contains(strings.Join(info.Features, ","), FeaturePush) {
		t.Errorf("server info = %+v %+v", info.Features, info.Push)
	}

	phone := f.device("phone")
	subs := e.subscribe("carol", phone, http.StatusOK)
	if len(subs.Subscriptions) != 1 || !subs.Subscriptions[0].Current || subs.Subscriptions[0].LastPushAt != nil {
		t.Fatalf("subscriptions = %+v", subs)
	}
	// idempotent, and the same endpoint moves to the account that posts it
	e.subscribe("carol", phone, http.StatusOK)
	if subs := e.subscribe("bob", phone, http.StatusOK); len(subs.Subscriptions) != 1 {
		t.Errorf("bob = %+v", subs)
	}
	if rec := e.do(http.MethodGet, "/push/subscriptions/", "carol"); !strings.Contains(rec.Body.String(), `"subscriptions": []`) {
		t.Errorf("carol still has it: %s", rec.Body.String())
	}

	bad := f.device("bad")
	bad["endpoint"] = "https://evil.example.com/up"
	rec := e.doBody(http.MethodPost, "/push/subscriptions/", "bob", mustMarshal(t, bad))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), CodeEndpointNotAllowed) || !strings.Contains(rec.Body.String(), "allowedHosts") {
		t.Errorf("not allowed: %d %s", rec.Code, rec.Body.String())
	}
	bad = f.device("bad")
	bad["keys"] = map[string]string{"p256dh": "AAAA", "auth": "AAAA"}
	if rec := e.doBody(http.MethodPost, "/push/subscriptions/", "bob", mustMarshal(t, bad)); rec.Code != http.StatusBadRequest {
		t.Errorf("bad keys: %d", rec.Code)
	}

	rec = e.doBody(http.MethodDelete, "/push/subscriptions/", "bob", mustMarshal(t, map[string]string{"endpoint": phone["endpoint"].(string)}))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"subscriptions": []`) {
		t.Errorf("delete: %d %s", rec.Code, rec.Body.String())
	}
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPushReachesFollowers(t *testing.T) {
	e := newSyncEnv(t)
	f := e.withPush()
	setup, index := e.docID("guide/setup.md"), e.docID("index.md")
	e.subscribe("bob", f.device("bob"), http.StatusOK)
	e.subscribe("carol", f.device("carol"), http.StatusOK)
	e.pins(http.MethodPut, "/pins/family/"+setup+"/", "alice", http.StatusOK)
	e.pins(http.MethodPut, "/pins/personal/"+index+"/", "bob", http.StatusOK)

	// a family pin reaches everyone but the author, once per editing session
	a := newTestClient(t, e, "alice")
	a.connect(setup)
	a.appendText("More.\n")
	eventually(t, "the pushes of the edit", func() bool { return f.count("bob") == 1 && f.count("carol") == 1 })
	feed := e.feed("carol", "")
	if p := f.last("carol"); p.Type != push.MsgFeedChanged || p.EntryID != feed.Entries[0].ID {
		t.Errorf("payload = %+v, entry %s", p, feed.Entries[0].ID)
	}
	f.mu.Lock()
	h := f.headers
	f.mu.Unlock()
	if h.Get("TTL") != "14400" || h.Get("Urgency") != "normal" || h.Get("Topic") != "feed" || h.Get("Content-Encoding") != "aes128gcm" ||
		!strings.HasPrefix(h.Get("Authorization"), "vapid t=") {
		t.Errorf("headers = %v", h)
	}
	a.appendText("Even more.\n")
	time.Sleep(400 * time.Millisecond)
	if f.count("bob") != 1 || f.count("carol") != 1 {
		t.Errorf("pushed again in the same session: %d %d", f.count("bob"), f.count("carol"))
	}
	a.disconnect()

	// bob's own edit of his pinned page reaches nobody, carol does not follow it
	b := newTestClient(t, e, "bob")
	b.connect(index)
	b.appendText("Bob was here.\n")
	b.disconnect()
	e.ss.Activity().Wait()
	time.Sleep(400 * time.Millisecond)
	if f.count("bob") != 1 || f.count("carol") != 1 {
		t.Errorf("own edit pushed: %d %d", f.count("bob"), f.count("carol"))
	}

	// a subscription that is gone at the push service is removed
	f.setStatus("carol", http.StatusGone)
	e.pins(http.MethodPut, "/pins/family/"+index+"/", "alice", http.StatusOK)
	a = newTestClient(t, e, "alice")
	a.connect(index)
	a.appendText("Alice too.\n")
	eventually(t, "carol's subscription to go", func() bool {
		subs, _ := e.ss.Activity().Subscriptions(e.authn.UserByUsername("carol").ID)
		return len(subs) == 0 && f.count("bob") == 2
	})
	a.disconnect()

	// a subscription of an ended session is removed without a push
	bob := e.authn.UserByUsername("bob")
	sessions, err := e.authn.Sessions(bob.ID)
	if err != nil || len(sessions) == 0 {
		t.Fatal(sessions, err)
	}
	for _, s := range sessions {
		if err := e.authn.RevokeSession(bob.ID, s.ID, nil, auth.RequestInfo{}); err != nil {
			t.Fatal(err)
		}
	}
	e.postMove("alice", collab.MoveRequest{Type: wiki.TypeDocument, ID: index, Name: "home"})
	eventually(t, "bob's subscription to go", func() bool {
		subs, _ := e.ss.Activity().Subscriptions(bob.ID)
		return len(subs) == 0
	})
	if f.count("bob") != 2 {
		t.Errorf("pushed to an ended session: %d", f.count("bob"))
	}
}
