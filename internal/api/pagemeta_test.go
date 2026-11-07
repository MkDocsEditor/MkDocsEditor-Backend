package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki/wikitest"
	automerge "github.com/automerge/automerge-go"
)

func TestPageMetaInTreeFeedAndPins(t *testing.T) {
	e := newSyncEnv(t)
	setup := e.docID("guide/setup.md")
	e.writeFile("guide/setup.md", "---\ntitle: Getting started\ndescription: First steps\ntags: [setup, intro]\nsearch:\n  exclude: true\n---\n# Setup\n")
	e.tm.CreateItemTree()

	var tree wiki.Section
	if err := json.Unmarshal(e.do(http.MethodGet, "/section/", "carol").bytes(), &tree); err != nil {
		t.Fatal(err)
	}
	d := wikitest.FindDocument(&tree, "setup")
	if d == nil || d.Title != "Getting started" || d.Meta.Description == nil || *d.Meta.Description != "First steps" ||
		strings.Join(d.Meta.Tags, ",") != "setup,intro" || !d.Meta.SearchExclude || d.Meta.Icon != nil {
		t.Fatalf("document = %+v", d)
	}
	if home := wikitest.FindDocument(&tree, "index"); home == nil || home.Title != "Home" || home.Meta.Title != nil || home.Meta.Tags == nil {
		t.Errorf("index = %+v", home)
	}

	// a live edit reaches the tree
	a := newTestClient(t, e, "alice")
	a.connect(setup)
	a.edit(func(text *automerge.Text) error { return text.Splice(0, text.Len(), "# Renamed\n") })
	eventually(t, "the new title in the tree", func() bool {
		e.tm.CreateItemTree()
		d := wikitest.FindDocument(e.tm.Root(), "setup")
		return d != nil && d.Title == "Renamed"
	})
	a.disconnect()

	pins := e.pins(http.MethodPut, "/pins/personal/"+setup+"/", "carol", http.StatusOK)
	if pins.Personal[0].Title != "Renamed" {
		t.Errorf("pin = %+v", pins.Personal[0])
	}
	var feed *FeedResponse
	eventually(t, "the edit in the feed", func() bool {
		feed = e.feed("carol", "")
		return len(feed.Entries) == 1
	})
	if feed.Entries[0].Title == nil || *feed.Entries[0].Title != "Renamed" {
		t.Errorf("entry = %+v", feed.Entries[0])
	}
}

func TestWikiConfig(t *testing.T) {
	e := newSyncEnv(t)
	rec := e.do(http.MethodGet, "/wiki/config/", "carol")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var config wiki.WikiConfig
	if err := json.Unmarshal(rec.bytes(), &config); err != nil {
		t.Fatal(err)
	}
	if config.SiteName != "Test Wiki" || !config.UseDirectoryURLs || len(config.Plugins) != 1 || config.Plugins[0].Name != "search" ||
		strings.Join(config.ExtraCSS, ",") != "stylesheets/extra.css" {
		t.Errorf("config = %+v", config)
	}
}
