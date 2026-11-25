package api

import (
	"net/url"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
)

// Links to the web client for agents, so they can link a page for a person. See docs/mcp.md.
// The base is server.auth.clientUrl; without it there are no links, never a guess from the
// request.

// webURL is the page of the web client for a document, section or resource id (kind
// "document", "section" or "resource", the routes of the web client), "" without a client URL
func (t *mcpTools) webURL(kind string, id string) string {
	base := t.rs.authenticator.ClientURL()
	if base == "" || id == "" {
		return ""
	}
	return base + "/" + kind + "/" + url.PathEscape(id)
}

// documentURL is the page of the web client for a document
func (t *mcpTools) documentURL(d *wiki.Document) string {
	if d == nil {
		return ""
	}
	return t.webURL("document", d.ID)
}

// documentURLOfPath is the page of the web client for the document at a path, "" if no page is
// there now (deleted or moved since)
func (t *mcpTools) documentURLOfPath(rel string) string {
	tm := t.rs.treeManager
	return t.documentURL(tm.GetDocument(tm.IDOfRelPath(rel)))
}

// urlPattern is the url of an item of a kind with the placeholder <id>, for lists
func urlPattern(base string, kind string) string {
	return base + "/" + kind + "/<id>"
}

// urlLine is "url: …\n" for the header of a text answer, "" without a URL
func urlLine(u string) string {
	if u == "" {
		return ""
	}
	return "url: " + u + "\n"
}
