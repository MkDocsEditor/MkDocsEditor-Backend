package backend

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
)

type restTestEnv struct {
	t       *testing.T
	project string
	tm      *TreeManager
	rs      *RestService
	user    string
	pass    string
}

func newRestTestEnv(t *testing.T) *restTestEnv {
	t.Helper()
	project := newTestWiki(t)
	tm := NewTreeManager()
	sm := NewAutomergeSyncManager(tm)
	rs := NewRestService(tm, sm)
	wcm := NewWebsocketConnectionManager(tm)
	rs.RegisterWebsocketHandler(wcm)
	sm.SetWebsocketConnectionManager(wcm)
	return &restTestEnv{t: t, project: project, tm: tm, rs: rs}
}

func (e *restTestEnv) do(method, path string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if e.user != "" {
		req.SetBasicAuth(e.user, e.pass)
	}
	rec := httptest.NewRecorder()
	e.rs.echoRest.ServeHTTP(rec, req)
	return rec
}

func (e *restTestEnv) doJSON(method, path string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.do(method, path, bytes.NewReader(b), "application/json")
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func expectStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, want, rec.Body.String())
	}
}

func TestRestAliveAndTree(t *testing.T) {
	e := newRestTestEnv(t)

	expectStatus(t, e.do(http.MethodGet, "/alive/", nil, ""), http.StatusOK)
	// trailing slash is added automatically
	expectStatus(t, e.do(http.MethodGet, "/alive", nil, ""), http.StatusOK)

	rec := e.do(http.MethodGet, "/section/", nil, "")
	expectStatus(t, rec, http.StatusOK)
	root := decode[Section](t, rec)
	if root.ID != RootSectionID || findSectionByName(&root, "guide") == nil {
		t.Fatalf("unexpected tree: %s", rec.Body.String())
	}

	guide := findSectionByName(&root, "guide")
	rec = e.do(http.MethodGet, "/section/"+guide.ID+"/", nil, "")
	expectStatus(t, rec, http.StatusOK)
	if decode[Section](t, rec).Name != "guide" {
		t.Error("wrong section returned")
	}
	expectStatus(t, e.do(http.MethodGet, "/section/unknown/", nil, ""), http.StatusNotFound)

	setup := findDocumentByName(&root, "setup")
	rec = e.do(http.MethodGet, "/document/"+setup.ID+"/", nil, "")
	expectStatus(t, rec, http.StatusOK)
	if decode[Document](t, rec).Name != "setup" {
		t.Error("wrong document returned")
	}
	rec = e.do(http.MethodGet, "/document/"+setup.ID+"/content/", nil, "")
	expectStatus(t, rec, http.StatusOK)
	if rec.Body.String() != "# Setup\n\nSome text.\n" {
		t.Errorf("content = %q", rec.Body.String())
	}
	expectStatus(t, e.do(http.MethodGet, "/document/unknown/", nil, ""), http.StatusNotFound)
	expectStatus(t, e.do(http.MethodGet, "/document/unknown/content/", nil, ""), http.StatusNotFound)

	png := findResourceByName(&root, "image.png")
	rec = e.do(http.MethodGet, "/resource/"+png.ID+"/", nil, "")
	expectStatus(t, rec, http.StatusOK)
	rec = e.do(http.MethodGet, "/resource/"+png.ID+"/content/", nil, "")
	expectStatus(t, rec, http.StatusOK)
	if rec.Body.String() != "\x89PNG\r\n\x1a\n\x00\x01binary" {
		t.Errorf("resource content = %q", rec.Body.String())
	}
	expectStatus(t, e.do(http.MethodGet, "/resource/unknown/", nil, ""), http.StatusNotFound)
}

func TestRestMkDocsConfig(t *testing.T) {
	e := newRestTestEnv(t)
	rec := e.do(http.MethodGet, "/mkdocs/config/", nil, "")
	expectStatus(t, rec, http.StatusOK)
	if decode[map[string]any](t, rec)["site_name"] != "Test Wiki" {
		t.Errorf("unexpected config: %s", rec.Body.String())
	}
}

func TestRestSectionLifecycle(t *testing.T) {
	e := newRestTestEnv(t)

	rec := e.doJSON(http.MethodPost, "/section/", NewSectionRequest{Parent: RootSectionID, Name: "notes"})
	expectStatus(t, rec, http.StatusOK)
	section := decode[Section](t, rec)
	if section.Name != "notes" || section.ID == "" {
		t.Fatalf("unexpected section: %+v", section)
	}
	if _, err := os.Stat(filepath.Join(e.project, "docs", "notes")); err != nil {
		t.Fatal(err)
	}

	expectStatus(t, e.doJSON(http.MethodPost, "/section/", NewSectionRequest{Parent: RootSectionID, Name: "notes"}), http.StatusConflict)
	expectStatus(t, e.doJSON(http.MethodPost, "/section/", NewSectionRequest{Parent: "unknown", Name: "x"}), http.StatusNotFound)
	expectStatus(t, e.doJSON(http.MethodPost, "/section/", NewSectionRequest{Parent: RootSectionID, Name: "../x"}), http.StatusBadRequest)

	rec = e.doJSON(http.MethodPut, "/section/"+section.ID+"/", RenameSectionRequest{Name: "journal"})
	expectStatus(t, rec, http.StatusOK)
	renamed := decode[Section](t, rec)
	if renamed.Name != "journal" || renamed.ID == section.ID {
		t.Fatalf("unexpected renamed section: %+v", renamed)
	}
	expectStatus(t, e.doJSON(http.MethodPut, "/section/"+section.ID+"/", RenameSectionRequest{Name: "x"}), http.StatusNotFound)

	expectStatus(t, e.do(http.MethodDelete, "/section/"+renamed.ID+"/", nil, ""), http.StatusOK)
	expectStatus(t, e.do(http.MethodDelete, "/section/"+renamed.ID+"/", nil, ""), http.StatusNotFound)
	if _, err := os.Stat(filepath.Join(e.project, "docs", "journal")); !os.IsNotExist(err) {
		t.Error("section folder still exists")
	}
}

func TestRestDocumentLifecycle(t *testing.T) {
	e := newRestTestEnv(t)

	rec := e.doJSON(http.MethodPost, "/document/", NewDocumentRequest{Parent: RootSectionID, Name: "todo"})
	expectStatus(t, rec, http.StatusOK)
	doc := decode[Document](t, rec)
	if doc.Name != "todo" || doc.SubUrl != "todo/" {
		t.Fatalf("unexpected document: %+v", doc)
	}
	expectStatus(t, e.doJSON(http.MethodPost, "/document/", NewDocumentRequest{Parent: RootSectionID, Name: "todo"}), http.StatusConflict)
	expectStatus(t, e.doJSON(http.MethodPost, "/document/", NewDocumentRequest{Parent: "unknown", Name: "x"}), http.StatusNotFound)

	rec = e.doJSON(http.MethodPut, "/document/"+doc.ID+"/", RenameDocumentRequest{Name: "done"})
	expectStatus(t, rec, http.StatusOK)
	renamed := decode[Document](t, rec)
	if renamed.Name != "done" {
		t.Fatalf("unexpected renamed document: %+v", renamed)
	}
	if _, err := os.Stat(filepath.Join(e.project, "docs", "done.md")); err != nil {
		t.Fatal(err)
	}

	expectStatus(t, e.do(http.MethodDelete, "/document/"+renamed.ID+"/", nil, ""), http.StatusOK)
	expectStatus(t, e.do(http.MethodDelete, "/document/"+renamed.ID+"/", nil, ""), http.StatusNotFound)
}

func TestRestResourceUploadAndRename(t *testing.T) {
	e := newRestTestEnv(t)
	binary := "\x00\xff\xfe binary \x89PNG"

	// multipart file part (binary safe)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "upload.bin")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write([]byte(binary))
	_ = mw.Close()
	rec := e.do(http.MethodPost, "/resource/"+RootSectionID+"/upload.bin/", &body, mw.FormDataContentType())
	expectStatus(t, rec, http.StatusOK)
	res := decode[Resource](t, rec)
	if b, _ := os.ReadFile(filepath.Join(e.project, "docs", "upload.bin")); string(b) != binary {
		t.Errorf("uploaded content = %q", b)
	}

	// plain form value (older clients)
	body.Reset()
	mw = multipart.NewWriter(&body)
	_ = mw.WriteField("file", "text content")
	_ = mw.Close()
	expectStatus(t, e.do(http.MethodPost, "/resource/"+RootSectionID+"/notes.txt/", &body, mw.FormDataContentType()), http.StatusOK)
	if b, _ := os.ReadFile(filepath.Join(e.project, "docs", "notes.txt")); string(b) != "text content" {
		t.Errorf("uploaded content = %q", b)
	}

	// missing field
	body.Reset()
	mw = multipart.NewWriter(&body)
	_ = mw.Close()
	expectStatus(t, e.do(http.MethodPost, "/resource/"+RootSectionID+"/empty.txt/", &body, mw.FormDataContentType()), http.StatusBadRequest)

	rec = e.doJSON(http.MethodPut, "/resource/"+res.ID+"/", RenameResourceRequest{Name: "renamed.bin"})
	expectStatus(t, rec, http.StatusOK)
	renamed := decode[Resource](t, rec)
	if renamed.Name != "renamed.bin" {
		t.Fatalf("unexpected resource: %+v", renamed)
	}
	expectStatus(t, e.do(http.MethodDelete, "/resource/"+renamed.ID+"/", nil, ""), http.StatusOK)
}

func TestRestLegacyBasicAuth(t *testing.T) {
	newTestWiki(t)
	configuration.CurrentConfig.Server.BasicAuth = configuration.AuthenticationConfiguration{User: "admin", Password: "secret"}
	e := newRestTestEnv(t)
	configuration.CurrentConfig.Server.BasicAuth = configuration.AuthenticationConfiguration{User: "admin", Password: "secret"}
	e.rs = NewRestService(e.tm, nil)

	// the liveness probe needs no credentials
	expectStatus(t, e.do(http.MethodGet, "/alive/", nil, ""), http.StatusOK)
	rec := e.do(http.MethodGet, "/section/", nil, "")
	expectStatus(t, rec, http.StatusUnauthorized)
	if !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "basic") && !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Basic") {
		t.Errorf("missing WWW-Authenticate header: %v", rec.Header())
	}

	e.user, e.pass = "admin", "wrong"
	expectStatus(t, e.do(http.MethodGet, "/section/", nil, ""), http.StatusUnauthorized)
	e.user, e.pass = "admin", "secret"
	expectStatus(t, e.do(http.MethodGet, "/section/", nil, ""), http.StatusOK)
}
