package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki/wikitest"
)

func TestQRCodes(t *testing.T) {
	e := newSyncEnv(t)
	e.writeFile("guide/setup.md", "---\nqr: [{key: Waschmaschine, label: Keller}, trockner, -bad]\n---\n# Washing\n")
	e.tm.CreateItemTree()

	getCode := func(path string, status int) map[string]any {
		t.Helper()
		rec := e.do(http.MethodGet, path, "carol")
		if rec.Code != status {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	list := func() *QRListResponse {
		t.Helper()
		var out QRListResponse
		if err := json.Unmarshal(e.do(http.MethodGet, "/qr/", "carol").bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return &out
	}

	var tree wiki.Section
	if err := json.Unmarshal(e.do(http.MethodGet, "/section/", "carol").bytes(), &tree); err != nil {
		t.Fatal(err)
	}
	if d := wikitest.FindDocument(&tree, "setup"); d == nil || len(d.Meta.QR) != 2 || d.Meta.QR[0].Key != "waschmaschine" ||
		*d.Meta.QR[0].Label != "Keller" || d.Meta.QR[1].Label != nil {
		t.Errorf("tree meta = %+v", d)
	}

	code := getCode("/qr/WASCHMASCHINE/", http.StatusOK)
	if code["key"] != "waschmaschine" || code["label"] != "Keller" || code["title"] != "Washing" || code["lastScannedAt"] != nil ||
		code["item"].(map[string]any)["path"] != "guide/setup.md" {
		t.Errorf("code = %v", code)
	}
	// a scan is recorded only with scan=1, after the key reached the store
	e.ss.Activity().Wait()
	if code = getCode("/qr/waschmaschine/?scan=1", http.StatusOK); code["lastScannedAt"] == nil {
		t.Errorf("scan not recorded: %v", code)
	}
	getCode("/qr/unknown/", http.StatusNotFound)
	getCode("/qr/unknown/?scan=1", http.StatusNotFound)
	getCode("/qr/-bad/", http.StatusNotFound)

	l := list()
	if len(l.Codes) != 2 || l.Codes[0].Key != "trockner" || l.Codes[1].LastScannedAt == nil || l.Codes[0].LastScannedAt != nil ||
		len(l.Invalid) != 1 || l.Invalid[0].Value != "-bad" || len(l.Conflicts) != 0 || len(l.Gone) != 0 {
		t.Errorf("list = %+v", l)
	}

	// a second page with the same key is a conflict
	e.writeFile("index.md", "---\nqr: trockner\n---\n# Home\n")
	e.tm.CreateItemTree()
	conflict := getCode("/qr/trockner/?scan=1", http.StatusConflict)
	details := conflict["details"].(map[string]any)
	if conflict["code"] != CodeQRConflict || len(details["items"].([]any)) != 2 || details["lastScannedAt"] == nil {
		t.Errorf("conflict = %v", conflict)
	}
	if l = list(); len(l.Conflicts) != 1 || len(l.Conflicts[0].Items) != 2 || len(l.Codes) != 1 {
		t.Errorf("list = %+v", l)
	}

	// the page is deleted: its keys are gone, also after a restart
	if err := os.Remove(filepath.Join(e.project, "docs", "guide", "setup.md")); err != nil {
		t.Fatal(err)
	}
	e.tm.CreateItemTree()
	e.ss.Activity().Wait()
	e.restart()
	gone := getCode("/qr/waschmaschine/", http.StatusGone)
	details = gone["details"].(map[string]any)
	if gone["code"] != CodeQRGone || details["lastPath"] != "guide/setup.md" || details["label"] != "Keller" || details["lastScannedAt"] == nil {
		t.Errorf("gone = %v", gone)
	}
	if l = list(); len(l.Gone) != 1 || l.Gone[0].Key != "waschmaschine" || len(l.Codes) != 1 || l.Codes[0].Key != "trockner" ||
		l.Codes[0].Item.Path != "index.md" {
		t.Errorf("list = %+v", l)
	}
}
