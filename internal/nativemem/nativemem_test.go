package nativemem

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/metrics"
	automerge "github.com/automerge/automerge-go"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// residentAnon returns the anonymous resident memory of the process in bytes
func residentAnon(t *testing.T) int {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Skip("no /proc/self/status")
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "RssAnon:"); ok {
			kb, err := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "kB")))
			if err != nil {
				t.Fatal(err)
			}
			return kb * 1024
		}
	}
	t.Skip("no RssAnon")
	return 0
}

func release() {
	// the second release returns what the finalizers of the first one freed
	Release()
	time.Sleep(50 * time.Millisecond)
	Release()
}

// TestLoadsDoNotGrowMemory loads a document with a long history again and again, as opening a
// document and the authorship do. Without the arena limit and the release, every load of this
// 13 KB document kept about 7 MB.
func TestLoadsDoNotGrowMemory(t *testing.T) {
	if !Supported() {
		t.Skip("not glibc")
	}
	defer Start()()

	doc := automerge.New()
	if err := doc.Path("content").Set(automerge.NewText("")); err != nil {
		t.Fatal(err)
	}
	text := doc.Path("content").Text()
	for range 3000 {
		if err := text.Insert(0, "abcdefgh "); err != nil {
			t.Fatal(err)
		}
		if _, err := doc.Commit("edit"); err != nil {
			t.Fatal(err)
		}
	}
	saved := doc.Save()

	load := func(n int) {
		for range n {
			loaded, err := automerge.Load(saved)
			if err != nil {
				t.Fatal(err)
			}
			_ = loaded.Heads()
		}
		release()
	}
	// the first loads fill the arenas, after that they reuse them
	for range 4 {
		load(10)
	}
	before := residentAnon(t)
	for range 10 {
		load(10)
	}
	after := residentAnon(t)
	if grown := after - before; grown > 64<<20 {
		t.Fatalf("100 loads kept %d MiB", grown>>20)
	}

	if inUse := testutil.ToFloat64(metrics.NativeHeap.WithLabelValues(metrics.NativeHeapInUse)); inUse <= 0 {
		t.Fatalf("native_heap_bytes{state=in-use} is %v", inUse)
	}
}
