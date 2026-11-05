package api

import (
	"net/http"
	"path"
	"strings"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/references"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	"github.com/labstack/echo/v4"
)

// Statuses of references
const (
	RefStatusOK     = "ok"
	RefStatusBroken = "broken"
)

type (
	// Reference is one reference of a document, see docs/references.md section 4
	Reference struct {
		Kind        string              `json:"kind"`
		Line        int                 `json:"line"`
		Column      int                 `json:"column"`
		Destination string              `json:"destination"`
		Anchor      string              `json:"anchor"`
		Status      string              `json:"status"`
		Target      *references.ItemRef `json:"target"`
		Reason      string              `json:"reason,omitempty"`
		Path        string              `json:"path,omitempty"`
		// MovedTo and SuggestedDestination are set for broken references to old paths of moved items
		MovedTo              *references.ItemRef `json:"movedTo,omitempty"`
		SuggestedDestination string              `json:"suggestedDestination,omitempty"`
	}

	// DocumentReferences is the answer of GET /document/{id}/references/
	DocumentReferences struct {
		Document   *references.ItemRef `json:"document"`
		References []*Reference        `json:"references"`
	}

	// Backlink is a reference to an item
	Backlink struct {
		Source      *references.ItemRef `json:"source"`
		Kind        string              `json:"kind"`
		Line        int                 `json:"line"`
		Column      int                 `json:"column"`
		Destination string              `json:"destination"`
		Anchor      string              `json:"anchor"`
		Target      *references.ItemRef `json:"target"`
		Internal    bool                `json:"internal"`
	}

	// Backlinks is the answer of the backlinks endpoints
	Backlinks struct {
		Item      *references.ItemRef `json:"item"`
		Backlinks []*Backlink         `json:"backlinks"`
	}

	// BrokenReference is a broken reference with its document
	BrokenReference struct {
		Source *references.ItemRef `json:"source"`
		*Reference
	}

	// BrokenReferences is the answer of GET /references/broken/
	BrokenReferences struct {
		Broken []*BrokenReference `json:"broken"`
	}

	// UnusedResources is the answer of GET /references/unused-resources/
	UnusedResources struct {
		Resources []*wiki.Resource `json:"resources"`
	}

	// LinkResult is the answer of GET /link/
	LinkResult struct {
		Destination string              `json:"destination"`
		Markdown    string              `json:"markdown"`
		Target      *references.ItemRef `json:"target"`
	}
)

func (rs *RestService) toReference(r references.ResolvedRef) *Reference {
	ref := &Reference{
		Kind:        r.Kind,
		Line:        r.Line,
		Column:      r.Column,
		Destination: r.Raw,
		Anchor:      r.Dest.Anchor,
		Status:      RefStatusOK,
		Target:      r.Target,
	}
	if r.Target == nil {
		ref.Status = RefStatusBroken
		ref.Reason = r.Reason
		ref.Path = r.Dest.Path
		if moved, ok := rs.sync.MovedTarget(r.Source.RelPath, r.Dest); ok {
			ref.MovedTo, ref.SuggestedDestination = moved.Item, moved.Destination
		}
	}
	return ref
}

// GET /document/:id/references/
func (rs *RestService) getDocumentReferences(c echo.Context) error {
	id := c.Param(urlParamID)
	d := rs.treeManager.GetDocument(id)
	if d == nil {
		return rs.ReturnNotFound(c, id)
	}
	snap := rs.refs.Snapshot()
	result := &DocumentReferences{Document: references.DocumentRef(d), References: []*Reference{}}
	for _, r := range snap.Refs[d.RelPath] {
		result.References = append(result.References, rs.toReference(r))
	}
	return c.JSONPretty(http.StatusOK, result, jsonIndent)
}

// GET /document/:id/backlinks/
func (rs *RestService) getDocumentBacklinks(c echo.Context) error {
	id := c.Param(urlParamID)
	d := rs.treeManager.GetDocument(id)
	if d == nil {
		return rs.ReturnNotFound(c, id)
	}
	return rs.backlinks(c, references.DocumentRef(d), "", func(t *references.ItemRef) bool { return t.Type == wiki.TypeDocument && t.ID == d.ID })
}

// GET /resource/:id/backlinks/
func (rs *RestService) getResourceBacklinks(c echo.Context) error {
	id := c.Param(urlParamID)
	r := rs.treeManager.GetResource(id)
	if r == nil {
		return rs.ReturnNotFound(c, id)
	}
	return rs.backlinks(c, references.ResourceRef(r), "", func(t *references.ItemRef) bool { return t.Type == wiki.TypeResource && t.ID == r.ID })
}

// GET /section/:id/backlinks/: references to the section and everything inside it
func (rs *RestService) getSectionBacklinks(c echo.Context) error {
	id := c.Param(urlParamID)
	s := rs.treeManager.GetSection(id)
	if s == nil {
		return rs.ReturnNotFound(c, id)
	}
	return rs.backlinks(c, references.SectionRef(s), s.RelPath, func(t *references.ItemRef) bool { return wiki.IsInside(t.Path, s.RelPath) })
}

// backlinks answers the references whose target matches. Sources inside section (a path,
// "" for none) are marked as internal.
func (rs *RestService) backlinks(c echo.Context, item *references.ItemRef, section string, match func(*references.ItemRef) bool) error {
	snap := rs.refs.Snapshot()
	result := &Backlinks{Item: item, Backlinks: []*Backlink{}}
	for _, r := range snap.Backlinks(match) {
		result.Backlinks = append(result.Backlinks, &Backlink{
			Source:      references.DocumentRef(r.Source),
			Kind:        r.Kind,
			Line:        r.Line,
			Column:      r.Column,
			Destination: r.Raw,
			Anchor:      r.Dest.Anchor,
			Target:      r.Target,
			Internal:    item.Type == wiki.TypeSection && wiki.IsInside(r.Source.RelPath, section),
		})
	}
	return c.JSONPretty(http.StatusOK, result, jsonIndent)
}

// GET /references/broken/
func (rs *RestService) getBrokenReferences(c echo.Context) error {
	snap := rs.refs.Snapshot()
	result := &BrokenReferences{Broken: []*BrokenReference{}}
	for _, r := range snap.All() {
		if r.Target == nil {
			result.Broken = append(result.Broken, &BrokenReference{Source: references.DocumentRef(r.Source), Reference: rs.toReference(r)})
		}
	}
	return c.JSONPretty(http.StatusOK, result, jsonIndent)
}

// GET /references/unused-resources/
func (rs *RestService) getUnusedResources(c echo.Context) error {
	snap := rs.refs.Snapshot()
	result := &UnusedResources{Resources: snap.UnusedResources()}
	if result.Resources == nil {
		result.Resources = []*wiki.Resource{}
	}
	return c.JSONPretty(http.StatusOK, result, jsonIndent)
}

// GET /link/?from=&to=&anchor=
func (rs *RestService) getLink(c echo.Context) error {
	fromID, toID := c.QueryParam("from"), c.QueryParam("to")
	if fromID == "" || toID == "" {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "from and to are required", map[string]any{"field": "from"})
	}
	lookup := references.NewTreeLookup(rs.treeManager.Root())
	from := rs.treeManager.GetDocument(fromID)
	if from == nil {
		return rs.ReturnNotFound(c, fromID)
	}
	target := lookup.Item(toID)
	if target == nil {
		return rs.ReturnNotFound(c, toID)
	}
	lt := references.LinkTarget{Type: target.Type, Path: target.Path}
	text := target.Name
	if target.Type == wiki.TypeSection {
		if index := wiki.SectionIndexDocument(lookup.Sections[target.Path]); index != nil {
			lt.Index = index.RelPath
		}
	}
	dest := references.LinkPath(from.RelPath, lt, c.QueryParam("anchor"))
	markdown := "[" + escapeLinkText(text) + "](" + dest + ")"
	if r := lookup.Resources[target.Path]; r != nil && r.Kind == wiki.ResourceKindImage {
		markdown = "![" + escapeLinkText(strings.TrimSuffix(r.Name, path.Ext(r.Name))) + "](" + dest + ")"
	}
	return c.JSONPretty(http.StatusOK, &LinkResult{Destination: dest, Markdown: markdown, Target: target}, jsonIndent)
}

// escapeLinkText escapes the characters that would end the text of a markdown link
func escapeLinkText(s string) string {
	return strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`).Replace(s)
}
