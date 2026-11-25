package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"path"
	"strings"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/collab"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/references"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP tools for the tree beyond pages: uploading, moving and deleting files and sections,
// deleting pages, links, the change feed and blame. They do what the REST endpoints of the web
// and Android apps do. See docs/mcp.md.

const (
	mcpFeedDefaultLimit = 30
	mcpFeedMaxLimit     = 200
)

type (
	mcpUploadInput struct {
		Path     string `json:"path" jsonschema:"file path relative to the wiki root (like guide/img/plot.png), or relative to document"`
		Document string `json:"document,omitempty" jsonschema:"page the path is relative to, like a link in it"`
		Text     string `json:"text,omitempty" jsonschema:"content of a text file; give text or base64"`
		Base64   string `json:"base64,omitempty" jsonschema:"content of a binary file, base64 encoded; give text or base64"`
		Replace  bool   `json:"replace,omitempty" jsonschema:"replace the file if it exists; without it an existing file is an error"`
	}

	mcpDeleteDocumentInput struct {
		Document string `json:"document" jsonschema:"page path relative to the wiki root, like guide/setup.md, or the page id"`
		Force    bool   `json:"force,omitempty" jsonschema:"delete even if people have the page open"`
	}

	mcpDeleteSectionInput struct {
		Section   string `json:"section" jsonschema:"section path relative to the wiki root, like guide/old"`
		Recursive bool   `json:"recursive,omitempty" jsonschema:"delete the section with all its pages, files and sections; required if it is not empty"`
		Force     bool   `json:"force,omitempty" jsonschema:"delete even if people have one of its pages open"`
	}

	mcpMoveResourceInput struct {
		Resource string `json:"resource" jsonschema:"file path relative to the wiki root, a link destination (with document), or the resource id"`
		Document string `json:"document,omitempty" jsonschema:"page the link is in: relative links are resolved from it"`
		Section  string `json:"section,omitempty" jsonschema:"target section path, empty to stay in the section; / for the top level"`
		Name     string `json:"name,omitempty" jsonschema:"new file name with its extension, empty to keep the name"`
	}

	mcpMoveSectionInput struct {
		Section string `json:"section" jsonschema:"section path relative to the wiki root, like guide/old"`
		Target  string `json:"target,omitempty" jsonschema:"new parent section path, empty to stay; / for the top level"`
		Name    string `json:"name,omitempty" jsonschema:"new folder name, empty to keep the name"`
	}

	mcpSectionPathInput struct {
		Path string `json:"path" jsonschema:"section path relative to the wiki root, like guide/new; missing parent sections are created"`
	}

	mcpCheckLinksInput struct {
		Section string `json:"section,omitempty" jsonschema:"only pages and files below this section path"`
	}

	mcpLinkInput struct {
		Document string `json:"document" jsonschema:"page the link goes into"`
		Target   string `json:"target" jsonschema:"page, file or section path relative to the wiki root, or an id"`
		Anchor   string `json:"anchor,omitempty" jsonschema:"heading anchor in the target page, without #"`
	}

	mcpFeedInput struct {
		Limit  int    `json:"limit,omitempty" jsonschema:"maximum number of entries, 1 to 200, default 30"`
		Before string `json:"before,omitempty" jsonschema:"cursor from the previous answer, for older entries"`
	}
)

// registerTreeTools adds the read tools of this file, and the write tools if write is true
func (t *mcpTools) registerTreeTools(server *mcp.Server, readOnly *mcp.ToolAnnotations, write bool) {
	mcp.AddTool(server, &mcp.Tool{Name: "check_links", Annotations: readOnly, Description: "Find broken links (to pages, " +
		"files or sections that do not exist) and files no page links to, optionally only below a section."}, t.checkLinks)
	mcp.AddTool(server, &mcp.Tool{Name: "link_to", Annotations: readOnly, Description: "Get the markdown link from a page " +
		"to another page, a file or a section: the relative destination, escaped, as MkDocs resolves it. Use it " +
		"instead of writing relative paths yourself."}, t.linkTo)
	mcp.AddTool(server, &mcp.Tool{Name: "recent_changes", Annotations: readOnly, Description: "The change feed of the " +
		"whole wiki, newest first: edits, created, moved and deleted pages and sections, with who and where."}, t.recentChanges)
	if t.rs.git != nil {
		mcp.AddTool(server, &mcp.Tool{Name: "blame", Annotations: readOnly, Description: "Who last changed each line of " +
			"a page, as blocks of lines with commit, time, author and agents; uncommitted lines as pending."}, t.blame)
	}
	if !write {
		return
	}
	notDestructive := false
	mcp.AddTool(server, &mcp.Tool{Name: "upload_resource", Description: "Create or replace a file that is no page, like " +
		"an image or a script: text as text, binary as base64 (keep it small, the request is limited to 4 MiB). " +
		"Missing sections are created. Pages (.md) are created with create_document. For larger files use " +
		"request_upload."}, t.uploadResource)
	mcp.AddTool(server, &mcp.Tool{Name: "request_upload", Annotations: &mcp.ToolAnnotations{DestructiveHint: &notDestructive},
		Description: "Get a one-time URL to upload a file that is too large for upload_resource. Send the " +
			"file from your shell with the curl command in the answer: its bytes do not pass through you. The URL works " +
			"once, for this path only, for 10 minutes."}, t.requestUpload)
	mcp.AddTool(server, &mcp.Tool{Name: "create_section", Annotations: &mcp.ToolAnnotations{DestructiveHint: &notDestructive},
		Description: "Create a section (a folder), and missing parent sections."}, t.createSection)
	mcp.AddTool(server, &mcp.Tool{Name: "move_resource", Description: "Move or rename a file that is no page. Links to it " +
		"are updated."}, t.moveResource)
	mcp.AddTool(server, &mcp.Tool{Name: "move_section", Description: "Move or rename a section with everything in it. " +
		"Links to its pages and files are updated."}, t.moveSection)
	mcp.AddTool(server, &mcp.Tool{Name: "delete_document", Description: "Delete a page. Links to it break: the answer " +
		"lists them. The page stays in the git history."}, t.deleteDocument)
	mcp.AddTool(server, &mcp.Tool{Name: "delete_resource", Description: "Delete a file that is no page. Links to it " +
		"break: the answer lists them."}, t.deleteResource)
	mcp.AddTool(server, &mcp.Tool{Name: "delete_section", Description: "Delete a section. A section that is not empty " +
		"needs recursive. Links into it break: the answer lists them."}, t.deleteSection)
}

// targetPath resolves a path from the wiki root, or relative to a page, to a clean path
// relative to the wiki root
func (t *mcpTools) targetPath(p, document string) (string, error) {
	from := ""
	if strings.TrimSpace(document) != "" {
		d, err := t.document(document)
		if err != nil {
			return "", err
		}
		from = d.RelPath
	}
	dest, ok := references.ParseDestination(from, strings.TrimSpace(p))
	if !ok || dest.Outside || dest.Path == "" || dest.Anchor != "" || strings.ContainsAny(p, "?#") {
		return "", fmt.Errorf("invalid path %q: give a path inside the wiki", p)
	}
	return dest.Path, nil
}

// ensureSection returns the section at a path, creating it and missing parents. created are
// the paths of the new sections.
func (t *mcpTools) ensureSection(rel string) (s *wiki.Section, created []string, err error) {
	tm := t.rs.treeManager
	s = tm.Root()
	if rel == "" || rel == "." {
		return s, nil, nil
	}
	for _, name := range strings.Split(rel, "/") {
		next := tm.GetSection(tm.IDOfRelPath(path.Join(s.RelPath, name)))
		if next == nil {
			if next, err = tm.CreateSection(s.ID, name); err != nil {
				return nil, created, fmt.Errorf("cannot create the section %s: %w", path.Join(s.RelPath, name), err)
			}
			created = append(created, next.RelPath)
			t.rs.itemCreatedBy(t.caller.user.ID, wiki.TypeSection, next.Path)
		}
		s = next
	}
	return s, created, nil
}

func (t *mcpTools) uploadResource(_ context.Context, _ *mcp.CallToolRequest, in mcpUploadInput) (*mcp.CallToolResult, any, error) {
	rel, err := t.uploadTarget(in.Path, in.Document)
	if err != nil {
		return nil, nil, err
	}
	var content []byte
	switch {
	case in.Text != "" && in.Base64 != "":
		return nil, nil, errors.New("give text or base64, not both")
	case in.Base64 != "":
		if content, err = base64.StdEncoding.DecodeString(strings.Join(strings.Fields(in.Base64), "")); err != nil {
			return nil, nil, fmt.Errorf("base64 is invalid: %w", err)
		}
	default:
		content = []byte(in.Text)
	}
	msg, err := t.storeResource(rel, bytes.NewReader(content), in.Replace)
	if err != nil {
		return nil, nil, err
	}
	return text(msg), nil, nil
}

// uploadTarget resolves and checks the path of an upload: no page, a valid name, no section
func (t *mcpTools) uploadTarget(p, document string) (string, error) {
	rel, err := t.targetPath(p, document)
	if err != nil {
		return "", err
	}
	if strings.HasSuffix(strings.ToLower(rel), wiki.MarkdownFileExtension) {
		return "", errors.New("pages (.md) are created with create_document and changed with edit_document")
	}
	if err = wiki.ValidateName(path.Base(rel)); err != nil {
		return "", fmt.Errorf("invalid file name %q: no leading dot, no slashes or control characters", path.Base(rel))
	}
	tm := t.rs.treeManager
	if tm.GetDocument(tm.IDOfRelPath(rel)) != nil || tm.GetSection(tm.IDOfRelPath(rel)) != nil {
		return "", fmt.Errorf("%s is a page or a section, not a file", rel)
	}
	return rel, nil
}

// storeResource creates or replaces a file at a checked path and commits it. It returns the
// message for the agent.
func (t *mcpTools) storeResource(rel string, content io.Reader, replace bool) (string, error) {
	tm := t.rs.treeManager
	if err := t.allowWrite(); err != nil {
		return "", err
	}
	var r *wiki.Resource
	var created []string
	var err error
	verb, done := "Upload", "uploaded"
	if existing := tm.GetResource(tm.IDOfRelPath(rel)); existing != nil {
		if !replace {
			return "", fmt.Errorf("%s exists (%s): pass replace to overwrite it", rel, formatBytes(int(existing.Filesize)))
		}
		if r, err = tm.ReplaceResource(existing.ID, content); err != nil {
			return "", err
		}
		verb, done = "Replace", "replaced"
	} else {
		var s *wiki.Section
		if s, created, err = t.ensureSection(path.Dir(rel)); err != nil {
			return "", err
		}
		if r, err = tm.CreateResource(s.ID, path.Base(rel), content); err != nil {
			return "", err
		}
	}
	if r == nil {
		return "", fmt.Errorf("%s was written, but is not in the tree: is the name excluded?", rel)
	}
	t.rs.commitTreeAs(t.caller.user, verb+" %s", r.Path)
	msg := fmt.Sprintf("%s %s (%s, %s)", done, r.RelPath, formatBytes(int(r.Filesize)), r.MimeType)
	if len(created) > 0 {
		msg += "\ncreated sections: " + strings.Join(created, ", ")
	}
	return msg, nil
}

func (t *mcpTools) createSection(_ context.Context, _ *mcp.CallToolRequest, in mcpSectionPathInput) (*mcp.CallToolResult, any, error) {
	rel, err := t.targetPath(in.Path, "")
	if err != nil {
		return nil, nil, err
	}
	tm := t.rs.treeManager
	if tm.GetSection(tm.IDOfRelPath(rel)) != nil {
		return nil, nil, fmt.Errorf("the section %s exists", rel)
	}
	if err = t.allowWrite(); err != nil {
		return nil, nil, err
	}
	_, created, err := t.ensureSection(rel)
	if err != nil {
		return nil, nil, err
	}
	return text("created sections: " + strings.Join(created, ", ")), nil, nil
}

// moveItem moves an item and describes the result
func (t *mcpTools) moveItem(req *collab.MoveRequest) (*mcp.CallToolResult, *mcpMoved, error) {
	if req.Parent == "" && req.Name == "" {
		return nil, nil, errors.New("give a target section, a new name, or both")
	}
	if err := t.allowWrite(); err != nil {
		return nil, nil, err
	}
	result, err := t.rs.performMoveAs(t.caller.user, t.caller.info, req)
	if err != nil {
		var me *collab.MoveError
		if errors.As(err, &me) {
			return nil, nil, errors.New(me.Message)
		}
		return nil, nil, err
	}
	// the item types are the routes of the web client
	return nil, &mcpMoved{OldPath: result.OldPath, NewPath: result.NewPath, ID: result.NewID, URL: t.webURL(req.Type, result.NewID),
		UpdatedDocuments: len(result.Documents)}, nil
}

func (t *mcpTools) moveResource(_ context.Context, _ *mcp.CallToolRequest, in mcpMoveResourceInput) (*mcp.CallToolResult, *mcpMoved, error) {
	r, err := t.resource(in.Resource, in.Document)
	if err != nil {
		return nil, nil, err
	}
	req := &collab.MoveRequest{Type: wiki.TypeResource, ID: r.ID, Name: strings.TrimSpace(in.Name)}
	if in.Section != "" {
		s, err := t.section(in.Section)
		if err != nil {
			return nil, nil, err
		}
		req.Parent = s.ID
	}
	return t.moveItem(req)
}

func (t *mcpTools) moveSection(_ context.Context, _ *mcp.CallToolRequest, in mcpMoveSectionInput) (*mcp.CallToolResult, *mcpMoved, error) {
	s, err := t.section(in.Section)
	if err != nil {
		return nil, nil, err
	}
	if s.ID == t.rs.treeManager.Root().ID {
		return nil, nil, errors.New("the top level cannot be moved")
	}
	req := &collab.MoveRequest{Type: wiki.TypeSection, ID: s.ID, Name: strings.TrimSpace(in.Name)}
	if in.Target != "" {
		target, err := t.section(in.Target)
		if err != nil {
			return nil, nil, err
		}
		req.Parent = target.ID
	}
	return t.moveItem(req)
}

// deleteItem deletes an item and lists the links that broke, from sources outside of it
func (t *mcpTools) deleteItem(id, itemType string, force bool, broken *Backlinks) (*mcp.CallToolResult, any, error) {
	if err := t.allowWrite(); err != nil {
		return nil, nil, err
	}
	p, err := t.rs.deleteItemAs(t.caller.user, id, itemType, force)
	if errors.Is(err, collab.ErrDocumentInUse) {
		return nil, nil, errors.New("people have the page open: pass force to delete it anyway")
	}
	if err != nil {
		return nil, nil, err
	}
	msg := "deleted " + t.rs.relOf(p)
	var lines []string
	for _, b := range broken.Backlinks {
		if !b.Internal {
			lines = append(lines, fmt.Sprintf("%s:%d %s", b.Source.Path, b.Line, b.Destination))
		}
	}
	if len(lines) > 0 {
		msg += fmt.Sprintf("\nthese %d links are broken now, fix them with edit_document:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	return text(msg), nil, nil
}

func (t *mcpTools) deleteDocument(_ context.Context, _ *mcp.CallToolRequest, in mcpDeleteDocumentInput) (*mcp.CallToolResult, any, error) {
	d, err := t.document(in.Document)
	if err != nil {
		return nil, nil, err
	}
	broken := t.rs.documentBacklinks(d)
	for _, b := range broken.Backlinks {
		b.Internal = b.Source.ID == d.ID
	}
	return t.deleteItem(d.ID, wiki.TypeDocument, in.Force, broken)
}

func (t *mcpTools) deleteResource(_ context.Context, _ *mcp.CallToolRequest, in mcpResourceRef) (*mcp.CallToolResult, any, error) {
	r, err := t.resource(in.Resource, in.Document)
	if err != nil {
		return nil, nil, err
	}
	broken := t.rs.backlinksOf(references.ResourceRef(r), "", func(target *references.ItemRef) bool {
		return target.Type == wiki.TypeResource && target.ID == r.ID
	})
	return t.deleteItem(r.ID, wiki.TypeResource, false, broken)
}

func (t *mcpTools) deleteSection(_ context.Context, _ *mcp.CallToolRequest, in mcpDeleteSectionInput) (*mcp.CallToolResult, any, error) {
	s, err := t.section(in.Section)
	if err != nil {
		return nil, nil, err
	}
	if s.ID == t.rs.treeManager.Root().ID {
		return nil, nil, errors.New("the top level cannot be deleted")
	}
	if !in.Recursive && (len(s.Documents) > 0 || len(s.Resources) > 0 || len(s.Subsections) > 0) {
		return nil, nil, fmt.Errorf("%s has %d pages, %d files and %d sections: pass recursive to delete them all",
			s.RelPath, len(s.Documents), len(s.Resources), len(s.Subsections))
	}
	broken := t.rs.backlinksOf(references.SectionRef(s), s.RelPath, func(target *references.ItemRef) bool {
		return wiki.IsInside(target.Path, s.RelPath)
	})
	return t.deleteItem(s.ID, wiki.TypeSection, in.Force, broken)
}

func (t *mcpTools) checkLinks(_ context.Context, _ *mcp.CallToolRequest, in mcpCheckLinksInput) (*mcp.CallToolResult, any, error) {
	section := ""
	if strings.Trim(in.Section, "/") != "" {
		s, err := t.section(in.Section)
		if err != nil {
			return nil, nil, err
		}
		section = s.RelPath
	}
	inside := func(p string) bool { return section == "" || wiki.IsInside(p, section) }
	snap := t.rs.refs.Snapshot()
	var b strings.Builder
	var broken, unused []string
	for _, r := range snap.All() {
		if r.Target != nil || !inside(r.Source.RelPath) {
			continue
		}
		ref := t.rs.toReference(r)
		line := fmt.Sprintf("%s:%d %s (%s)", r.Source.RelPath, ref.Line, ref.Destination, ref.Reason)
		if ref.MovedTo != nil {
			line += fmt.Sprintf(", moved to %s: use %s", ref.MovedTo.Path, ref.SuggestedDestination)
		}
		broken = append(broken, line)
	}
	for _, r := range snap.UnusedResources() {
		if inside(r.RelPath) {
			unused = append(unused, fmt.Sprintf("%s (%s)", r.RelPath, formatBytes(int(r.Filesize))))
		}
	}
	fmt.Fprintf(&b, "broken links: %d", len(broken))
	for _, l := range broken {
		b.WriteString("\n" + l)
	}
	fmt.Fprintf(&b, "\n\nfiles no page or mkdocs.yml links to: %d", len(unused))
	for _, l := range unused {
		b.WriteString("\n" + l)
	}
	return text(b.String()), nil, nil
}

func (t *mcpTools) linkTo(_ context.Context, _ *mcp.CallToolRequest, in mcpLinkInput) (*mcp.CallToolResult, *LinkResult, error) {
	from, err := t.document(in.Document)
	if err != nil {
		return nil, nil, err
	}
	lookup := references.NewTreeLookup(t.rs.treeManager.Root())
	ref := strings.TrimSpace(in.Target)
	target := lookup.Item(ref)
	if target == nil {
		rel := strings.Trim(ref, "/")
		target = lookup.Resolve(rel)
		if target == nil && !strings.HasSuffix(rel, wiki.MarkdownFileExtension) {
			target = lookup.Resolve(rel + wiki.MarkdownFileExtension)
		}
	}
	if target == nil {
		return nil, nil, fmt.Errorf("no page, file or section %q: use list_tree", in.Target)
	}
	return nil, linkTo(lookup, from, target, strings.TrimPrefix(strings.TrimSpace(in.Anchor), "#")), nil
}

func (t *mcpTools) recentChanges(_ context.Context, _ *mcp.CallToolRequest, in mcpFeedInput) (*mcp.CallToolResult, any, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = mcpFeedDefaultLimit
	}
	feed, err := t.rs.feed(strings.TrimSpace(in.Before), min(limit, mcpFeedMaxLimit))
	if err != nil {
		return nil, nil, err
	}
	var b strings.Builder
	b.WriteString("newest first")
	for _, e := range feed.Entries {
		var who []string
		for _, a := range e.Authors {
			who = append(who, a.DisplayName)
		}
		fmt.Fprintf(&b, "\n%s %s %s %s", e.Time, e.Kind, e.ItemType, e.Path)
		if e.OldPath != "" {
			fmt.Fprintf(&b, " (from %s)", e.OldPath)
		}
		// only pages that are still there, under that path
		if e.ItemType == wiki.TypeDocument {
			if u := t.documentURLOfPath(e.Path); u != "" {
				fmt.Fprintf(&b, " %s", u)
			}
		}
		if len(who) > 0 {
			fmt.Fprintf(&b, " by %s", strings.Join(who, ", "))
		}
		if s := e.Summary; s != nil {
			fmt.Fprintf(&b, ": +%d -%d lines", s.LinesAdded, s.LinesRemoved)
			if len(s.Headings) > 0 {
				fmt.Fprintf(&b, " in %s", strings.Join(s.Headings, ", "))
			}
		}
	}
	if len(feed.Entries) == 0 {
		b.WriteString("\nno changes")
	}
	if feed.Next != nil {
		fmt.Fprintf(&b, "\nolder entries: before %s", *feed.Next)
	}
	return text(b.String()), nil, nil
}

func (t *mcpTools) blame(_ context.Context, _ *mcp.CallToolRequest, in mcpDocumentRef) (*mcp.CallToolResult, any, error) {
	if err := t.historyRepo(); err != nil {
		return nil, nil, err
	}
	d, err := t.document(in.Document)
	if err != nil {
		return nil, nil, err
	}
	resp, err := t.rs.blame(d, t.rs.git.Repo())
	if err != nil {
		return nil, nil, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "path: %s\nrevision: %s\nlines: %d", d.RelPath, resp.Revision, resp.Lines)
	for _, bl := range resp.Blocks {
		lines := fmt.Sprintf("%d", bl.Start)
		if bl.End != bl.Start {
			lines += fmt.Sprintf("-%d", bl.End)
		}
		who := "unknown"
		if bl.Author != nil {
			who = bl.Author.DisplayName
		}
		if len(bl.Agents) > 0 {
			who += " (agent " + strings.Join(bl.Agents, ", ") + ")"
		}
		if bl.Pending {
			fmt.Fprintf(&b, "\n%s: not committed yet, by %s", lines, who)
			continue
		}
		fmt.Fprintf(&b, "\n%s: %s %s by %s: %s", lines, shortRevision(*bl.Revision), *bl.Time, who, *bl.Message)
	}
	return text(b.String()), nil, nil
}

// maxMetaDescription is the most code points of a description in list_tree
const maxMetaDescription = 120

// formatPageMeta is the meta column of list_tree: the front matter fields that are set
func formatPageMeta(m wiki.PageMeta) string {
	var parts []string
	if len(m.Tags) > 0 {
		parts = append(parts, "tags: "+strings.Join(m.Tags, ", "))
	}
	if m.Status != nil {
		parts = append(parts, "status: "+*m.Status)
	}
	if m.Description != nil {
		d := []rune(*m.Description)
		if len(d) > maxMetaDescription {
			d = append(d[:maxMetaDescription], '…')
		}
		parts = append(parts, "description: "+string(d))
	}
	if m.SearchExclude {
		parts = append(parts, "excluded from search")
	}
	return strings.Join(parts, "; ")
}

func (t *mcpTools) readWikiConfig(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	// the filtered config of GET /wiki/config/, never the whole mkdocs.yml: it can hold secrets
	config, err := wiki.ReadWikiConfig(t.rs.treeManager.ConfigFile())
	if err != nil {
		log.Printf("Unable to read the wiki config: %v", err)
		return nil, nil, errors.New("mkdocs.yml cannot be read")
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return text(string(data)), nil, nil
}
