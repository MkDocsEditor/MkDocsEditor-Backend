package metrics

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestServerExposesMetrics(t *testing.T) {
	server, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve()
	defer server.Shutdown(context.Background())

	SetBuildInfo("1.2.3", "abc")
	resp, err := http.Get("http://" + server.Addr() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for _, want := range []string{`mkdocseditor_build_info{commit="abc",version="1.2.3"} 1`, "go_goroutines", "process_cpu_seconds_total"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("no %q in the metrics", want)
		}
	}

	other, err := http.Get("http://" + server.Addr() + "/alive/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Body.Close() }()
	if other.StatusCode != http.StatusNotFound {
		t.Errorf("the metrics server answers other paths: %d", other.StatusCode)
	}
}

func TestListenFailsOnABusyPort(t *testing.T) {
	first, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go first.Serve()
	defer first.Shutdown(context.Background())
	if _, err = Listen(first.Addr()); err == nil {
		t.Fatal("expected an error")
	}
}
