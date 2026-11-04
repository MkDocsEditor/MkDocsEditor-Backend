// Package references finds the links between documents and resources in markdown, keeps an index
// of them and plans the text edits that follow a move. See docs/references.md.
package references

import (
	"errors"
	"io/fs"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	"github.com/OneOfOne/xxhash"
	"gopkg.in/yaml.v3"
)

// Reasons of broken references
const (
	BrokenNotFound    = "not-found"
	BrokenOutsideDocs = "outside-docs"
)

// ItemRef is a short description of an item of the tree
type ItemRef struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Path string `json:"path"`
	Name string `json:"name"`
}

// TreeLookup finds the items of a tree snapshot by their path relative to the docs folder
type TreeLookup struct {
	root      *wiki.Section
	Documents map[string]*wiki.Document
	Resources map[string]*wiki.Resource
	Sections  map[string]*wiki.Section
	// DocumentList is sorted by path
	DocumentList []*wiki.Document
}

func NewTreeLookup(root *wiki.Section) *TreeLookup {
	l := &TreeLookup{
		root:      root,
		Documents: map[string]*wiki.Document{},
		Resources: map[string]*wiki.Resource{},
		Sections:  map[string]*wiki.Section{},
	}
	for s := range root.Walk() {
		l.Sections[s.RelPath] = s
		for _, d := range s.Documents {
			l.Documents[d.RelPath] = d
			l.DocumentList = append(l.DocumentList, d)
		}
		for _, r := range s.Resources {
			l.Resources[r.RelPath] = r
		}
	}
	sort.Slice(l.DocumentList, func(i, j int) bool { return l.DocumentList[i].RelPath < l.DocumentList[j].RelPath })
	return l
}

// Resolve returns the item a resolved path points to, see docs/references.md section 3
func (l *TreeLookup) Resolve(p string) *ItemRef {
	if d := l.Documents[p]; d != nil {
		return DocumentRef(d)
	}
	if r := l.Resources[p]; r != nil {
		return ResourceRef(r)
	}
	if s := l.Sections[p]; s != nil {
		if index := wiki.SectionIndexDocument(s); index != nil {
			return DocumentRef(index)
		}
		return SectionRef(s)
	}
	return nil
}

// Item finds an item of any type by id
func (l *TreeLookup) Item(id string) *ItemRef {
	if id == wiki.RootSectionID {
		return SectionRef(l.root)
	}
	for _, d := range l.Documents {
		if d.ID == id {
			return DocumentRef(d)
		}
	}
	for _, r := range l.Resources {
		if r.ID == id {
			return ResourceRef(r)
		}
	}
	for _, s := range l.Sections {
		if s.ID == id {
			return SectionRef(s)
		}
	}
	return nil
}

func DocumentRef(d *wiki.Document) *ItemRef {
	return &ItemRef{Type: wiki.TypeDocument, ID: d.ID, Path: d.RelPath, Name: d.Name}
}

func ResourceRef(r *wiki.Resource) *ItemRef {
	return &ItemRef{Type: wiki.TypeResource, ID: r.ID, Path: r.RelPath, Name: r.Name}
}

func SectionRef(s *wiki.Section) *ItemRef {
	return &ItemRef{Type: wiki.TypeSection, ID: s.ID, Path: s.RelPath, Name: s.Name}
}

// ResolvedRef is a parsed reference of a document, resolved against the tree
type ResolvedRef struct {
	parsedRef
	Source *wiki.Document
	Dest   Destination
	// Target is the item, nil for broken references
	Target *ItemRef
	Reason string
}

// resolveReferences parses and resolves the references of a document text
func resolveReferences(l *TreeLookup, source *wiki.Document, refs []parsedRef) []ResolvedRef {
	out := make([]ResolvedRef, 0, len(refs))
	for _, r := range refs {
		d, ok := ParseDestination(source.RelPath, r.DestinationText())
		if !ok {
			continue
		}
		rr := ResolvedRef{parsedRef: r, Source: source, Dest: d}
		switch {
		case d.Outside:
			rr.Reason = BrokenOutsideDocs
		default:
			rr.Target = l.Resolve(d.Path)
			if rr.Target == nil {
				rr.Reason = BrokenNotFound
			}
		}
		out = append(out, rr)
	}
	return out
}

// LiveTexts gives the current text of the documents that are being edited
type LiveTexts interface {
	// CurrentText returns the live text of a document. ok is false if it is not loaded.
	CurrentText(d *wiki.Document) (text string, ok bool)
}

// ReferenceIndex caches the parsed references of every document, see docs/references.md
// section 4. It is only a cache for the read endpoints: moves parse the current texts.
type ReferenceIndex struct {
	tm   *wiki.TreeManager
	sync LiveTexts

	mu    sync.Mutex
	cache map[string]*indexedDocument
}

type indexedDocument struct {
	key  string
	Refs []parsedRef
}

// NewReferenceIndex creates the index
func NewReferenceIndex(tm *wiki.TreeManager, syncServer LiveTexts) *ReferenceIndex {
	return &ReferenceIndex{tm: tm, sync: syncServer, cache: map[string]*indexedDocument{}}
}

// Warm builds the index in the background
func (ri *ReferenceIndex) Warm() {
	go func() {
		ri.Snapshot()
	}()
}

// indexSnapshot is the resolved references of all documents for one tree snapshot
type indexSnapshot struct {
	lookup *TreeLookup
	// configFile is the path of mkdocs.yml
	configFile string
	// Refs by source document path, in text order
	Refs map[string][]ResolvedRef
}

// Snapshot returns the current references of all documents, resolved against the current tree
func (ri *ReferenceIndex) Snapshot() *indexSnapshot {
	lookup := NewTreeLookup(ri.tm.Root())
	snap := &indexSnapshot{lookup: lookup, configFile: ri.tm.ConfigFile(), Refs: make(map[string][]ResolvedRef, len(lookup.DocumentList))}

	ri.mu.Lock()
	defer ri.mu.Unlock()
	seen := make(map[string]bool, len(lookup.DocumentList))
	for _, d := range lookup.DocumentList {
		seen[d.RelPath] = true
		refs, ok := ri.documentRefsLocked(d)
		if !ok {
			continue
		}
		snap.Refs[d.RelPath] = resolveReferences(lookup, d, refs)
	}
	for p := range ri.cache {
		if !seen[p] {
			delete(ri.cache, p)
		}
	}
	return snap
}

// documentRefsLocked returns the parsed references of a document, from the cache if its text
// did not change. Open documents are read from their live text. Caller holds mu.
func (ri *ReferenceIndex) documentRefsLocked(d *wiki.Document) ([]parsedRef, bool) {
	var key string
	text, live := ri.sync.CurrentText(d)
	if live {
		key = "live:" + strconv.FormatUint(xxhash.ChecksumString64(text), 16)
	} else {
		key = "disk:" + strconv.FormatInt(d.Filesize, 10) + ":" + strconv.FormatInt(d.ModTime.UnixNano(), 10)
	}
	if cached := ri.cache[d.RelPath]; cached != nil && cached.key == key {
		return cached.Refs, true
	}
	if !live {
		data, err := os.ReadFile(d.Path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				log.Printf("Unable to index %s: %v", d.RelPath, err)
			}
			return nil, false
		}
		text = string(data)
	}
	refs := ParseReferences(text)
	ri.cache[d.RelPath] = &indexedDocument{key: key, Refs: refs}
	return refs, true
}

// All returns every resolved reference, sorted by source path, line and column
func (s *indexSnapshot) All() []ResolvedRef {
	var out []ResolvedRef
	for _, d := range s.lookup.DocumentList {
		out = append(out, s.Refs[d.RelPath]...)
	}
	return out
}

// Backlinks returns the references whose target matches
func (s *indexSnapshot) Backlinks(match func(target *ItemRef) bool) []ResolvedRef {
	var out []ResolvedRef
	for _, r := range s.All() {
		if r.Target != nil && match(r.Target) {
			out = append(out, r)
		}
	}
	return out
}

// usedByConfig returns the string values of mkdocs.yml, which may name resources
func usedByConfig(configFile string) map[string]bool {
	used := map[string]bool{}
	data, err := os.ReadFile(configFile)
	if err != nil {
		return used
	}
	var root yaml.Node
	if err = yaml.Unmarshal(data, &root); err != nil {
		return used
	}
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.ScalarNode {
			used[strings.TrimPrefix(n.Value, "/")] = true
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(&root)
	return used
}

// UnusedResources returns the resources no reference and no value in mkdocs.yml points to
func (s *indexSnapshot) UnusedResources() []*wiki.Resource {
	used := usedByConfig(s.configFile)
	for _, r := range s.All() {
		if r.Target != nil && r.Target.Type == wiki.TypeResource {
			used[r.Target.Path] = true
		}
	}
	var out []*wiki.Resource
	for p, r := range s.lookup.Resources {
		if !used[p] {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RelPath < out[j].RelPath })
	return out
}
