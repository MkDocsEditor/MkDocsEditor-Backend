package activity

import (
	"github.com/sergi/go-diff/diffmatchpatch"
)

// Structured line diffs for the revision history (GET /document/{id}/history/diff/?format=json),
// with the heading anchors of the feed, see docs/sync-protocol.md.

// DiffContext is the number of unchanged lines around a change in a hunk
const DiffContext = 3

// Diff line operations
const (
	OpContext = " "
	OpRemove  = "-"
	OpAdd     = "+"
)

// DiffLine is a line of a hunk
type DiffLine struct {
	Op   string `json:"op"`
	Text string `json:"text"`
}

// Hunk is a block of changed lines with context, like a hunk of a unified diff
type Hunk struct {
	// Heading is the nearest heading of the text after the change at or above the first
	// changed line, nil if there is none
	Heading *string `json:"heading"`
	// HeadingOccurrence counts the headings with the same text from the top, 1 for the first
	HeadingOccurrence int `json:"headingOccurrence"`
	// Offset is NewStart minus the line of Heading, or NewStart - 1 without a heading, like
	// Range.Offset of the feed. It is negative when the context starts above the heading.
	Offset int `json:"offset"`
	// OldStart and NewStart are 1-based line numbers of the first line of the hunk in the
	// text before and after; with 0 lines on a side they are the line before the hunk
	OldStart int        `json:"oldStart"`
	OldLines int        `json:"oldLines"`
	NewStart int        `json:"newStart"`
	NewLines int        `json:"newLines"`
	Lines    []DiffLine `json:"lines"`
}

// Hunks compares two texts line by line and returns the changes with DiffContext lines of
// context. Changes closer than twice the context share a hunk.
func Hunks(before string, after string) []Hunk {
	a, b := splitLines(before), splitLines(after)
	// one entry per line of the whole diff
	type row struct {
		op         string
		text       string
		oldN, newN int // 0-based line numbers after this row on each side
	}
	var rows []row
	oldN, newN := 0, 0
	for _, d := range lineDiff(before, after) {
		n := len([]rune(d.Text))
		for range n {
			switch d.Type {
			case diffmatchpatch.DiffEqual:
				rows = append(rows, row{OpContext, b[newN], oldN, newN})
				oldN++
				newN++
			case diffmatchpatch.DiffDelete:
				rows = append(rows, row{OpRemove, a[oldN], oldN, newN})
				oldN++
			case diffmatchpatch.DiffInsert:
				rows = append(rows, row{OpAdd, b[newN], oldN, newN})
				newN++
			}
		}
	}
	// ranges of rows [start, end) that become hunks
	var spans [][2]int
	for i, r := range rows {
		if r.op == OpContext {
			continue
		}
		start, end := max(i-DiffContext, 0), min(i+1+DiffContext, len(rows))
		if n := len(spans); n > 0 && start <= spans[n-1][1] {
			spans[n-1][1] = max(spans[n-1][1], end)
			continue
		}
		spans = append(spans, [2]int{start, end})
	}
	headingOf := headingsByLine(after)
	titles := headingTexts(after)
	out := make([]Hunk, 0, len(spans))
	for _, s := range spans {
		h := Hunk{Lines: make([]DiffLine, 0, s[1]-s[0])}
		first := rows[s[0]]
		h.OldStart, h.NewStart = first.oldN+1, first.newN+1
		changed := -1
		for _, r := range rows[s[0]:s[1]] {
			h.Lines = append(h.Lines, DiffLine{Op: r.op, Text: r.text})
			if r.op != OpAdd {
				h.OldLines++
			}
			if r.op != OpRemove {
				h.NewLines++
			}
			if changed < 0 && r.op != OpContext {
				// the line of the text after at the first change; removed lines belong
				// to the line above them
				changed = r.newN
				if r.op == OpRemove {
					changed = r.newN - 1
				}
			}
		}
		if h.OldLines == 0 {
			h.OldStart--
		}
		if h.NewLines == 0 {
			h.NewStart--
		}
		h.Offset = h.NewStart - 1
		at := min(max(changed, 0), len(headingOf)-1)
		if at >= 0 && headingOf[at] >= 0 {
			title := titles[headingOf[at]]
			h.Heading = &title
			h.Offset = h.NewStart - (headingOf[at] + 1)
			for l, t := range titles {
				if t == title && l <= headingOf[at] {
					h.HeadingOccurrence++
				}
			}
		}
		out = append(out, h)
	}
	return out
}
