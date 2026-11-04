// Package wikitest holds test helpers for code that works on a wiki.
package wikitest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
)

// New creates a small MkDocs project in a temporary folder, points the global
// configuration at it and returns the project path.
//
//	mkdocs.yml        (extra_css: stylesheets/extra.css)
//	docs/index.md
//	docs/stylesheets/extra.css   (ignored through extra_css)
//	docs/.hidden.md              (ignored, hidden)
//	docs/blacklisted/            (ignored through the blacklist)
//	docs/guide/setup.md
//	docs/guide/image.png
func New(t *testing.T) string {
	t.Helper()
	project := t.TempDir()
	docs := filepath.Join(project, "docs")

	files := map[string]string{
		"mkdocs.yml":                 "site_name: Test Wiki\nextra_css:\n  - stylesheets/extra.css\n",
		"docs/index.md":              "# Home\n",
		"docs/stylesheets/extra.css": "body {}\n",
		"docs/.hidden.md":            "hidden\n",
		"docs/blacklisted/secret.md": "secret\n",
		"docs/guide/setup.md":        "# Setup\n\nSome text.\n",
		"docs/guide/image.png":       "\x89PNG\r\n\x1a\n\x00\x01binary",
	}
	for name, content := range files {
		path := filepath.Join(project, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	old := configuration.CurrentConfig
	t.Cleanup(func() { configuration.CurrentConfig = old })
	configuration.CurrentConfig = configuration.Configuration{
		MkDocs: configuration.MkDocsConfiguration{
			ProjectPath: project,
			ConfigFile:  filepath.Join(project, "mkdocs.yml"),
			DocsPath:    docs,
			Blacklist:   []string{"blacklisted"},
		},
	}
	return project
}

// FindDocument returns the document with the given name below a section, or nil
func FindDocument(s *wiki.Section, name string) *wiki.Document {
	for _, d := range s.Documents {
		if d.Name == name {
			return d
		}
	}
	for _, sub := range s.Subsections {
		if d := FindDocument(sub, name); d != nil {
			return d
		}
	}
	return nil
}

// FindSection returns the section with the given name below a section, or nil
func FindSection(s *wiki.Section, name string) *wiki.Section {
	if s.Name == name {
		return s
	}
	for _, sub := range s.Subsections {
		if r := FindSection(sub, name); r != nil {
			return r
		}
	}
	return nil
}

// FindResource returns the resource with the given name below a section, or nil
func FindResource(s *wiki.Section, name string) *wiki.Resource {
	for _, r := range s.Resources {
		if r.Name == name {
			return r
		}
	}
	for _, sub := range s.Subsections {
		if r := FindResource(sub, name); r != nil {
			return r
		}
	}
	return nil
}
