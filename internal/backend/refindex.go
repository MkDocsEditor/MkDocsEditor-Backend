package backend

import (
	"html"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
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

// treeLookup finds the items of a tree snapshot by their path relative to the docs folder
type treeLookup struct {
	root      *Section
	documents map[string]*Document
	resources map[string]*Resource
	sections  map[string]*Section
	// documentList is sorted by path
	documentList []*Document
}

func newTreeLookup(root *Section) *treeLookup {
	l := &treeLookup{
		root:      root,
		documents: map[string]*Document{},
		resources: map[string]*Resource{},
		sections:  map[string]*Section{},
	}
	var walk func(s *Section)
	walk = func(s *Section) {
		l.sections[s.RelPath] = s
		for _, d := range *s.Documents {
			l.documents[d.RelPath] = d
			l.documentList = append(l.documentList, d)
		}
		for _, r := range *s.Resources {
			l.resources[r.RelPath] = r
		}
		for _, sub := range *s.Subsections {
			walk(sub)
		}
	}
	if root != nil {
		walk(root)
	}
	sort.Slice(l.documentList, func(i, j int) bool { return l.documentList[i].RelPath < l.documentList[j].RelPath })
	return l
}

// resolve returns the item a resolved path points to, see docs/references.md section 3
func (l *treeLookup) resolve(p string) *ItemRef {
	if d := l.documents[p]; d != nil {
		return documentRef(d)
	}
	if r := l.resources[p]; r != nil {
		return resourceRef(r)
	}
	if s := l.sections[p]; s != nil {
		if index := sectionIndexDocument(s); index != nil {
			return documentRef(index)
		}
		return sectionRef(s)
	}
	return nil
}

// sectionIndexDocument returns the index document of a section (index.md, else README.md), or nil
func sectionIndexDocument(s *Section) *Document {
	var readme *Document
	for _, d := range *s.Documents {
		switch d.Name + markdownFileExtension {
		case "index.md":
			return d
		case "README.md":
			readme = d
		}
	}
	return readme
}

// item finds an item of any type by id
func (l *treeLookup) item(id string) *ItemRef {
	if id == RootSectionID {
		return sectionRef(l.root)
	}
	for _, d := range l.documents {
		if d.ID == id {
			return documentRef(d)
		}
	}
	for _, r := range l.resources {
		if r.ID == id {
			return resourceRef(r)
		}
	}
	for _, s := range l.sections {
		if s.ID == id {
			return sectionRef(s)
		}
	}
	return nil
}

func documentRef(d *Document) *ItemRef {
	return &ItemRef{Type: TypeDocument, ID: d.ID, Path: d.RelPath, Name: d.Name}
}

func resourceRef(r *Resource) *ItemRef {
	return &ItemRef{Type: TypeResource, ID: r.ID, Path: r.RelPath, Name: r.Name}
}

func sectionRef(s *Section) *ItemRef {
	return &ItemRef{Type: TypeSection, ID: s.ID, Path: s.RelPath, Name: s.Name}
}

// resolvedRef is a parsed reference of a document, resolved against the tree
type resolvedRef struct {
	parsedRef
	Source *Document
	Dest   destination
	// Target is the item, nil for broken references
	Target *ItemRef
	Reason string
}

// resolveReferences parses and resolves the references of a document text
func resolveReferences(l *treeLookup, source *Document, refs []parsedRef) []resolvedRef {
	out := make([]resolvedRef, 0, len(refs))
	for _, r := range refs {
		raw := r.Raw
		if r.HTML {
			raw = html.UnescapeString(raw)
		}
		d, ok := parseDestination(source.RelPath, raw)
		if !ok {
			continue
		}
		rr := resolvedRef{parsedRef: r, Source: source, Dest: d}
		switch {
		case d.outside:
			rr.Reason = BrokenOutsideDocs
		default:
			rr.Target = l.resolve(d.path)
			if rr.Target == nil {
				rr.Reason = BrokenNotFound
			}
		}
		out = append(out, rr)
	}
	return out
}

// ReferenceIndex caches the parsed references of every document, see docs/references.md
// section 4. It is only a cache for the read endpoints: moves parse the current texts.
type ReferenceIndex struct {
	tm   *TreeManager
	sync *SyncServer

	mu    sync.Mutex
	cache map[string]*indexedDocument
}

type indexedDocument struct {
	key  string
	refs []parsedRef
}

// NewReferenceIndex creates the index. sync may be nil.
func NewReferenceIndex(tm *TreeManager, syncServer *SyncServer) *ReferenceIndex {
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
	lookup *treeLookup
	// refs by source document path, in text order
	refs map[string][]resolvedRef
}

// Snapshot returns the current references of all documents, resolved against the current tree
func (ri *ReferenceIndex) Snapshot() *indexSnapshot {
	lookup := newTreeLookup(ri.tm.Root())
	snap := &indexSnapshot{lookup: lookup, refs: make(map[string][]resolvedRef, len(lookup.documentList))}

	ri.mu.Lock()
	defer ri.mu.Unlock()
	seen := make(map[string]bool, len(lookup.documentList))
	for _, d := range lookup.documentList {
		seen[d.RelPath] = true
		refs, ok := ri.documentRefsLocked(d)
		if !ok {
			continue
		}
		snap.refs[d.RelPath] = resolveReferences(lookup, d, refs)
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
func (ri *ReferenceIndex) documentRefsLocked(d *Document) ([]parsedRef, bool) {
	var text string
	var key string
	live := false
	if ri.sync != nil {
		text, live = ri.sync.CurrentText(d)
	}
	if live {
		key = "live:" + strconv.FormatUint(xxhash.ChecksumString64(text), 16)
	} else {
		key = "disk:" + strconv.FormatInt(d.Filesize, 10) + ":" + strconv.FormatInt(d.ModTime.UnixNano(), 10)
	}
	if cached := ri.cache[d.RelPath]; cached != nil && cached.key == key {
		return cached.refs, true
	}
	if !live {
		data, err := os.ReadFile(d.Path)
		if err != nil {
			if !os.IsNotExist(err) {
				log.Printf("Unable to index %s: %v", d.RelPath, err)
			}
			return nil, false
		}
		text = string(data)
	}
	refs := parseReferences(text)
	ri.cache[d.RelPath] = &indexedDocument{key: key, refs: refs}
	return refs, true
}

// all returns every resolved reference, sorted by source path, line and column
func (s *indexSnapshot) all() []resolvedRef {
	var out []resolvedRef
	for _, d := range s.lookup.documentList {
		out = append(out, s.refs[d.RelPath]...)
	}
	return out
}

// backlinks returns the references whose target matches
func (s *indexSnapshot) backlinks(match func(target *ItemRef) bool) []resolvedRef {
	var out []resolvedRef
	for _, r := range s.all() {
		if r.Target != nil && match(r.Target) {
			out = append(out, r)
		}
	}
	return out
}

// usedByConfig returns the string values of mkdocs.yml, which may name resources
func usedByConfig() map[string]bool {
	used := map[string]bool{}
	data, err := os.ReadFile(configuration.CurrentConfig.MkDocs.ConfigFile)
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

// unusedResources returns the resources no reference and no value in mkdocs.yml points to
func (s *indexSnapshot) unusedResources() []*Resource {
	used := usedByConfig()
	for _, r := range s.all() {
		if r.Target != nil && r.Target.Type == TypeResource {
			used[r.Target.Path] = true
		}
	}
	var out []*Resource
	for p, r := range s.lookup.resources {
		if !used[p] {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RelPath < out[j].RelPath })
	return out
}
