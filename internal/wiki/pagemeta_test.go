package wiki

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func ptr(s string) *string { return &s }

func TestParsePageMeta(t *testing.T) {
	text := "---\ntitle: Setup guide\ndescription: How to start\nicon: material/rocket\nstatus: new\ntags:\n  - Setup\n  - 42\nsearch:\n  exclude: true\n---\n# Heading\n"
	meta, body := ParsePageMeta(text)
	want := PageMeta{Title: ptr("Setup guide"), Description: ptr("How to start"), Icon: ptr("material/rocket"), Status: ptr("new"),
		Tags: []string{"Setup", "42"}, SearchExclude: true}
	if !reflect.DeepEqual(meta, want) || body != "# Heading\n" {
		t.Errorf("meta %+v body %q", meta, body)
	}
	if meta, _ := ParsePageMeta("\uFEFF---\r\ntags: single\r\n...\r\ntext"); !reflect.DeepEqual(meta.Tags, []string{"single"}) {
		t.Errorf("BOM, CRLF and ...: %+v", meta)
	}
	for _, text := range []string{"no front matter\n", "---\nnot closed\n", "---\n: : invalid\n---\n", "text\n---\ntitle: x\n---\n"} {
		meta, body := ParsePageMeta(text)
		if meta.Title != nil || len(meta.Tags) != 0 || meta.Tags == nil || body != text {
			t.Errorf("%q: %+v %q", text, meta, body)
		}
	}
}

func TestPageTitle(t *testing.T) {
	for _, c := range []struct{ text, path, want string }{
		{"---\ntitle: From meta\n---\n# Heading\n", "a.md", "From meta"},
		{"intro\n\n# The heading #\n# Second\n", "a.md", "The heading"},
		{"```\n# in code\n```\nSetext title\n===\n", "a.md", "Setext title"},
		{"## Only level two\n", "pid-tuning_guide.md", "Pid tuning guide"},
		{"text\n", "WiFi Guest.md", "WiFi Guest"},
		{"", "index.md", "Home"},
		{"", "garden/index.md", "Garden"},
		{"", "Smart Home/README.md", "Smart Home"},
		{"---\ntitle: ''\n---\n#hashtag\n", "notes.md", "Notes"},
	} {
		meta, body := ParsePageMeta(c.text)
		if got := PageTitle(meta, body, c.path); got != c.want {
			t.Errorf("%q %s: %q, want %q", c.text, c.path, got, c.want)
		}
	}
}

// The tree carries the metadata and rereads a document when it changes
func TestTreeHasPageMeta(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "page.md"), []byte("# First\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tm := &TreeManager{rootPath: dir}
	tm.CreateItemTree()
	doc := func() *Document { return tm.Root().Documents[0] }
	if doc().Title != "First" || doc().Meta.Tags == nil {
		t.Fatalf("doc = %+v", doc())
	}
	if err := os.WriteFile(filepath.Join(dir, "page.md"), []byte("---\ntitle: Second\ntags: [a]\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Minute)
	_ = os.Chtimes(filepath.Join(dir, "page.md"), future, future)
	tm.CreateItemTree()
	if doc().Title != "Second" || strings.Join(doc().Meta.Tags, ",") != "a" {
		t.Errorf("after the change: %+v", doc())
	}
}
