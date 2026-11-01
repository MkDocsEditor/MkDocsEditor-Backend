package backend

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	automerge "github.com/automerge/automerge-go"
)

func TestReferenceEditsKeepTheMarkup(t *testing.T) {
	root := &Section{Type: TypeSection, ID: RootSectionID, Documents: []*Document{}, Subsections: []*Section{}, Resources: []*Resource{}}
	guide := &Section{Type: TypeSection, ID: "g", Name: "guide", RelPath: "guide", Documents: []*Document{}, Subsections: []*Section{}, Resources: []*Resource{}}
	root.Subsections = append(root.Subsections, guide)
	root.Documents = append(root.Documents, &Document{RelPath: "index.md", Name: "index"})
	guide.Documents = append(guide.Documents, &Document{RelPath: "guide/setup.md", Name: "setup"})
	guide.Resources = append(guide.Resources, &Resource{RelPath: "guide/Bild 1.png", Name: "Bild 1.png"})
	lookup := newTreeLookup(root)
	op := &moveOp{oldRel: "guide", newRel: "Anleitungen/guide"}

	text := "# Start\n" +
		"Siehe [die Einrichtung](guide/setup.md#install \"Name\") und ![Bild](<guide/Bild 1.png>){ width=300 }.\n" +
		"<img alt=\"x\" src='guide/Bild%201.png'> [Mappe](guide/) [extern](https://example.org/guide/setup.md)\n" +
		"`[code](guide/setup.md)` [kaputt](guide/fehlt.md) [abs](/guide/setup.md)\n" +
		"[def]: ./guide/setup.md\n"
	edits := referenceEdits(text, "index.md", "index.md", lookup, op.mapPath)
	got := applyEdits(text, edits)
	want := "# Start\n" +
		"Siehe [die Einrichtung](Anleitungen/guide/setup.md#install \"Name\") und ![Bild](<Anleitungen/guide/Bild 1.png>){ width=300 }.\n" +
		"<img alt=\"x\" src='Anleitungen/guide/Bild%201.png'> [Mappe](Anleitungen/guide/) [extern](https://example.org/guide/setup.md)\n" +
		"`[code](guide/setup.md)` [kaputt](guide/fehlt.md) [abs](/Anleitungen/guide/setup.md)\n" +
		"[def]: ./Anleitungen/guide/setup.md\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	lines := lineEdits(text, edits)
	if len(lines) != 4 || lines[0].Line != 2 || lines[3].Line != 5 || lines[3].After != "[def]: ./Anleitungen/guide/setup.md" {
		t.Errorf("line edits: %+v", lines)
	}

	// references inside the moved section to the outside change, those within it do not
	inside := "[Start](../index.md) [Bild](Bild%201.png) [selbst](setup.md#a)\n"
	got = applyEdits(inside, referenceEdits(inside, "guide/setup.md", "Anleitungen/guide/setup.md", lookup, op.mapPath))
	if got != "[Start](../../index.md) [Bild](Bild%201.png) [selbst](setup.md#a)\n" {
		t.Errorf("inside: %s", got)
	}

	// the Automerge splices produce the same text, and only touch what differs
	doc := automerge.New()
	if err := doc.Path(contentKey).Set(automerge.NewText(text)); err != nil {
		t.Fatal(err)
	}
	if err := spliceEdits(doc.Path(contentKey).Text(), text, edits); err != nil {
		t.Fatal(err)
	}
	if s, _ := contentText(doc); s != want {
		t.Errorf("splices give\n%s", s)
	}
}

func TestConfigEdits(t *testing.T) {
	content := "site_name: Wiki # Kommentar\n" +
		"theme:\n  name: material\n  logo: guide/logo.png\n" +
		"nav:\n" +
		"  - Start: index.md\n" +
		"  - Anleitung:\n" +
		"    - 'Einrichtung': 'guide/setup.md'   # bleibt\n" +
		"    - \"Grüße\": \"guide/grüße.md\"\n" +
		"    - guide/plain.md\n" +
		"    - Extern: https://example.org/guide/setup.md\n" +
		"extra_javascript:\n  - guide/x.js\n  - path: guide/y.js\n    defer: true\n" +
		"markdown_extensions:\n  - pymdownx.emoji:\n      emoji_index: !!python/name:material.extensions.emoji.twemoji\n"
	op := &moveOp{oldRel: "guide", newRel: "Hand buch: neu"}
	got, edits := configEdits(content, "mkdocs.yml", op.mapPath)
	want := "site_name: Wiki # Kommentar\n" +
		"theme:\n  name: material\n  logo: \"Hand buch: neu/logo.png\"\n" +
		"nav:\n" +
		"  - Start: index.md\n" +
		"  - Anleitung:\n" +
		"    - 'Einrichtung': 'Hand buch: neu/setup.md'   # bleibt\n" +
		"    - \"Grüße\": \"Hand buch: neu/grüße.md\"\n" +
		"    - \"Hand buch: neu/plain.md\"\n" +
		"    - Extern: https://example.org/guide/setup.md\n" +
		"extra_javascript:\n  - \"Hand buch: neu/x.js\"\n  - path: \"Hand buch: neu/y.js\"\n    defer: true\n" +
		"markdown_extensions:\n  - pymdownx.emoji:\n      emoji_index: !!python/name:material.extensions.emoji.twemoji\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if len(edits) != 6 || edits[0].Line != 4 || edits[0].File != "mkdocs.yml" {
		t.Errorf("edits: %+v", edits)
	}

	op = &moveOp{oldRel: "guide/setup.md", newRel: "Anleitung/Einrichtung.md"}
	got, _ = configEdits(content, "mkdocs.yml", op.mapPath)
	if !strings.Contains(got, "    - 'Einrichtung': 'Anleitung/Einrichtung.md'   # bleibt\n") {
		t.Errorf("single file:\n%s", got)
	}
}

// moveRequest posts a move and decodes the answer
func (e *restTestEnv) move(req MoveRequest, wantStatus int) (*MoveResult, map[string]any) {
	e.t.Helper()
	rec := e.doJSON(http.MethodPost, "/move/", req)
	if rec.Code != wantStatus {
		e.t.Fatalf("move %+v: %d %s", req, rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		var errBody map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &errBody)
		return nil, errBody
	}
	var result MoveResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		e.t.Fatal(err)
	}
	return &result, nil
}

func (e *restTestEnv) read(rel string) string {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(e.tm.RootPath(), filepath.FromSlash(rel)))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(data)
}

func TestMoveSection(t *testing.T) {
	e := newRestTestEnv(t, withReferenceWiki(t))
	rec := e.doJSON(http.MethodPost, "/section/", NewItemRequest{Parent: RootSectionID, Name: "Handbuch"})
	expectStatus(t, rec, http.StatusOK)
	handbuch := decode[Section](t, rec)
	guide := findSectionByName(e.tm.Root(), "guide")
	oldSetupID := e.docID("guide/setup.md")
	configBefore, _ := os.ReadFile(filepath.Join(e.project, "mkdocs.yml"))

	// the preview changes nothing
	preview, _ := e.move(MoveRequest{Type: TypeSection, ID: guide.ID, Parent: handbuch.ID, DryRun: true}, http.StatusOK)
	if !preview.DryRun || preview.Item != nil || preview.OldPath != "guide" || preview.NewPath != "Handbuch/guide" || preview.NewID != e.docID("Handbuch/guide") {
		t.Errorf("preview: %+v", preview)
	}
	if len(preview.Moved) != 3 || preview.Moved[0].OldPath != "guide" || preview.Moved[1].NewPath != "Handbuch/guide/image.png" ||
		preview.Moved[2].OldID != oldSetupID || preview.Moved[2].NewID != e.docID("Handbuch/guide/setup.md") {
		t.Errorf("moved: %+v", preview.Moved)
	}
	if len(preview.Documents) != 2 {
		t.Fatalf("documents: %+v", preview.Documents)
	}
	setup, index := preview.Documents[0], preview.Documents[1]
	if setup.Path != "guide/setup.md" || setup.NewPath != "Handbuch/guide/setup.md" || len(setup.Edits) != 2 ||
		setup.Edits[0] != (LineEdit{Line: 3, Before: "Zurück zur [Startseite](../index.md).", After: "Zurück zur [Startseite](../../index.md)."}) ||
		setup.Edits[1].After != "<img src=\"../../img/Strand%20(2).png\" alt=\"Strand\">" {
		t.Errorf("setup: %+v", setup)
	}
	if index.Path != "index.md" || len(index.Edits) != 1 || index.Edits[0].After != "- [Einrichtung](Handbuch/guide/setup.md#install)" {
		t.Errorf("index: %+v", index)
	}
	if len(preview.Config) != 1 || preview.Config[0].After != "    - Einrichtung: Handbuch/guide/setup.md" {
		t.Errorf("config: %+v", preview.Config)
	}
	if _, err := os.Stat(filepath.Join(e.tm.RootPath(), "guide")); err != nil {
		t.Error("the preview moved the section")
	}
	if configAfter, _ := os.ReadFile(filepath.Join(e.project, "mkdocs.yml")); string(configAfter) != string(configBefore) {
		t.Error("the preview changed mkdocs.yml")
	}

	result, _ := e.move(MoveRequest{Type: TypeSection, ID: guide.ID, Parent: handbuch.ID}, http.StatusOK)
	if result.DryRun || result.Item == nil || len(result.Documents) != 2 || len(result.Config) != 1 {
		t.Fatalf("result: %+v", result)
	}
	if got := e.read("index.md"); !strings.Contains(got, "- [Einrichtung](Handbuch/guide/setup.md#install)\n") {
		t.Errorf("index.md: %s", got)
	}
	got := e.read("Handbuch/guide/setup.md")
	if !strings.Contains(got, "[Startseite](../../index.md)") || !strings.Contains(got, "![Bild](image.png)") ||
		!strings.Contains(got, "[raus](../../x.md)") {
		t.Errorf("setup.md: %s", got)
	}
	if cfg, _ := os.ReadFile(filepath.Join(e.project, "mkdocs.yml")); !strings.Contains(string(cfg), "    - Einrichtung: Handbuch/guide/setup.md\n") {
		t.Errorf("mkdocs.yml: %s", cfg)
	}
	if e.tm.GetDocument(e.docID("Handbuch/guide/setup.md")) == nil || e.tm.GetSection(guide.ID) != nil {
		t.Error("tree not updated")
	}
	// the index sees the new links: nothing is broken that was not broken before
	var broken BrokenReferences
	e.getJSON("/references/broken/", &broken)
	if len(broken.Broken) != 2 {
		t.Errorf("broken after the move: %+v", broken.Broken)
	}

	// old ids redirect
	rec = e.do(http.MethodGet, "/document/"+oldSetupID+"/", nil, "")
	expectStatus(t, rec, http.StatusNotFound)
	notFound := decode[ErrorResult](t, rec)
	if moved, _ := notFound.Details["movedTo"].(map[string]any); moved["path"] != "Handbuch/guide/setup.md" {
		t.Errorf("404 details: %+v", notFound.Details)
	}
	var movedResp MovedResponse
	e.getJSON("/moved/"+oldSetupID+"/", &movedResp)
	if movedResp.OldID != oldSetupID || movedResp.Type != TypeDocument || movedResp.Item.ID != e.docID("Handbuch/guide/setup.md") || movedResp.MovedAt.IsZero() {
		t.Errorf("moved: %+v", movedResp)
	}
	expectStatus(t, e.do(http.MethodGet, "/moved/"+e.docID("index.md")+"/", nil, ""), http.StatusNotFound)
	expectStatus(t, e.do(http.MethodGet, "/moved/unknown/", nil, ""), http.StatusNotFound)

	// a second move of the section: the old id follows the chain
	e.move(MoveRequest{Type: TypeSection, ID: result.NewID, Parent: RootSectionID, Name: "Anleitung"}, http.StatusOK)
	e.getJSON("/moved/"+oldSetupID+"/", &movedResp)
	if movedResp.Item.Path != "Anleitung/setup.md" {
		t.Errorf("after the second move: %+v", movedResp.Item)
	}
	if got := e.read("Anleitung/setup.md"); !strings.Contains(got, "[Startseite](../index.md)") {
		t.Errorf("setup.md after the second move: %s", got)
	}
}

func TestMoveValidation(t *testing.T) {
	e := newRestTestEnv(t, withReferenceWiki(t))
	root := e.tm.Root()
	guide := findSectionByName(root, "guide")
	rezepte := findSectionByName(root, "rezepte")
	sub := decode[Section](t, e.doJSON(http.MethodPost, "/section/", NewItemRequest{Parent: guide.ID, Name: "sub"}))
	logo := findResourceByName(root, "logo.png")

	reason := func(body map[string]any) any {
		details, _ := body["details"].(map[string]any)
		return details["reason"]
	}
	for _, tt := range []struct {
		name   string
		req    MoveRequest
		status int
		reason string
	}{
		{"root", MoveRequest{Type: TypeSection, ID: RootSectionID, Parent: guide.ID}, http.StatusBadRequest, MoveReasonRoot},
		{"into itself", MoveRequest{Type: TypeSection, ID: guide.ID, Parent: guide.ID}, http.StatusBadRequest, MoveReasonIntoItself},
		{"into a subsection", MoveRequest{Type: TypeSection, ID: guide.ID, Parent: sub.ID}, http.StatusBadRequest, MoveReasonIntoItself},
		{"no change", MoveRequest{Type: TypeDocument, ID: e.docID("index.md"), Parent: RootSectionID}, http.StatusBadRequest, MoveReasonNoChange},
		{"invalid name", MoveRequest{Type: TypeDocument, ID: e.docID("index.md"), Name: "../x"}, http.StatusBadRequest, MoveReasonInvalidName},
		{"hidden name", MoveRequest{Type: TypeResource, ID: logo.ID, Name: ".logo.png"}, http.StatusBadRequest, MoveReasonInvalidName},
		{"blacklisted", MoveRequest{Type: TypeSection, ID: sub.ID, Parent: RootSectionID, Name: "blacklisted"}, http.StatusBadRequest, MoveReasonBlacklisted},
		{"extra_css", MoveRequest{Type: TypeResource, ID: logo.ID, Parent: e.tm.GenerateID(filepath.Join(e.tm.RootPath(), "stylesheets")), Name: "extra.css"}, http.StatusBadRequest, MoveReasonBlacklisted},
		{"invalid type", MoveRequest{Type: "folder", ID: guide.ID}, http.StatusBadRequest, MoveReasonInvalidType},
		{"exists", MoveRequest{Type: TypeDocument, ID: e.docID("rezepte/index.md"), Parent: RootSectionID}, http.StatusConflict, MoveReasonExists},
		{"unknown item", MoveRequest{Type: TypeDocument, ID: "nope", Parent: RootSectionID}, http.StatusNotFound, ""},
		{"unknown parent", MoveRequest{Type: TypeDocument, ID: e.docID("index.md"), Parent: "nope"}, http.StatusNotFound, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, body := e.move(tt.req, tt.status)
			if tt.reason != "" && reason(body) != tt.reason {
				t.Errorf("reason %v, want %s", reason(body), tt.reason)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(rezepte.Path, "index.md")); err != nil {
		t.Error("a rejected move changed something")
	}

	// readers may not move, not even as a preview
	r := newRestTestEnv(t, withReferenceWiki(t), withTestUsers(t))
	r.user, r.pass = "kid", "pw-kid"
	expectStatus(t, r.doJSON(http.MethodPost, "/move/", MoveRequest{Type: TypeDocument, ID: r.docID("index.md"), Name: "x", DryRun: true}), http.StatusForbidden)
	r.user, r.pass = "anna", "pw-anna"
	expectStatus(t, r.doJSON(http.MethodPost, "/move/", MoveRequest{Type: TypeDocument, ID: r.docID("index.md"), Name: "x", DryRun: true}), http.StatusOK)
}

func TestRenameRewritesReferences(t *testing.T) {
	e := newRestTestEnv(t, withReferenceWiki(t))
	logo := findResourceByName(e.tm.Root(), "logo.png")
	rec := e.doJSON(http.MethodPut, "/resource/"+logo.ID+"/", RenameRequest{Name: "Logo groß.png"})
	expectStatus(t, rec, http.StatusOK)
	if r := decode[Resource](t, rec); r.Name != "Logo groß.png" || r.RelPath != "img/Logo groß.png" {
		t.Errorf("renamed: %+v", r)
	}
	if got := e.read("index.md"); !strings.Contains(got, "![Logo](img/Logo%20groß.png)") {
		t.Errorf("index.md: %s", got)
	}
	if got := e.read("rezepte/Käsekuchen.md"); !strings.Contains(got, "[bild]: ../img/Logo%20groß.png\n") {
		t.Errorf("Käsekuchen.md: %s", got)
	}

	// renaming a document to its own name answers the document, as before
	id := e.docID("rezepte/Käsekuchen.md")
	expectStatus(t, e.doJSON(http.MethodPut, "/document/"+id+"/", RenameRequest{Name: "Käsekuchen"}), http.StatusOK)

	// updateReferences: false only moves
	off := false
	e.move(MoveRequest{Type: TypeDocument, ID: id, Name: "Torte", UpdateReferences: &off}, http.StatusOK)
	if got := e.read("rezepte/index.md"); !strings.Contains(got, "[Käsekuchen](Käsekuchen.md)") {
		t.Errorf("rezepte/index.md: %s", got)
	}

	// audit log
	events, _, err := e.rs.authenticator.AuditLog(auth.AuditQuery{Type: auth.AuditItemRenamed})
	if err != nil || len(events) != 2 || events[1].Details["oldPath"] != "img/logo.png" || events[1].Details["documents"] != float64(2) {
		t.Errorf("audit: %v %+v", err, events)
	}
}

func TestBrokenReferenceMovedTo(t *testing.T) {
	e := newRestTestEnv(t, withReferenceWiki(t))
	e.move(MoveRequest{Type: TypeResource, ID: findResourceByName(e.tm.Root(), "logo.png").ID, Name: "Logo.png"}, http.StatusOK)
	// a link to the old path that arrives on disk (git pull) is reported with the item now
	if err := os.WriteFile(filepath.Join(e.tm.RootPath(), "reference", "api.md"), []byte("# API\n![alt](../img/logo.png)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.tm.CreateItemTree()
	var broken BrokenReferences
	e.getJSON("/references/broken/", &broken)
	var found *BrokenReference
	for _, b := range broken.Broken {
		if b.Source.Path == "reference/api.md" {
			found = b
		}
	}
	if found == nil || found.MovedTo == nil || found.MovedTo.Path != "img/Logo.png" || found.SuggestedDestination != "../img/Logo.png" {
		t.Fatalf("broken: %+v", found)
	}
}
