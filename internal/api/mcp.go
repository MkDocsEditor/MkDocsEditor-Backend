package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/collab"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	"github.com/labstack/echo/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The MCP server at /mcp: tools for LLM agents, thin wrappers over the content API. It speaks
// the streamable HTTP transport without sessions and authenticates like every other route,
// usually with an app token. See docs/mcp.md.

// EndpointPathMCP is the path of the MCP server, below APIPrefix
const EndpointPathMCP = "/mcp/"

const mcpInstructions = `A family wiki of markdown pages (MkDocs). Find pages with search or list_tree,
read_document before changing a page, then change it with edit_document (preferred) or
write_document. People may edit the same page at the same time: changes merge, so keep edits small.`

// mcpCaller is who calls a tool: the user and the app token of the HTTP request
type mcpCaller struct {
	user  *auth.User
	token *auth.AppToken
	info  auth.RequestInfo
}

type mcpCallerKey struct{}

// agent returns who changes content with a tool call
func (c *mcpCaller) agent() collab.Agent {
	agent := collab.Agent{AccountID: c.user.ID}
	if c.token != nil {
		agent.TokenID, agent.TokenName = c.token.ID, c.token.Name
	}
	return agent
}

// rateKey is the key of the content write limit: per app token, else per account
func (c *mcpCaller) rateKey() string {
	if c.token != nil {
		return "token:" + c.token.ID
	}
	return contentWriteRateKeyPrefix + c.user.ID
}

// newMCPHandler returns the echo handler of the MCP server. The SDK handler gets a server per
// request with the tools the caller may use.
func (rs *RestService) newMCPHandler() echo.HandlerFunc {
	schemas := mcp.NewSchemaCache()
	handler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		caller, _ := r.Context().Value(mcpCallerKey{}).(*mcpCaller)
		if caller == nil {
			return nil
		}
		return rs.newMCPServer(caller, schemas)
	}, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
		// every request is authenticated, and behind the reverse proxy requests come from
		// localhost with the public host name
		DisableLocalhostProtection: true,
		MaxRequestBodyBytes:        maxContentRequestBytes,
	})
	return func(c *echo.Context) error {
		caller := &mcpCaller{user: UserFromContext(c), token: AppTokenFromContext(c), info: requestInfo(c)}
		req := c.Request()
		handler.ServeHTTP(c.Response(), req.WithContext(context.WithValue(req.Context(), mcpCallerKey{}, caller)))
		return nil
	}
}

// newMCPServer creates the MCP server of a caller: the read tools, and the write tools if the
// caller may change content (role editor or admin, app token with the scope write)
func (rs *RestService) newMCPServer(caller *mcpCaller, schemas *mcp.SchemaCache) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "mkdocseditor", Title: "MkDocsEditor wiki"},
		&mcp.ServerOptions{Instructions: mcpInstructions, SchemaCache: schemas})
	tools := &mcpTools{rs: rs, caller: caller}
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}
	mcp.AddTool(server, &mcp.Tool{Name: "search", Annotations: readOnly, Description: "Full text search over all pages. " +
		"Every word must match (case-insensitive) in the text, title or path. Returns paths, titles and matching lines."}, tools.search)
	mcp.AddTool(server, &mcp.Tool{Name: "list_tree", Annotations: readOnly, Description: "List the pages (path and title), " +
		"optionally only those below a section path."}, tools.listTree)
	mcp.AddTool(server, &mcp.Tool{Name: "read_document", Annotations: readOnly, Description: "Read a page: its markdown and its revision. " +
		"Read before you change a page."}, tools.readDocument)
	mcp.AddTool(server, &mcp.Tool{Name: "get_backlinks", Annotations: readOnly, Description: "List the pages that link to a page."}, tools.getBacklinks)
	if !caller.user.CanWrite() {
		return server
	}
	notDestructive := false
	mcp.AddTool(server, &mcp.Tool{Name: "edit_document", Annotations: &mcp.ToolAnnotations{DestructiveHint: &notDestructive},
		Description: "Change a page by replacing text. Each old must match the page exactly once, including whitespace; " +
			"add surrounding lines to make it unique. Edits apply in order, all or none. Pass the revision from read_document " +
			"as baseRevision. Prefer this over write_document."}, tools.editDocument)
	mcp.AddTool(server, &mcp.Tool{Name: "write_document", Description: "Replace the whole markdown of a page. baseRevision " +
		"(from read_document) is required; changes others made since are kept. Use edit_document for partial changes."}, tools.writeDocument)
	mcp.AddTool(server, &mcp.Tool{Name: "create_document", Annotations: &mcp.ToolAnnotations{DestructiveHint: &notDestructive},
		Description: "Create a page in a section (a folder path, empty for the top level), optionally with content."}, tools.createDocument)
	mcp.AddTool(server, &mcp.Tool{Name: "move_document", Description: "Move or rename a page. Links to it in other pages are " +
		"updated. Give the target section path, the new name (without .md), or both."}, tools.moveDocument)
	return server
}

// mcpTools are the tool handlers of one caller
type mcpTools struct {
	rs     *RestService
	caller *mcpCaller
}

type (
	mcpDocumentRef struct {
		Document string `json:"document" jsonschema:"page path relative to the wiki root, like guide/setup.md, or the page id"`
	}

	mcpSearchInput struct {
		Query string `json:"query" jsonschema:"words to search for"`
		Limit int    `json:"limit,omitempty" jsonschema:"maximum number of results, 1 to 100, default 20"`
	}

	mcpListInput struct {
		Section string `json:"section,omitempty" jsonschema:"only pages below this section path, like guide"`
	}

	mcpEditInput struct {
		Document     string            `json:"document" jsonschema:"page path relative to the wiki root, like guide/setup.md, or the page id"`
		Edits        []collab.TextEdit `json:"edits" jsonschema:"replacements: old must occur exactly once, new replaces it"`
		BaseRevision string            `json:"baseRevision,omitempty" jsonschema:"revision from read_document the edits refer to"`
	}

	mcpWriteInput struct {
		Document     string `json:"document" jsonschema:"page path relative to the wiki root, like guide/setup.md, or the page id"`
		Content      string `json:"content" jsonschema:"the complete new markdown"`
		BaseRevision string `json:"baseRevision" jsonschema:"revision from read_document the content is based on"`
	}

	mcpCreateInput struct {
		Section string `json:"section" jsonschema:"section path for the page, empty for the top level"`
		Name    string `json:"name" jsonschema:"file name of the page without .md"`
		Content string `json:"content,omitempty" jsonschema:"markdown of the new page"`
	}

	mcpMoveInput struct {
		Document string `json:"document" jsonschema:"page path relative to the wiki root, like guide/setup.md, or the page id"`
		Section  string `json:"section,omitempty" jsonschema:"target section path, empty to stay in the section; / for the top level"`
		Name     string `json:"name,omitempty" jsonschema:"new file name without .md, empty to keep the name"`
	}

	mcpRevision struct {
		Path     string `json:"path"`
		Revision string `json:"revision"`
	}

	mcpCreated struct {
		ID       string `json:"id"`
		Path     string `json:"path"`
		Revision string `json:"revision"`
	}

	mcpMoved struct {
		OldPath string `json:"oldPath"`
		NewPath string `json:"newPath"`
		ID      string `json:"id"`
		// UpdatedDocuments is the number of pages whose links were updated
		UpdatedDocuments int `json:"updatedDocuments"`
	}

	mcpBacklink struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Link string `json:"link"`
	}

	mcpBacklinks struct {
		Backlinks []mcpBacklink `json:"backlinks"`
	}
)

// document finds a page by path (with or without .md, with or without a leading slash) or id
func (t *mcpTools) document(ref string) (*wiki.Document, error) {
	ref = strings.TrimSpace(ref)
	tm := t.rs.treeManager
	if d := tm.GetDocument(ref); d != nil {
		return d, nil
	}
	rel := strings.Trim(ref, "/")
	if !strings.HasSuffix(rel, wiki.MarkdownFileExtension) {
		rel += wiki.MarkdownFileExtension
	}
	if rel != wiki.MarkdownFileExtension && path.Clean(rel) == rel {
		if d := tm.GetDocument(tm.IDOfRelPath(rel)); d != nil {
			return d, nil
		}
	}
	return nil, fmt.Errorf("no page %q: use search or list_tree to find the path", ref)
}

// section finds a section by path ("" and "/" are the top level) or id
func (t *mcpTools) section(ref string) (*wiki.Section, error) {
	tm := t.rs.treeManager
	ref = strings.TrimSpace(ref)
	if s := tm.GetSection(ref); s != nil && ref != "" {
		return s, nil
	}
	rel := strings.Trim(ref, "/")
	if rel == "" || path.Clean(rel) == rel {
		if s := tm.GetSection(tm.IDOfRelPath(rel)); s != nil {
			return s, nil
		}
	}
	return nil, fmt.Errorf("no section %q: use list_tree to see the sections", ref)
}

// allowWrite applies the rate limit of content changes
func (t *mcpTools) allowWrite() error {
	if err := t.rs.authenticator.AllowContentWrite(t.caller.rateKey(), t.caller.info.IP); err != nil {
		var rl *auth.RateLimitError
		if errors.As(err, &rl) {
			return fmt.Errorf("too many changes, retry in %d seconds", rl.Seconds())
		}
		return err
	}
	return nil
}

// contentToolError turns an error of a content change into a message for the agent
func contentToolError(err error) error {
	switch {
	case errors.Is(err, collab.ErrUnknownRevision):
		return errors.New("the page does not know this revision any more: read_document again and redo the change")
	case errors.Is(err, collab.ErrInvalidRevision):
		return errors.New("invalid baseRevision: pass the revision read_document returned")
	}
	return err
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func (t *mcpTools) search(_ context.Context, _ *mcp.CallToolRequest, in mcpSearchInput) (*mcp.CallToolResult, *SearchResponse, error) {
	q := strings.TrimSpace(in.Query)
	if q == "" || len([]rune(q)) > maxSearchQueryRunes {
		return nil, nil, errors.New("query must have 1 to 200 characters")
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	return nil, t.rs.search(q, min(limit, maxSearchLimit)), nil
}

func (t *mcpTools) listTree(_ context.Context, _ *mcp.CallToolRequest, in mcpListInput) (*mcp.CallToolResult, any, error) {
	root := t.rs.treeManager.Root()
	if strings.Trim(in.Section, "/") != "" {
		s, err := t.section(in.Section)
		if err != nil {
			return nil, nil, err
		}
		root = s
	}
	var lines []string
	for s := range root.Walk() {
		for _, d := range s.Documents {
			lines = append(lines, d.RelPath+"\t"+d.Title)
		}
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return text("no pages"), nil, nil
	}
	return text("path\ttitle\n" + strings.Join(lines, "\n")), nil, nil
}

func (t *mcpTools) readDocument(_ context.Context, _ *mcp.CallToolRequest, in mcpDocumentRef) (*mcp.CallToolResult, any, error) {
	d, err := t.document(in.Document)
	if err != nil {
		return nil, nil, err
	}
	content, revision, err := t.rs.sync.ReadContent(d)
	if err != nil {
		return nil, nil, err
	}
	return text(fmt.Sprintf("path: %s\ntitle: %s\nrevision: %s\n---\n%s", d.RelPath, d.Title, revision, content)), nil, nil
}

func (t *mcpTools) getBacklinks(_ context.Context, _ *mcp.CallToolRequest, in mcpDocumentRef) (*mcp.CallToolResult, *mcpBacklinks, error) {
	d, err := t.document(in.Document)
	if err != nil {
		return nil, nil, err
	}
	out := &mcpBacklinks{Backlinks: []mcpBacklink{}}
	for _, b := range t.rs.documentBacklinks(d).Backlinks {
		out.Backlinks = append(out.Backlinks, mcpBacklink{Path: b.Source.Path, Line: b.Line, Link: b.Destination})
	}
	return nil, out, nil
}

func (t *mcpTools) editDocument(_ context.Context, _ *mcp.CallToolRequest, in mcpEditInput) (*mcp.CallToolResult, *mcpRevision, error) {
	d, err := t.document(in.Document)
	if err != nil {
		return nil, nil, err
	}
	if len(in.Edits) == 0 || len(in.Edits) > maxEditsPerRequest {
		return nil, nil, errors.New("edits must have 1 to 100 entries")
	}
	if err = t.allowWrite(); err != nil {
		return nil, nil, err
	}
	revision, err := t.rs.sync.ApplyEdits(d, in.Edits, in.BaseRevision, t.caller.agent())
	if err != nil {
		return nil, nil, contentToolError(err)
	}
	return nil, &mcpRevision{Path: d.RelPath, Revision: revision}, nil
}

func (t *mcpTools) writeDocument(_ context.Context, _ *mcp.CallToolRequest, in mcpWriteInput) (*mcp.CallToolResult, *mcpRevision, error) {
	d, err := t.document(in.Document)
	if err != nil {
		return nil, nil, err
	}
	if in.BaseRevision == "" {
		return nil, nil, errors.New("baseRevision is required: read_document first")
	}
	if err = t.allowWrite(); err != nil {
		return nil, nil, err
	}
	revision, err := t.rs.sync.WriteContent(d, in.Content, in.BaseRevision, t.caller.agent())
	if err != nil {
		return nil, nil, contentToolError(err)
	}
	return nil, &mcpRevision{Path: d.RelPath, Revision: revision}, nil
}

func (t *mcpTools) createDocument(_ context.Context, _ *mcp.CallToolRequest, in mcpCreateInput) (*mcp.CallToolResult, *mcpCreated, error) {
	s, err := t.section(in.Section)
	if err != nil {
		return nil, nil, err
	}
	if err = t.allowWrite(); err != nil {
		return nil, nil, err
	}
	d, err := t.rs.createDocumentAs(t.caller.user, s.ID, strings.TrimSuffix(strings.TrimSpace(in.Name), wiki.MarkdownFileExtension))
	if err != nil {
		return nil, nil, err
	}
	_, revision, err := t.rs.sync.ReadContent(d)
	if err != nil {
		return nil, nil, err
	}
	if in.Content != "" {
		if revision, err = t.rs.sync.WriteContent(d, in.Content, revision, t.caller.agent()); err != nil {
			return nil, nil, fmt.Errorf("the page %s was created, but writing its content failed: %w", d.RelPath, err)
		}
	}
	return nil, &mcpCreated{ID: d.ID, Path: d.RelPath, Revision: revision}, nil
}

func (t *mcpTools) moveDocument(_ context.Context, _ *mcp.CallToolRequest, in mcpMoveInput) (*mcp.CallToolResult, *mcpMoved, error) {
	d, err := t.document(in.Document)
	if err != nil {
		return nil, nil, err
	}
	req := &collab.MoveRequest{Type: wiki.TypeDocument, ID: d.ID, Name: strings.TrimSuffix(strings.TrimSpace(in.Name), wiki.MarkdownFileExtension)}
	if in.Section != "" {
		s, err := t.section(in.Section)
		if err != nil {
			return nil, nil, err
		}
		req.Parent = s.ID
	}
	if req.Parent == "" && req.Name == "" {
		return nil, nil, errors.New("give a target section, a new name, or both")
	}
	if err = t.allowWrite(); err != nil {
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
	return nil, &mcpMoved{OldPath: result.OldPath, NewPath: result.NewPath, ID: result.NewID, UpdatedDocuments: len(result.Documents)}, nil
}
