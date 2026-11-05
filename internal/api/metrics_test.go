package api

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/collab"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// lockedBuffer is written by the event log and read by the test
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// syncEvents parses the event log
func syncEvents(t *testing.T, b *lockedBuffer) []collab.SyncEvent {
	t.Helper()
	var events []collab.SyncEvent
	for line := range strings.Lines(b.String()) {
		var e collab.SyncEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("not a JSON line: %q: %v", line, err)
		}
		if e.Type != collab.SyncEventType || e.Time == "" {
			t.Fatalf("incomplete event: %q", line)
		}
		events = append(events, e)
	}
	return events
}

func hasEvent(events []collab.SyncEvent, match func(collab.SyncEvent) bool) bool {
	for _, e := range events {
		if match(e) {
			return true
		}
	}
	return false
}

// gaugeValue reads one value of a collector. labelValue is empty for a gauge without labels.
func gaugeValue(t *testing.T, c prometheus.Collector, name string, labelValue string) float64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatal(err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != metrics.Namespace+"_"+name {
			continue
		}
		for _, m := range f.GetMetric() {
			if labelValue == "" || (len(m.GetLabel()) == 1 && m.GetLabel()[0].GetValue() == labelValue) {
				return m.GetGauge().GetValue()
			}
		}
	}
	t.Fatalf("no metric %s{%s}", name, labelValue)
	return 0
}

func TestMetricsAndEventsOfASyncSession(t *testing.T) {
	e := newSyncEnv(t)
	eventLog := &lockedBuffer{}
	e.ss.SetEventLog(eventLog)
	collector := NewMetricsCollector(e.ss, e.tm, nil)
	id := e.docID("guide/setup.md")

	changesBefore := testutil.ToFloat64(metrics.SyncChanges.WithLabelValues("alice"))
	flushesBefore := testutil.ToFloat64(metrics.DocumentFlushes.WithLabelValues(metrics.ResultOK))
	receivedBefore := testutil.ToFloat64(metrics.SyncMessages.WithLabelValues(metrics.DirectionReceived))
	sentBefore := testutil.ToFloat64(metrics.SyncBytes.WithLabelValues(metrics.DirectionSent))
	readOnlyBefore := testutil.ToFloat64(metrics.SyncRejected.WithLabelValues(metrics.RejectReadOnly))
	externalBefore := testutil.ToFloat64(metrics.ExternalChanges.WithLabelValues(metrics.ResultOK))

	alice := newTestClient(t, e, "alice")
	alice.connect(id)
	carol := newTestClient(t, e, "carol")
	carol.connect(id)

	if v := gaugeValue(t, collector, "document_connections", "write"); v != 1 {
		t.Errorf("writers = %v", v)
	}
	if v := gaugeValue(t, collector, "document_connections", "read"); v != 1 {
		t.Errorf("readers = %v", v)
	}
	if v := gaugeValue(t, collector, "users_online", ""); v != 2 {
		t.Errorf("users online = %v", v)
	}
	if v := gaugeValue(t, collector, "documents_loaded", ""); v != 1 {
		t.Errorf("loaded documents = %v", v)
	}
	if v := gaugeValue(t, collector, "tree_items", "document"); v != 2 {
		t.Errorf("documents = %v", v)
	}

	const secret = "text that must stay out of the log"
	alice.appendText(secret)
	eventually(t, "the change on disk", func() bool { return strings.Contains(e.readFile("guide/setup.md"), secret) })
	carol.waitFor("alice's edit", func() bool { return strings.Contains(carol.textLocked(), secret) })

	carol.appendText("dropped")
	eventually(t, "the rejected change", func() bool {
		return testutil.ToFloat64(metrics.SyncRejected.WithLabelValues(metrics.RejectReadOnly)) > readOnlyBefore
	})

	e.writeFile("guide/setup.md", "changed on disk\n")
	e.ss.CheckDisk()

	if testutil.ToFloat64(metrics.SyncChanges.WithLabelValues("alice")) <= changesBefore {
		t.Error("changes not counted")
	}
	if testutil.ToFloat64(metrics.DocumentFlushes.WithLabelValues(metrics.ResultOK)) <= flushesBefore {
		t.Error("flush not counted")
	}
	if testutil.ToFloat64(metrics.SyncMessages.WithLabelValues(metrics.DirectionReceived)) <= receivedBefore {
		t.Error("received messages not counted")
	}
	if testutil.ToFloat64(metrics.SyncBytes.WithLabelValues(metrics.DirectionSent)) <= sentBefore {
		t.Error("sent bytes not counted")
	}
	if testutil.ToFloat64(metrics.ExternalChanges.WithLabelValues(metrics.ResultOK)) <= externalBefore {
		t.Error("external change not counted")
	}

	alice.disconnect()
	carol.disconnect()
	eventually(t, "the document to unload", func() bool {
		return hasEvent(syncEvents(t, eventLog), func(ev collab.SyncEvent) bool { return ev.Kind == collab.SyncEventUnloaded })
	})

	events := syncEvents(t, eventLog)
	const path = "guide/setup.md"
	for what, match := range map[string]func(collab.SyncEvent) bool{
		"load": func(ev collab.SyncEvent) bool { return ev.Kind == collab.SyncEventLoaded && ev.Path == path },
		"alice joined": func(ev collab.SyncEvent) bool {
			return ev.Kind == collab.SyncEventJoined && ev.User == "alice" && ev.Access == "write" && ev.Client == "test"
		},
		"carol joined": func(ev collab.SyncEvent) bool {
			return ev.Kind == collab.SyncEventJoined && ev.User == "carol" && ev.Access == "read"
		},
		"change": func(ev collab.SyncEvent) bool {
			return ev.Kind == collab.SyncEventChangesApplied && ev.User == "alice" && ev.Path == path && ev.Changes > 0 &&
				ev.Bytes > 0 && ev.Heads > 0 && ev.Actor == alice.actorID
		},
		"rejected": func(ev collab.SyncEvent) bool {
			return ev.Kind == collab.SyncEventRejected && ev.User == "carol" && ev.Reason == metrics.RejectReadOnly
		},
		"external": func(ev collab.SyncEvent) bool { return ev.Kind == collab.SyncEventExternalChange && ev.Path == path },
		"flushed": func(ev collab.SyncEvent) bool {
			return ev.Kind == collab.SyncEventFlushed && ev.Path == path && ev.Bytes > 0
		},
		"left": func(ev collab.SyncEvent) bool { return ev.Kind == collab.SyncEventLeft && ev.User == "alice" },
	} {
		if !hasEvent(events, match) {
			t.Errorf("no %s event in %s", what, eventLog.String())
		}
	}
	if strings.Contains(eventLog.String(), secret) {
		t.Error("document text in the event log")
	}
}

// countMethod sums the finished requests of a method label over all routes and statuses
func countMethod(t *testing.T, method string) float64 {
	t.Helper()
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var sum float64
	for _, f := range families {
		if f.GetName() != metrics.Namespace+"_http_requests_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "method" && l.GetValue() == method {
					sum += m.GetCounter().GetValue()
				}
			}
		}
	}
	return sum
}

func TestHTTPMetricsUseRoutePatterns(t *testing.T) {
	e := newSyncEnv(t)
	request := func(method string, path string) {
		t.Helper()
		req, err := http.NewRequest(method, e.http.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	count := func(method, route, status string) float64 {
		return testutil.ToFloat64(metrics.HTTPRequests.WithLabelValues(method, route, status))
	}

	statusBefore := count("GET", "/auth/status/", "200")
	request("GET", "/auth/status")
	if count("GET", "/auth/status/", "200") != statusBefore+1 {
		t.Error("/auth/status/ not counted with its route")
	}

	// readiness probes would drown the requests of real clients
	aliveBefore := count("GET", EndpointPathAlive, "200")
	request("GET", "/alive")
	request("GET", "/alive/")
	if count("GET", EndpointPathAlive, "200") != aliveBefore {
		t.Error("a check of /alive/ is counted")
	}

	// the auth middleware answers before the missing route matters
	const unknown = "/this-path-must-not-become-a-label/"
	unmatchedBefore := count("GET", unmatchedRoute, "401")
	otherBefore := countMethod(t, "other")
	request("GET", unknown)
	request("FOO", unknown)
	if count("GET", unmatchedRoute, "401") != unmatchedBefore+1 {
		t.Error("an unknown path is not counted as unmatched")
	}
	if countMethod(t, "other") != otherBefore+1 {
		t.Error("an unknown method is not counted as other")
	}

	// the pattern is the label, not the id of the document, and 401 is the final status
	routeBefore := count("GET", "/document/:id/", "401")
	request("GET", "/document/"+e.docID("index.md")+"/")
	if count("GET", "/document/:id/", "401") != routeBefore+1 {
		t.Error("request not counted with its route pattern")
	}

	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if strings.Contains(l.GetValue(), "this-path-must-not") {
					t.Errorf("the request path became the label %s of %s", l.GetName(), f.GetName())
				}
			}
		}
	}
}

func TestRequestLogLeavesOutAliveChecks(t *testing.T) {
	e := newSyncEnv(t)

	var logged lockedBuffer
	previous := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(previous) })

	for _, path := range []string{"/alive/", "/auth/status/"} {
		resp, err := http.Get(e.http.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", path, resp.StatusCode)
		}
	}

	output := logged.String()
	if strings.Contains(output, "/alive/") {
		t.Errorf("a check of /alive/ is logged: %s", output)
	}
	if !strings.Contains(output, "GET /auth/status/ 200") {
		t.Errorf("another request is not logged: %s", output)
	}
}
