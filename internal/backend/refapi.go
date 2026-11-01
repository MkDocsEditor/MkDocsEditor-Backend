package backend

import (
	"net/http"
	"path"
	"strings"

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
		Kind        string   `json:"kind"`
		Line        int      `json:"line"`
		Column      int      `json:"column"`
		Destination string   `json:"destination"`
		Anchor      string   `json:"anchor"`
		Status      string   `json:"status"`
		Target      *ItemRef `json:"target"`
		Reason      string   `json:"reason,omitempty"`
		Path        string   `json:"path,omitempty"`
		// MovedTo and SuggestedDestination are set for broken references to old paths of moved items
		MovedTo              *ItemRef `json:"movedTo,omitempty"`
		SuggestedDestination string   `json:"suggestedDestination,omitempty"`
	}

	// DocumentReferences is the answer of GET /document/{id}/references/
	DocumentReferences struct {
		Document   *ItemRef     `json:"document"`
		References []*Reference `json:"references"`
	}

	// Backlink is a reference to an item
	Backlink struct {
		Source      *ItemRef `json:"source"`
		Kind        string   `json:"kind"`
		Line        int      `json:"line"`
		Column      int      `json:"column"`
		Destination string   `json:"destination"`
		Anchor      string   `json:"anchor"`
		Target      *ItemRef `json:"target"`
		Internal    bool     `json:"internal"`
	}

	// Backlinks is the answer of the backlinks endpoints
	Backlinks struct {
		Item      *ItemRef    `json:"item"`
		Backlinks []*Backlink `json:"backlinks"`
	}

	// BrokenReference is a broken reference with its document
	BrokenReference struct {
		Source *ItemRef `json:"source"`
		*Reference
	}

	// BrokenReferences is the answer of GET /references/broken/
	BrokenReferences struct {
		Broken []*BrokenReference `json:"broken"`
	}

	// UnusedResources is the answer of GET /references/unused-resources/
	UnusedResources struct {
		Resources []*Resource `json:"resources"`
	}

	// LinkResult is the answer of GET /link/
	LinkResult struct {
		Destination string   `json:"destination"`
		Markdown    string   `json:"markdown"`
		Target      *ItemRef `json:"target"`
	}
)

func (rs *RestService) toReference(r resolvedRef) *Reference {
	ref := &Reference{
		Kind:        r.Kind,
		Line:        r.Line,
		Column:      r.Column,
		Destination: r.Raw,
		Anchor:      r.Dest.anchor,
		Status:      RefStatusOK,
		Target:      r.Target,
	}
	if r.Target == nil {
		ref.Status = RefStatusBroken
		ref.Reason = r.Reason
		ref.Path = r.Dest.path
		if rs.sync != nil {
			if moved, ok := rs.sync.movedTarget(r.Source.RelPath, r.Dest); ok {
				ref.MovedTo, ref.SuggestedDestination = moved.item, moved.destination
			}
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
	result := &DocumentReferences{Document: documentRef(d), References: []*Reference{}}
	for _, r := range snap.refs[d.RelPath] {
		result.References = append(result.References, rs.toReference(r))
	}
	return c.JSONPretty(http.StatusOK, result, indentationChar)
}

// GET /document/:id/backlinks/
func (rs *RestService) getDocumentBacklinks(c echo.Context) error {
	id := c.Param(urlParamID)
	d := rs.treeManager.GetDocument(id)
	if d == nil {
		return rs.ReturnNotFound(c, id)
	}
	return rs.backlinks(c, documentRef(d), "", func(t *ItemRef) bool { return t.Type == TypeDocument && t.ID == d.ID })
}

// GET /resource/:id/backlinks/
func (rs *RestService) getResourceBacklinks(c echo.Context) error {
	id := c.Param(urlParamID)
	r := rs.treeManager.GetResource(id)
	if r == nil {
		return rs.ReturnNotFound(c, id)
	}
	return rs.backlinks(c, resourceRef(r), "", func(t *ItemRef) bool { return t.Type == TypeResource && t.ID == r.ID })
}

// GET /section/:id/backlinks/: references to the section and everything inside it
func (rs *RestService) getSectionBacklinks(c echo.Context) error {
	id := c.Param(urlParamID)
	s := rs.treeManager.GetSection(id)
	if s == nil {
		return rs.ReturnNotFound(c, id)
	}
	return rs.backlinks(c, sectionRef(s), s.RelPath, func(t *ItemRef) bool { return isInside(t.Path, s.RelPath) })
}

// isInside reports whether p is the path dir or below it ("" is the docs folder)
func isInside(p string, dir string) bool {
	if dir == "" {
		return true
	}
	_, ok := movePath(p, dir, dir)
	return ok
}

// backlinks answers the references whose target matches. Sources inside section (a path,
// "" for none) are marked as internal.
func (rs *RestService) backlinks(c echo.Context, item *ItemRef, section string, match func(*ItemRef) bool) error {
	snap := rs.refs.Snapshot()
	result := &Backlinks{Item: item, Backlinks: []*Backlink{}}
	for _, r := range snap.backlinks(match) {
		result.Backlinks = append(result.Backlinks, &Backlink{
			Source:      documentRef(r.Source),
			Kind:        r.Kind,
			Line:        r.Line,
			Column:      r.Column,
			Destination: r.Raw,
			Anchor:      r.Dest.anchor,
			Target:      r.Target,
			Internal:    item.Type == TypeSection && isInside(r.Source.RelPath, section),
		})
	}
	return c.JSONPretty(http.StatusOK, result, indentationChar)
}

// GET /references/broken/
func (rs *RestService) getBrokenReferences(c echo.Context) error {
	snap := rs.refs.Snapshot()
	result := &BrokenReferences{Broken: []*BrokenReference{}}
	for _, r := range snap.all() {
		if r.Target == nil {
			result.Broken = append(result.Broken, &BrokenReference{Source: documentRef(r.Source), Reference: rs.toReference(r)})
		}
	}
	return c.JSONPretty(http.StatusOK, result, indentationChar)
}

// GET /references/unused-resources/
func (rs *RestService) getUnusedResources(c echo.Context) error {
	snap := rs.refs.Snapshot()
	result := &UnusedResources{Resources: snap.unusedResources()}
	if result.Resources == nil {
		result.Resources = []*Resource{}
	}
	return c.JSONPretty(http.StatusOK, result, indentationChar)
}

// GET /link/?from=&to=&anchor=
func (rs *RestService) getLink(c echo.Context) error {
	fromID, toID := c.QueryParam("from"), c.QueryParam("to")
	if fromID == "" || toID == "" {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "from and to are required", map[string]any{"field": "from"})
	}
	lookup := newTreeLookup(rs.treeManager.Root())
	from := rs.treeManager.GetDocument(fromID)
	if from == nil {
		return rs.ReturnNotFound(c, fromID)
	}
	target := lookup.item(toID)
	if target == nil {
		return rs.ReturnNotFound(c, toID)
	}
	lt := LinkTarget{Type: target.Type, Path: target.Path}
	text := target.Name
	if target.Type == TypeSection {
		if index := sectionIndexDocument(lookup.sections[target.Path]); index != nil {
			lt.Index = index.RelPath
		}
	}
	dest := LinkPath(from.RelPath, lt, c.QueryParam("anchor"))
	markdown := "[" + escapeLinkText(text) + "](" + dest + ")"
	if r := lookup.resources[target.Path]; r != nil && r.Kind == ResourceKindImage {
		markdown = "![" + escapeLinkText(strings.TrimSuffix(r.Name, path.Ext(r.Name))) + "](" + dest + ")"
	}
	return c.JSONPretty(http.StatusOK, &LinkResult{Destination: dest, Markdown: markdown, Target: target}, indentationChar)
}

// escapeLinkText escapes the characters that would end the text of a markdown link
func escapeLinkText(s string) string {
	return strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`).Replace(s)
}
