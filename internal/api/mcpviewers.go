package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/activity"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/collab"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Where the people who have a page open are: read_presence and the block of read_document. See
// docs/mcp.md section 4.

const (
	// maxPresenceViewers is the most viewers read_presence and read_document describe
	maxPresenceViewers = 10
	// maxSelectionText is the most code points of a selected text in an answer
	maxSelectionText = 200
	// maxViewportHeadings is the most headings of a viewport in an answer
	maxViewportHeadings = 10
)

type (
	mcpPresence struct {
		Path     string      `json:"path"`
		Revision string      `json:"revision"`
		Viewers  []mcpViewer `json:"viewers"`
	}

	mcpViewer struct {
		Name   string `json:"name"`
		Device string `json:"device,omitempty"`
		// Updated is the seconds since the last presence of the connection
		Updated int `json:"updated"`
		// Position is "unknown" if the client sends no indexes
		Position    string    `json:"position,omitempty"`
		Approximate bool      `json:"approximate,omitempty"`
		Cursor      *mcpPlace `json:"cursor,omitempty"`
		// Heading is the heading the cursor is under, like "## Setup"
		Heading string `json:"heading,omitempty"`
		// Selections is the number of selections if there is more than one
		Selections int           `json:"selections,omitempty"`
		Selection  *mcpSelection `json:"selection,omitempty"`
		Viewport   *mcpLines     `json:"viewport,omitempty"`
	}

	// mcpPlace is a position: line and column 1-based, column and index in code points
	mcpPlace struct {
		Line   int `json:"line"`
		Column int `json:"column"`
		Index  int `json:"index"`
	}

	mcpSelection struct {
		From   mcpPlace `json:"from"`
		To     mcpPlace `json:"to"`
		Length int      `json:"length"`
		Text   string   `json:"text"`
	}

	// mcpLines are the visible lines, 1-based and inclusive, and the headings that start in them
	mcpLines struct {
		Top      int      `json:"top"`
		Bottom   int      `json:"bottom"`
		Headings []string `json:"headings,omitempty"`
	}
)

// displayName is the name of an account in answers, the caller's own marked "(you)"
func (t *mcpTools) displayName(id string) string {
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
	return name
}

// textLines is a text with the code point offsets of its line starts
type textLines struct {
	runes    []rune
	starts   []int
	headings []activity.Heading
}

func newTextLines(content string) *textLines {
	tl := &textLines{runes: []rune(content), starts: []int{0}, headings: activity.Headings(content)}
	for i, r := range tl.runes {
		if r == '\n' {
			tl.starts = append(tl.starts, i+1)
		}
	}
	return tl
}

// line is the 0-based line of an index
func (tl *textLines) line(index int) int {
	i, found := slices.BinarySearch(tl.starts, index)
	if found {
		return i
	}
	return i - 1
}

func (tl *textLines) place(index int) mcpPlace {
	l := tl.line(index)
	return mcpPlace{Line: l + 1, Column: index - tl.starts[l] + 1, Index: index}
}

// headingAt is the heading at or above a 0-based line, "" for none
func (tl *textLines) headingAt(line int) string {
	out := ""
	for _, h := range tl.headings {
		if h.Line > line {
			break
		}
		out = formatHeading(h)
	}
	return out
}

func formatHeading(h activity.Heading) string {
	return strings.Repeat("#", h.Level) + " " + h.Text
}

// viewerPositions describes where the people who have a page open are. It returns the text and
// revision they refer to, and ok false if nobody has the page open.
func (t *mcpTools) viewerPositions(d *wiki.Document) (string, string, []mcpViewer, bool, error) {
	content, revision, positions, err := t.rs.sync.ViewerPositions(d)
	if errors.Is(err, collab.ErrNobodyOpen) {
		return "", "", nil, false, nil
	}
	if err != nil {
		return "", "", nil, false, err
	}
	tl := newTextLines(content)
	now := time.Now()
	out := []mcpViewer{}
	for _, p := range positions[:min(len(positions), maxPresenceViewers)] {
		v := mcpViewer{Name: t.displayName(p.AccountID), Device: p.DeviceName, Updated: max(int(now.Sub(p.LastActive).Seconds()), 0),
			Approximate: p.Approximate}
		if !p.Known {
			v.Position = "unknown"
			out = append(out, v)
			continue
		}
		if len(p.Selections) > 0 {
			sel := p.Selections[0]
			cursor := tl.place(sel.Head)
			v.Cursor = &cursor
			v.Heading = tl.headingAt(cursor.Line - 1)
			if len(p.Selections) > 1 {
				v.Selections = len(p.Selections)
			}
			if sel.Anchor != sel.Head {
				from, to := min(sel.Anchor, sel.Head), max(sel.Anchor, sel.Head)
				text := string(tl.runes[from:to])
				if to-from > maxSelectionText {
					text = string(tl.runes[from:from+maxSelectionText]) + "…"
				}
				v.Selection = &mcpSelection{From: tl.place(from), To: tl.place(to), Length: to - from, Text: text}
			}
		}
		if vp := p.Viewport; vp != nil {
			top, bottom := tl.line(vp.Top), tl.line(vp.Bottom)
			lines := &mcpLines{Top: top + 1, Bottom: bottom + 1}
			for _, h := range tl.headings {
				if h.Line >= top && h.Line <= bottom && len(lines.Headings) < maxViewportHeadings {
					lines.Headings = append(lines.Headings, formatHeading(h))
				}
			}
			v.Viewport = lines
		}
		out = append(out, v)
	}
	return content, revision, out, true, nil
}

// formatViewerLine is the line of a viewer in read_document
func formatViewerLine(v mcpViewer) string {
	name := v.Name
	if v.Device != "" {
		name += " (" + v.Device + ")"
	}
	var parts []string
	switch {
	case v.Position != "":
		parts = append(parts, "position "+v.Position)
	case v.Cursor == nil:
		parts = append(parts, "no cursor")
	default:
		cursor := fmt.Sprintf("line %d col %d", v.Cursor.Line, v.Cursor.Column)
		if v.Heading != "" {
			cursor += fmt.Sprintf(" in %q", v.Heading)
		}
		parts = append(parts, cursor)
	}
	if s := v.Selection; s != nil {
		parts = append(parts, fmt.Sprintf("selected %q (%d chars)", s.Text, s.Length))
	}
	if v.Selections > 1 {
		parts = append(parts, fmt.Sprintf("%d selections", v.Selections))
	}
	if vp := v.Viewport; vp != nil {
		view := fmt.Sprintf("viewing lines %d-%d", vp.Top, vp.Bottom)
		if len(vp.Headings) > 0 {
			view += " (" + strings.Join(vp.Headings, ", ") + ")"
		}
		parts = append(parts, view)
	}
	if v.Approximate {
		parts = append(parts, "approximate")
	}
	parts = append(parts, fmt.Sprintf("%d s ago", v.Updated))
	return "- " + name + ": " + strings.Join(parts, ", ")
}

func (t *mcpTools) readPresence(_ context.Context, _ *mcp.CallToolRequest, in mcpDocumentRef) (*mcp.CallToolResult, *mcpPresence, error) {
	d, err := t.document(in.Document)
	if err != nil {
		return nil, nil, err
	}
	_, revision, viewers, ok, err := t.viewerPositions(d)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, &mcpPresence{Path: d.RelPath, Viewers: []mcpViewer{}}, nil
	}
	return nil, &mcpPresence{Path: d.RelPath, Revision: revision, Viewers: viewers}, nil
}

// readPresenceDescription is the description of read_presence
const readPresenceDescription = "Where the people who have a page open are right now: per person the cursor (line, column, " +
	"the heading it is under), the selected text, the visible lines and how many seconds ago they moved. Use it when " +
	"someone refers to \"here\", \"this\" or \"where I am\". read_document lists the same in its header. Empty if " +
	"nobody has the page open."
