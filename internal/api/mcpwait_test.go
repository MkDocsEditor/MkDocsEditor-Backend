package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
)

func TestMCPWaitForChange(t *testing.T) {
	e := newSyncEnv(t)
	rel := "guide/setup.md"
	id := e.docID(rel)
	s := e.mcpSession(e.appToken("bob", "claude", auth.ScopeRead))
	doc := func(args map[string]any) map[string]any {
		args["document"] = rel
		return args
	}

	// nobody has the page open: nobody to wait for, at once
	start := time.Now()
	if out, failed := callTool(t, s, "wait_for_change", doc(map[string]any{"timeout": 5})); !failed || !strings.Contains(out, "nobody has this page open") {
		t.Errorf("nobody open = %q, %v", out, failed)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("nobody open waited %v", time.Since(start))
	}
	for _, timeout := range []int{-1, 31} {
		if out, failed := callTool(t, s, "wait_for_change", doc(map[string]any{"timeout": timeout})); !failed || !strings.Contains(out, "timeout must be") {
			t.Errorf("timeout %d = %q, %v", timeout, out, failed)
		}
	}

	a := newTestClient(t, e, "alice")
	a.connect(id)
	callTool(t, s, "read_document", doc(map[string]any{}))

	// no change: the timeout
	start = time.Now()
	if out, failed := callTool(t, s, "wait_for_change", doc(map[string]any{"timeout": 1})); failed || !strings.Contains(out, "no changes within 1 seconds") {
		t.Errorf("no change = %q, %v", out, failed)
	}
	if d := time.Since(start); d < time.Second || d > 3*time.Second {
		t.Errorf("the timeout took %v", d)
	}

	// a change while it waits: it returns early with the change
	type result struct {
		out    string
		failed bool
	}
	done := make(chan result, 1)
	go func() {
		out, failed := callTool(t, s, "wait_for_change", doc(map[string]any{"timeout": 20}))
		done <- result{out, failed}
	}()
	time.Sleep(200 * time.Millisecond)
	start = time.Now()
	a.appendText("Alice was here.\n")
	select {
	case r := <-done:
		if r.failed || !strings.Contains(r.out, "Alice was here.") || !strings.Contains(r.out, "alice") {
			t.Errorf("change = %q, %v", r.out, r.failed)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("it returned %v after the change", d)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("wait_for_change did not return after the change")
	}

	// the change was marked as seen: the next wait does not return it again
	if out, _ := callTool(t, s, "wait_for_change", doc(map[string]any{"timeout": 1})); !strings.Contains(out, "no changes within") {
		t.Errorf("after seen = %q", out)
	}

	// a change since the last read returns at once
	a.appendText("Again.\n")
	eventually(t, "server", func() bool { return strings.Contains(e.serverText(rel), "Again.") })
	start = time.Now()
	if out, _ := callTool(t, s, "wait_for_change", doc(map[string]any{"timeout": 20})); !strings.Contains(out, "Again.") {
		t.Errorf("changed before = %q", out)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("a change from before waited %v", d)
	}

	// a cancelled wait ends at once, so a client that disconnects holds nothing
	ctx, cancel := context.WithCancel(context.Background())
	revision, _ := e.rs.sync.OpenRevision(e.rs.treeManager.GetDocument(id))
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	start = time.Now()
	if _, changed, err := e.rs.sync.WaitForChange(ctx, e.rs.treeManager.GetDocument(id), revision); err != nil || changed {
		t.Errorf("cancelled = %v, %v", changed, err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("the cancelled wait took %v", d)
	}
}

func TestWaitCounter(t *testing.T) {
	var w waitCounter
	for i, want := range []bool{true, true, false} {
		if got := w.acquire("a"); got != want {
			t.Fatalf("acquire %d = %v", i, got)
		}
	}
	if !w.acquire("b") {
		t.Fatal("another caller")
	}
	w.release("a")
	if !w.acquire("a") {
		t.Error("release")
	}
}
