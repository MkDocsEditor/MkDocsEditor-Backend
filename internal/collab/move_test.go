package collab

import (
	"strings"
	"testing"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/references"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	automerge "github.com/automerge/automerge-go"
)

func TestReferenceEditsKeepTheMarkup(t *testing.T) {
	root := &wiki.Section{Type: wiki.TypeSection, ID: wiki.RootSectionID, Documents: []*wiki.Document{}, Subsections: []*wiki.Section{}, Resources: []*wiki.Resource{}}
	guide := &wiki.Section{Type: wiki.TypeSection, ID: "g", Name: "guide", RelPath: "guide", Documents: []*wiki.Document{}, Subsections: []*wiki.Section{}, Resources: []*wiki.Resource{}}
	root.Subsections = append(root.Subsections, guide)
	root.Documents = append(root.Documents, &wiki.Document{RelPath: "index.md", Name: "index"})
	guide.Documents = append(guide.Documents, &wiki.Document{RelPath: "guide/setup.md", Name: "setup"})
	guide.Resources = append(guide.Resources, &wiki.Resource{RelPath: "guide/Bild 1.png", Name: "Bild 1.png"})
	lookup := references.NewTreeLookup(root)
	op := &moveOp{oldRel: "guide", newRel: "Anleitungen/guide"}

	text := "# Start\n" +
		"Siehe [die Einrichtung](guide/setup.md#install \"Name\") und ![Bild](<guide/Bild 1.png>){ width=300 }.\n" +
		"<img alt=\"x\" src='guide/Bild%201.png'> [Mappe](guide/) [extern](https://example.org/guide/setup.md)\n" +
		"`[code](guide/setup.md)` [kaputt](guide/fehlt.md) [abs](/guide/setup.md)\n" +
		"[def]: ./guide/setup.md\n"
	edits := references.ReferenceEdits(text, "index.md", "index.md", lookup, op.mapPath)
	got := references.ApplyEdits(text, edits)
	want := "# Start\n" +
		"Siehe [die Einrichtung](Anleitungen/guide/setup.md#install \"Name\") und ![Bild](<Anleitungen/guide/Bild 1.png>){ width=300 }.\n" +
		"<img alt=\"x\" src='Anleitungen/guide/Bild%201.png'> [Mappe](Anleitungen/guide/) [extern](https://example.org/guide/setup.md)\n" +
		"`[code](guide/setup.md)` [kaputt](guide/fehlt.md) [abs](/Anleitungen/guide/setup.md)\n" +
		"[def]: ./Anleitungen/guide/setup.md\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	lines := references.LineEdits(text, edits)
	if len(lines) != 4 || lines[0].Line != 2 || lines[3].Line != 5 || lines[3].After != "[def]: ./Anleitungen/guide/setup.md" {
		t.Errorf("line edits: %+v", lines)
	}

	// references inside the moved section to the outside change, those within it do not
	inside := "[Start](../index.md) [Bild](Bild%201.png) [selbst](setup.md#a)\n"
	got = references.ApplyEdits(inside, references.ReferenceEdits(inside, "guide/setup.md", "Anleitungen/guide/setup.md", lookup, op.mapPath))
	if got != "[Start](../../index.md) [Bild](Bild%201.png) [selbst](setup.md#a)\n" {
		t.Errorf("inside: %s", got)
	}

	// the Automerge splices produce the same text, and only touch what differs
	doc := automerge.New()
	if err := doc.Path(ContentKey).Set(automerge.NewText(text)); err != nil {
		t.Fatal(err)
	}
	if err := references.SpliceEdits(doc.Path(ContentKey).Text(), text, edits); err != nil {
		t.Fatal(err)
	}
	if s, _ := ContentText(doc); s != want {
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
	got, edits := references.ConfigEdits(content, "mkdocs.yml", op.mapPath)
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
	got, _ = references.ConfigEdits(content, "mkdocs.yml", op.mapPath)
	if !strings.Contains(got, "    - 'Einrichtung': 'Anleitung/Einrichtung.md'   # bleibt\n") {
		t.Errorf("single file:\n%s", got)
	}
}
