package backend

import (
	"encoding/json"
	"os"
	"testing"
)

// The vectors in docs/link-paths.json are shared with the clients' tests
func TestLinkPathVectors(t *testing.T) {
	data, err := os.ReadFile("../../docs/link-paths.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Vectors []struct {
			Name     string     `json:"name"`
			From     string     `json:"from"`
			Target   LinkTarget `json:"target"`
			Anchor   string     `json:"anchor"`
			Expected string     `json:"expected"`
		} `json:"vectors"`
	}
	if err = json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Vectors) < 30 {
		t.Fatalf("only %d vectors", len(file.Vectors))
	}
	for _, v := range file.Vectors {
		if got := LinkPath(v.From, v.Target, v.Anchor); got != v.Expected {
			t.Errorf("%s: LinkPath(%q, %+v, %q) = %q, want %q", v.Name, v.From, v.Target, v.Anchor, got, v.Expected)
		}
		// every link the algorithm builds resolves back to its target
		if v.Expected == "" || v.Expected[0] == '#' {
			continue
		}
		d, ok := parseDestination(v.From, v.Expected)
		if !ok {
			t.Errorf("%s: %q is not a reference", v.Name, v.Expected)
			continue
		}
		want := v.Target.Path
		if v.Target.Type == TypeSection && v.Target.Index != "" {
			want = v.Target.Index
		}
		if d.path != want || d.outside {
			t.Errorf("%s: %q resolves to %q (outside %v), want %q", v.Name, v.Expected, d.path, d.outside, want)
		}
	}
}

func TestParseDestination(t *testing.T) {
	tests := []struct {
		from, raw string
		ok        bool
		path      string
		anchor    string
		outside   bool
		folder    bool
	}{
		{"guide/setup.md", "install.md", true, "guide/install.md", "", false, false},
		{"guide/setup.md", "../index.md#top", true, "index.md", "top", false, false},
		{"guide/setup.md", "./install.md?x=1#a", true, "guide/install.md", "a", false, false},
		{"guide/setup.md", "../img/Strand%20(2).png", true, "img/Strand (2).png", "", false, false},
		{"guide/setup.md", "/img/logo.png", true, "img/logo.png", "", false, false},
		{"guide/setup.md", "../reference/", true, "reference", "", false, true},
		{"guide/setup.md", "../../etc/passwd", true, "etc/passwd", "", true, false},
		{"index.md", "/../x.md", true, "x.md", "", true, false},
		{"index.md", "Gem%C3%BCse.md", true, "Gemüse.md", "", false, false},
		{"index.md", "100%zz.md", true, "100%zz.md", "", false, false},
		{"index.md", "a+b.md", true, "a+b.md", "", false, false},
		{"index.md", "https://example.org/x.md", false, "", "", false, false},
		{"index.md", "mailto:a@example.org", false, "", "", false, false},
		{"index.md", "//cdn.example.org/a.png", false, "", "", false, false},
		{"index.md", "#heading", false, "", "", false, false},
		{"index.md", "", false, "", "", false, false},
		{"index.md", "./a:b.md", true, "a:b.md", "", false, false},
	}
	for _, tt := range tests {
		d, ok := parseDestination(tt.from, tt.raw)
		if ok != tt.ok {
			t.Errorf("%q from %q: ok = %v, want %v", tt.raw, tt.from, ok, tt.ok)
			continue
		}
		if !ok {
			continue
		}
		if d.path != tt.path || d.anchor != tt.anchor || d.outside != tt.outside || d.folder != tt.folder {
			t.Errorf("%q from %q: got path %q anchor %q outside %v folder %v", tt.raw, tt.from, d.path, d.anchor, d.outside, d.folder)
		}
	}
}

func TestRewriteDestination(t *testing.T) {
	tests := []struct {
		name               string
		from, raw          string
		newFrom, newTarget string
		want               string
	}{
		{"target moved", "index.md", "guide/setup.md#install", "index.md", "Handbuch/guide/setup.md", "Handbuch/guide/setup.md#install"},
		{"source moved", "guide/setup.md", "../index.md", "Handbuch/guide/setup.md", "index.md", "../../index.md"},
		{"query and anchor kept", "a.md", "b.md?raw=1#x", "a.md", "c/b.md", "c/b.md?raw=1#x"},
		{"dot slash kept", "a/x.md", "./y.md", "a/x.md", "a/z.md", "./z.md"},
		{"dot slash not doubled", "a/x.md", "./y.md", "a/x.md", "y.md", "../y.md"},
		{"absolute stays absolute", "a/x.md", "/img/logo.png", "b/c/x.md", "assets/logo.png", "/assets/logo.png"},
		{"folder stays folder", "a/x.md", "../ref/", "a/x.md", "docs/ref", "../docs/ref/"},
		{"folder without slash", "x.md", "ref", "x.md", "docs/ref", "docs/ref"},
		{"encoded non-ASCII kept", "x.md", "Gem%C3%BCse.md", "x.md", "Garten/Gemüse.md", "Garten/Gem%C3%BCse.md"},
		{"raw umlauts", "x.md", "Gemüse.md", "x.md", "Garten/Gemüse.md", "Garten/Gemüse.md"},
		{"raw spaces kept", "x.md", "my file.pdf", "x.md", "files/my file.pdf", "files/my file.pdf"},
		{"spaces encoded", "x.md", "my%20file.pdf", "x.md", "files/my file.pdf", "files/my%20file.pdf"},
		{"self link", "a/x.md", "x.md", "b/y.md", "b/y.md", "y.md"},
		{"spelling of kept segments", "a/x.md", "../img/Strand%20(2).png", "a/b/x.md", "img/Strand (2).png", "../../img/Strand%20(2).png"},
		{"new segments use the algorithm", "x.md", "img/Strand%20(2).png", "x.md", "Bilder (alt)/Strand (2).png", "Bilder%20%28alt%29/Strand%20(2).png"},
	}
	for _, tt := range tests {
		d, ok := parseDestination(tt.from, tt.raw)
		if !ok {
			t.Fatalf("%s: %q is not a reference", tt.name, tt.raw)
		}
		if got := rewriteDestination(d, tt.newFrom, tt.newTarget); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}
