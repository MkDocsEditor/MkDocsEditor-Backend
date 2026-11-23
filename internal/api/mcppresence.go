package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/activity"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/collab"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Agent presence for MCP clients: point_at moves the agent's focus, and viewers tells who has a
// page open. See docs/mcp.md section 4.

// maxPointAtText is the longest select or heading of point_at
const maxPointAtText = 4096

type (
	mcpPointAtInput struct {
		Document string `json:"document" jsonschema:"page path relative to the wiki root, like guide/setup.md, or the page id"`
		Select   string `json:"select,omitempty" jsonschema:"a text of the page that occurs exactly once: it becomes the selection"`
		Heading  string `json:"heading,omitempty" jsonschema:"the text of a heading: the cursor goes to it and the view to its section"`
		Clear    bool   `json:"clear,omitempty" jsonschema:"true: remove your cursor from the page"`
	}

	mcpPointed struct {
		Path    string   `json:"path"`
		Viewers []string `json:"viewers"`
	}
)

// viewers returns the display names of the accounts that have a page open, sorted, the caller's
// own marked "(you)"
func (t *mcpTools) viewers(d *wiki.Document) []string {
	out := []string{}
	for _, id := range t.rs.sync.Viewers(d) {
		name := id
		if u := t.rs.authenticator.UserByID(id); u != nil {
			name = u.DisplayName
			if name == "" {
				name = u.Username
			}
		}
		if id == t.caller.user.ID {
			name += " (you)"
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// formatViewers is the viewers line of read_document
func formatViewers(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

func (t *mcpTools) pointAt(_ context.Context, _ *mcp.CallToolRequest, in mcpPointAtInput) (*mcp.CallToolResult, *mcpPointed, error) {
	d, err := t.document(in.Document)
	if err != nil {
		return nil, nil, err
	}
	given := 0
	for _, set := range []bool{in.Select != "", in.Heading != "", in.Clear} {
		if set {
			given++
		}
	}
	if given != 1 {
		return nil, nil, errors.New("give exactly one of select, heading and clear")
	}
	if len(in.Select) > maxPointAtText || len(in.Heading) > maxPointAtText {
		return nil, nil, errors.New("select and heading are limited to 4 KiB")
	}
	if t.caller.token == nil {
		return nil, nil, errors.New("point_at needs an app token")
	}
	if err = t.rs.authenticator.AllowAgentFocus(t.caller.rateKey(), t.caller.info.IP); err != nil {
		var rl *auth.RateLimitError
		if errors.As(err, &rl) {
			return nil, nil, fmt.Errorf("too many moves, retry in %d seconds", rl.Seconds())
		}
		return nil, nil, err
	}
	if len(t.rs.sync.Viewers(d)) == 0 {
		// before reading, so that a page nobody has open is not loaded
		return nil, nil, focusToolError(collab.ErrNobodyOpen)
	}
	agent := t.caller.agent()
	if in.Clear {
		if err = t.rs.sync.ClearAgentFocus(d, agent); err != nil {
			return nil, nil, focusToolError(err)
		}
		return nil, &mcpPointed{Path: d.RelPath, Viewers: t.viewers(d)}, nil
	}
	content, revision, err := t.rs.sync.ReadContent(d)
	if err != nil {
		return nil, nil, err
	}
	focus := collab.AgentFocus{Revision: revision}
	if in.Select != "" {
		at := strings.Index(content, in.Select)
		if at < 0 {
			return nil, nil, errors.New("select does not occur in the page")
		}
		// overlapping occurrences count too, like the edits of edit_document
		if strings.Contains(content[at+1:], in.Select) {
			return nil, nil, errors.New("select occurs more than once, add context to make it unique")
		}
		focus.From = utf8.RuneCountInString(content[:at])
		focus.To = focus.From + utf8.RuneCountInString(in.Select)
	} else {
		from, viewport, err := headingFocus(content, in.Heading)
		if err != nil {
			return nil, nil, err
		}
		focus.From, focus.To, focus.Viewport = from, from, viewport
	}
	if err = t.rs.sync.SetAgentFocus(d, agent, focus); err != nil {
		return nil, nil, focusToolError(err)
	}
	return nil, &mcpPointed{Path: d.RelPath, Viewers: t.viewers(d)}, nil
}

// headingFocus finds the only heading with a text: the index of the start of its line and the
// viewport of its section, in code points. Leading # and surrounding spaces are ignored.
func headingFocus(content string, heading string) (int, *collab.FocusViewport, error) {
	heading = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(heading), "#"))
	if heading == "" {
		return 0, nil, errors.New("heading is empty")
	}
	start, end, ok := activity.SectionLines(content, heading, 1)
	if !ok {
		return 0, nil, fmt.Errorf("no such heading %q", heading)
	}
	if _, _, twice := activity.SectionLines(content, heading, 2); twice {
		return 0, nil, fmt.Errorf("the heading %q occurs more than once", heading)
	}
	// code point offsets of the line starts, the last one the length of the text
	lineStarts := []int{0}
	n := 0
	for _, r := range content {
		n++
		if r == '\n' {
			lineStarts = append(lineStarts, n)
		}
	}
	if lineStarts[len(lineStarts)-1] != n {
		lineStarts = append(lineStarts, n)
	}
	top := lineStarts[start]
	bottom := n - 1
	if end < len(lineStarts)-1 {
		bottom = lineStarts[end] - 1
	}
	return top, &collab.FocusViewport{Top: top, Bottom: max(bottom, top)}, nil
}

// focusToolError turns an error of a focus into a message for the agent
func focusToolError(err error) error {
	switch {
	case errors.Is(err, collab.ErrNobodyOpen):
		return errors.New("nobody has this page open: there is nobody to point at anything")
	case errors.Is(err, collab.ErrNoAppToken):
		return errors.New("point_at needs an app token")
	case errors.Is(err, collab.ErrUnknownRevision):
		return errors.New("the page changed in a way that lost this position: try again")
	}
	return err
}
