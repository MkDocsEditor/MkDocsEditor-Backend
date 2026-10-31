package backend

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestTreeStructure(t *testing.T) {
	project := newTestWiki(t)
	tm := NewTreeManager()
	root := tm.Root()

	if root.ID != RootSectionID || root.Type != TypeSection {
		t.Fatalf("unexpected root: %+v", root)
	}
	if len(*root.Documents) != 1 || (*root.Documents)[0].Name != "index" {
		t.Fatalf("expected only index in root documents, got %+v", *root.Documents)
	}
	// stylesheets only contains an extra_css file, blacklisted is on the blacklist
	if findDocumentByName(root, "secret") != nil {
		t.Error("blacklisted document is in the tree")
	}
	if findResourceByName(root, "extra.css") != nil {
		t.Error("extra_css file is in the tree")
	}
	if findDocumentByName(root, ".hidden") != nil {
		t.Error("hidden document is in the tree")
	}

	guide := findSectionByName(root, "guide")
	if guide == nil {
		t.Fatal("section guide not found")
	}
	setup := findDocumentByName(guide, "setup")
	if setup == nil {
		t.Fatal("document setup not found")
	}
	wantPath := filepath.Join(project, "docs", "guide", "setup.md")
	if setup.Path != wantPath {
		t.Errorf("path = %s, want %s", setup.Path, wantPath)
	}
	if setup.SubUrl != "guide/setup/" {
		t.Errorf("url = %q, want guide/setup/", setup.SubUrl)
	}
	if setup.ID != tm.GenerateId(wantPath) {
		t.Error("document id is not derived from its path")
	}
	if tm.GetDocument(setup.ID) != setup {
		t.Error("GetDocument did not find the document")
	}
	if tm.GetSection(guide.ID) != guide {
		t.Error("GetSection did not find the section")
	}
	png := findResourceByName(guide, "image.png")
	if png == nil || tm.GetResource(png.ID) != png {
		t.Error("resource image.png not found")
	}
	if tm.GetDocument("does-not-exist") != nil || tm.GetSection("nope") != nil || tm.GetResource("nope") != nil {
		t.Error("getter returned an item for an unknown id")
	}

	content, err := tm.ReadDocumentContent(setup)
	if err != nil || content != "# Setup\n\nSome text.\n" {
		t.Errorf("content = %q, %v", content, err)
	}
}

func TestValidateName(t *testing.T) {
	valid := []string{"a", "My Page", "über", "file.png", "a..b"}
	invalid := []string{"", ".", "..", "../x", "a/b", "a\\b", ".hidden", "x\x00y"}
	for _, n := range valid {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", n, err)
		}
	}
	for _, n := range invalid {
		if err := ValidateName(n); !errors.Is(err, ErrInvalidName) {
			t.Errorf("ValidateName(%q) = %v, want ErrInvalidName", n, err)
		}
	}
}

func TestCreateRenameDelete(t *testing.T) {
	project := newTestWiki(t)
	docs := filepath.Join(project, "docs")
	tm := NewTreeManager()

	section, err := tm.CreateSection(RootSectionID, "new")
	if err != nil || section == nil {
		t.Fatalf("CreateSection: %v %v", section, err)
	}
	if _, err = os.Stat(filepath.Join(docs, "new")); err != nil {
		t.Fatal(err)
	}
	if _, err = tm.CreateSection(RootSectionID, "new"); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("duplicate section: %v", err)
	}
	if _, err = tm.CreateSection("unknown", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown parent: %v", err)
	}
	if _, err = tm.CreateSection(RootSectionID, "../escape"); !errors.Is(err, ErrInvalidName) {
		t.Errorf("path traversal: %v", err)
	}

	doc, err := tm.CreateDocument(section.ID, "page")
	if err != nil || doc == nil {
		t.Fatalf("CreateDocument: %v %v", doc, err)
	}
	if doc.Path != filepath.Join(docs, "new", "page.md") {
		t.Errorf("document path %s", doc.Path)
	}
	if _, err = tm.CreateDocument(section.ID, "page"); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("duplicate document: %v", err)
	}

	res, err := tm.CreateResource(section.ID, "data.bin", strings.NewReader("\x00\x01\x02"))
	if err != nil || res == nil {
		t.Fatalf("CreateResource: %v %v", res, err)
	}
	if b, _ := os.ReadFile(res.Path); string(b) != "\x00\x01\x02" {
		t.Errorf("resource content %q", b)
	}

	renamedDoc, err := tm.RenameDocument(doc, "renamed")
	if err != nil || renamedDoc == nil || renamedDoc.Name != "renamed" {
		t.Fatalf("RenameDocument: %+v %v", renamedDoc, err)
	}
	if tm.GetDocument(doc.ID) != nil {
		t.Error("old document id still resolves")
	}

	renamedRes, err := tm.RenameResource(res, "other.bin")
	if err != nil || renamedRes == nil || renamedRes.Name != "other.bin" {
		t.Fatalf("RenameResource: %+v %v", renamedRes, err)
	}

	renamedSection, err := tm.RenameSection(section, "moved")
	if err != nil || renamedSection == nil || renamedSection.Name != "moved" {
		t.Fatalf("RenameSection: %+v %v", renamedSection, err)
	}
	if findDocumentByName(renamedSection, "renamed") == nil {
		t.Error("document did not move with its section")
	}
	if _, err = tm.RenameSection(tm.Root(), "x"); err == nil {
		t.Error("root section was renamed")
	}

	ok, err := tm.DeleteItem(renamedSection.ID, TypeSection)
	if !ok || err != nil {
		t.Fatalf("DeleteItem: %v %v", ok, err)
	}
	if _, err = os.Stat(filepath.Join(docs, "moved")); !os.IsNotExist(err) {
		t.Error("section folder still exists")
	}
	if ok, _ = tm.DeleteItem("unknown", TypeDocument); ok {
		t.Error("deleting an unknown item reported success")
	}
	if _, err = tm.DeleteItem(RootSectionID, TypeSection); err == nil {
		t.Error("root section was deleted")
	}
}

func TestTreeChangeListener(t *testing.T) {
	newTestWiki(t)
	tm := NewTreeManager()
	var calls int
	tm.AddTreeChangeListener(func(oldTree, newTree *Section) {
		calls++
		if oldTree == newTree || newTree != tm.Root() {
			t.Error("listener got unexpected trees")
		}
	})
	if _, err := tm.CreateDocument(RootSectionID, "x"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("listener called %d times, want 1", calls)
	}
}

// Readers and writers run concurrently; run with -race.
func TestTreeConcurrentAccess(t *testing.T) {
	newTestWiki(t)
	tm := NewTreeManager()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				name := "doc" + string(rune('a'+i)) + string(rune('a'+j))
				d, err := tm.CreateDocument(RootSectionID, name)
				if err != nil {
					t.Error(err)
					return
				}
				if _, err = tm.DeleteItem(d.ID, TypeDocument); err != nil {
					t.Error(err)
				}
			}
		}(i)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				root := tm.Root()
				_ = findDocumentByName(root, "index")
				tm.CreateItemTree()
			}
		}()
	}
	wg.Wait()
}
