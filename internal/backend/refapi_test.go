package backend

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
)

// referenceWikiFiles are added to the test wiki by withReferenceWiki
var referenceWikiFiles = map[string]string{
	"mkdocs.yml": "site_name: Test Wiki\nextra_css:\n  - stylesheets/extra.css\ntheme:\n  name: material\n  favicon: img/favicon.png\n" +
		"nav:\n  - Start: index.md\n  - Anleitung:\n    - Einrichtung: guide/setup.md\n  - Rezepte: rezepte/index.md\n",
	"docs/index.md": "# Start\n\n- [Einrichtung](guide/setup.md#install)\n- [Rezepte](rezepte/)\n" +
		"![Logo](img/logo.png)\n[kaputt](fehlt.md)\n",
	"docs/guide/setup.md": "# Setup\n\nZurück zur [Startseite](../index.md).\n![Bild](image.png)\n" +
		"<img src=\"../img/Strand%20(2).png\" alt=\"Strand\">\n[raus](../../x.md)\n",
	"docs/rezepte/index.md":       "# Rezepte\n\n[Käsekuchen](Käsekuchen.md)\n",
	"docs/rezepte/Käsekuchen.md":  "# Käsekuchen\n\n![Foto][foto]\n\n[foto]: ../img/logo.png\n",
	"docs/img/logo.png":           "png",
	"docs/img/Strand (2).png":     "png",
	"docs/img/unbenutzt.pdf":      "pdf",
	"docs/img/favicon.png":        "png",
	"docs/reference/api.md":       "# API\n",
	"docs/reference/beispiel.zip": "zip",
}

// withReferenceWiki writes referenceWikiFiles into the test wiki (a configure function of newRestTestEnv)
func withReferenceWiki(t *testing.T) func(c *configuration.Configuration) {
	return func(c *configuration.Configuration) {
		for name, content := range referenceWikiFiles {
			p := filepath.Join(c.MkDocs.ProjectPath, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// withTestUsers configures an editor (anna, pw-anna) and a reader (kid, pw-kid)
func withTestUsers(t *testing.T) func(c *configuration.Configuration) {
	return func(c *configuration.Configuration) {
		c.Server.Auth.Users = []configuration.UserConfiguration{
			{Username: "anna", PasswordHash: hashFor(t, "pw-anna")},
			{Username: "kid", PasswordHash: hashFor(t, "pw-kid"), Role: "reader"},
		}
	}
}

func (e *restTestEnv) getJSON(path string, into any) {
	e.t.Helper()
	rec := e.do(http.MethodGet, path, nil, "")
	if rec.Code != http.StatusOK {
		e.t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
		e.t.Fatal(err)
	}
}

func (e *restTestEnv) docID(rel string) string {
	e.t.Helper()
	return e.tm.GenerateId(filepath.Join(e.tm.RootPath(), filepath.FromSlash(rel)))
}

func TestDocumentReferences(t *testing.T) {
	e := newRestTestEnv(t, withReferenceWiki(t))
	var result DocumentReferences
	e.getJSON("/document/"+e.docID("index.md")+"/references/", &result)

	if result.Document.Path != "index.md" || len(result.References) != 4 {
		t.Fatalf("got %+v", result)
	}
	setup, rezepte, logo, broken := result.References[0], result.References[1], result.References[2], result.References[3]
	if setup.Kind != RefKindLink || setup.Line != 3 || setup.Column != 17 || setup.Destination != "guide/setup.md#install" ||
		setup.Anchor != "install" || setup.Status != RefStatusOK || setup.Target.Path != "guide/setup.md" {
		t.Errorf("setup: %+v %+v", setup, setup.Target)
	}
	// a folder link resolves to the section's index document
	if rezepte.Target == nil || rezepte.Target.Type != TypeDocument || rezepte.Target.Path != "rezepte/index.md" {
		t.Errorf("rezepte: %+v", rezepte)
	}
	if logo.Kind != RefKindImage || logo.Target.Type != TypeResource || logo.Target.Name != "logo.png" {
		t.Errorf("logo: %+v", logo)
	}
	if broken.Status != RefStatusBroken || broken.Target != nil || broken.Reason != BrokenNotFound || broken.Path != "fehlt.md" {
		t.Errorf("broken: %+v", broken)
	}

	// the index follows changes on disk
	if err := os.WriteFile(filepath.Join(e.tm.RootPath(), "index.md"), []byte("# Start\n[API](reference/api.md)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.tm.CreateItemTree()
	e.getJSON("/document/"+e.docID("index.md")+"/references/", &result)
	if len(result.References) != 1 || result.References[0].Target.Path != "reference/api.md" {
		t.Errorf("after the change: %+v", result.References)
	}

	if rec := e.do(http.MethodGet, "/document/nope/references/", nil, ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown document: %d", rec.Code)
	}
}

func TestBacklinks(t *testing.T) {
	e := newRestTestEnv(t, withReferenceWiki(t))
	logo := findResourceByName(e.tm.Root(), "logo.png")

	var result Backlinks
	e.getJSON("/resource/"+logo.ID+"/backlinks/", &result)
	if result.Item.Path != "img/logo.png" || len(result.Backlinks) != 2 {
		t.Fatalf("got %+v", result)
	}
	if b := result.Backlinks[0]; b.Source.Path != "index.md" || b.Kind != RefKindImage || b.Line != 5 || b.Internal {
		t.Errorf("first: %+v", b)
	}
	if b := result.Backlinks[1]; b.Source.Path != "rezepte/Käsekuchen.md" || b.Kind != RefKindDefinition || b.Destination != "../img/logo.png" {
		t.Errorf("second: %+v", b)
	}

	e.getJSON("/document/"+e.docID("index.md")+"/backlinks/", &result)
	if len(result.Backlinks) != 1 || result.Backlinks[0].Source.Path != "guide/setup.md" {
		t.Errorf("index backlinks: %+v", result.Backlinks)
	}

	// a section's backlinks include its index document and everything inside it
	rezepte := findSectionByName(e.tm.Root(), "rezepte")
	e.getJSON("/section/"+rezepte.ID+"/backlinks/", &result)
	if result.Item.Type != TypeSection || len(result.Backlinks) != 2 {
		t.Fatalf("section backlinks: %+v", result)
	}
	if b := result.Backlinks[0]; b.Source.Path != "index.md" || b.Target.Path != "rezepte/index.md" || b.Internal {
		t.Errorf("first: %+v", b)
	}
	if b := result.Backlinks[1]; b.Source.Path != "rezepte/index.md" || b.Target.Path != "rezepte/Käsekuchen.md" || !b.Internal {
		t.Errorf("second: %+v", b)
	}

	// readers may read backlinks
	e2 := newRestTestEnv(t, withReferenceWiki(t), withTestUsers(t))
	e2.user, e2.pass = "kid", "pw-kid"
	if rec := e2.do(http.MethodGet, "/resource/"+findResourceByName(e2.tm.Root(), "logo.png").ID+"/backlinks/", nil, ""); rec.Code != http.StatusOK {
		t.Errorf("reader: %d %s", rec.Code, rec.Body.String())
	}
}

func TestBrokenReferencesAndUnusedResources(t *testing.T) {
	e := newRestTestEnv(t, withReferenceWiki(t))
	var broken BrokenReferences
	e.getJSON("/references/broken/", &broken)
	if len(broken.Broken) != 2 {
		t.Fatalf("got %+v", broken.Broken)
	}
	if b := broken.Broken[0]; b.Source.Path != "guide/setup.md" || b.Reason != BrokenOutsideDocs || b.Destination != "../../x.md" {
		t.Errorf("first: %+v %+v", b.Source, b.Reference)
	}
	if b := broken.Broken[1]; b.Source.Path != "index.md" || b.Reason != BrokenNotFound || b.Line != 6 {
		t.Errorf("second: %+v %+v", b.Source, b.Reference)
	}

	var unused UnusedResources
	e.getJSON("/references/unused-resources/", &unused)
	var paths []string
	for _, r := range unused.Resources {
		paths = append(paths, r.RelPath)
	}
	// favicon.png is used by mkdocs.yml, Strand (2).png by an HTML tag
	if len(paths) != 2 || paths[0] != "img/unbenutzt.pdf" || paths[1] != "reference/beispiel.zip" {
		t.Errorf("unused: %v", paths)
	}
}

func TestLinkEndpoint(t *testing.T) {
	e := newRestTestEnv(t, withReferenceWiki(t))
	root := e.tm.Root()
	setup := e.docID("guide/setup.md")

	var result LinkResult
	e.getJSON("/link/?from="+setup+"&to="+findResourceByName(root, "Strand (2).png").ID, &result)
	if result.Destination != "../img/Strand%20%282%29.png" || result.Markdown != "![Strand (2)](../img/Strand%20%282%29.png)" {
		t.Errorf("image: %+v", result)
	}
	e.getJSON("/link/?from="+setup+"&to="+e.docID("rezepte/Käsekuchen.md")+"&anchor=zutaten", &result)
	if result.Destination != "../rezepte/Käsekuchen.md#zutaten" || result.Markdown != "[Käsekuchen](../rezepte/Käsekuchen.md#zutaten)" {
		t.Errorf("document: %+v", result)
	}
	e.getJSON("/link/?from="+setup+"&to="+findSectionByName(root, "rezepte").ID, &result)
	if result.Destination != "../rezepte/index.md" || result.Target.Type != TypeSection {
		t.Errorf("section with index: %+v", result)
	}
	e.getJSON("/link/?from="+setup+"&to="+findSectionByName(root, "reference").ID, &result)
	if result.Destination != "../reference/" {
		t.Errorf("section without index: %+v", result)
	}
	e.getJSON("/link/?from="+setup+"&to=root", &result)
	if result.Destination != "../index.md" {
		t.Errorf("root: %+v", result)
	}
	if rec := e.do(http.MethodGet, "/link/?from="+setup, nil, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("missing to: %d", rec.Code)
	}
	if rec := e.do(http.MethodGet, "/link/?from=nope&to=root", nil, ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown from: %d", rec.Code)
	}
}

func TestTreeResourceFields(t *testing.T) {
	e := newRestTestEnv(t, withReferenceWiki(t))
	var root map[string]any
	e.getJSON("/section/", &root)
	if root["path"] != "" || root["index"] != e.docID("index.md") {
		t.Errorf("root: path %v index %v", root["path"], root["index"])
	}
	tree := e.tm.Root()
	if s := findSectionByName(tree, "rezepte"); s.RelPath != "rezepte" || s.Index != e.docID("rezepte/index.md") {
		t.Errorf("rezepte: %+v", s)
	}
	if s := findSectionByName(tree, "reference"); s.Index != "" {
		t.Errorf("reference has index %q", s.Index)
	}
	if d := findDocumentByName(tree, "Käsekuchen"); d.RelPath != "rezepte/Käsekuchen.md" {
		t.Errorf("document path %q", d.RelPath)
	}
	for name, want := range map[string][2]string{
		"logo.png":       {"image/png", ResourceKindImage},
		"unbenutzt.pdf":  {"application/pdf", ResourceKindPDF},
		"beispiel.zip":   {"application/zip", ResourceKindArchive},
		"Strand (2).png": {"image/png", ResourceKindImage},
	} {
		r := findResourceByName(tree, name)
		if r.MimeType != want[0] || r.Kind != want[1] {
			t.Errorf("%s: %s %s", name, r.MimeType, r.Kind)
		}
	}
	if r := findResourceByName(tree, "Strand (2).png"); r.RelPath != "img/Strand (2).png" {
		t.Errorf("resource path %q", r.RelPath)
	}
	if mimeTypeOf("x.unknownext") != mimeTypeUnknown || resourceKind(mimeTypeUnknown) != ResourceKindOther {
		t.Error("unknown extension")
	}
}
