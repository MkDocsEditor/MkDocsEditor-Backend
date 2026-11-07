package activity

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"slices"
	"strings"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	"github.com/sergi/go-diff/diffmatchpatch"
)

const (
	// maxHeadings is the number of headings a summary names at most
	maxHeadings = 5
	// maxRanges is the number of ranges a summary has at most
	maxRanges = 20
	// mergeGap is the number of unchanged lines between two changes that still makes them one
	// range
	mergeGap = 3
	// textHashLength is the number of hex digits of the text hash
	textHashLength = 16
)

// Summary tells what an edit changed
type Summary struct {
	LinesAdded   int `json:"linesAdded"`
	LinesRemoved int `json:"linesRemoved"`
	// Headings are the texts of the headings above the changed lines, in document order
	Headings []string `json:"headings"`
	// TextHash identifies the text after the edit, see docs/dashboard.md. Empty for entries
	// recorded before it existed.
	TextHash string `json:"textHash,omitempty"`
	// Ranges are the changed lines of the text after the edit, in text order. nil for entries
	// recorded before they existed.
	Ranges []Range `json:"ranges"`
}

// Range is a block of added or changed lines, or with Count 0 the place of removed lines
type Range struct {
	// Line is 1-based. With Count 0 the removed lines were before it.
	Line  int `json:"line"`
	Count int `json:"count"`
	// Heading is the nearest heading at or above Line, nil if there is none
	Heading *string `json:"heading"`
	// HeadingOccurrence counts the headings with the same text from the top, 1 for the first
	HeadingOccurrence int `json:"headingOccurrence"`
	// Offset is Line minus the line of the heading, or Line - 1 without a heading
	Offset int `json:"offset"`
}

// TextHash is the first hex digits of SHA-256 over the text
func TextHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:textHashLength]
}

var (
	dmp = diffmatchpatch.New()
	// headingPattern matches an ATX heading: up to three spaces, one to six #, the text and
	// optional closing #
	headingPattern = regexp.MustCompile(`^ {0,3}#{1,6}(?:[ \t]+(.*?))?(?:[ \t]+#+)?[ \t]*$`)
	fencePattern   = regexp.MustCompile("^ {0,3}(```|~~~)")
)

// Summarize compares two texts line by line
func Summarize(before string, after string) Summary {
	headingOf := headingsByLine(after)
	var touched []int
	touch := func(line int) {
		if line >= 0 && line < len(headingOf) && headingOf[line] >= 0 {
			touched = append(touched, headingOf[line])
		}
	}
	s := Summary{Headings: []string{}, TextHash: TextHash(after), Ranges: []Range{}}
	// hunks are [start, end) on the side of after, 0-based
	var hunks [][2]int
	hunk := func(start, end int) {
		if n := len(hunks); n > 0 && start-hunks[n-1][1] <= mergeGap {
			hunks[n-1][1] = max(hunks[n-1][1], end)
			return
		}
		hunks = append(hunks, [2]int{start, end})
	}
	line := 0
	for _, d := range lineDiff(before, after) {
		n := len([]rune(d.Text))
		switch d.Type {
		case diffmatchpatch.DiffEqual:
			line += n
		case diffmatchpatch.DiffInsert:
			s.LinesAdded += n
			for i := line; i < line+n; i++ {
				touch(i)
			}
			hunk(line, line+n)
			line += n
		case diffmatchpatch.DiffDelete:
			s.LinesRemoved += n
			// the removed lines were below the line before them
			touch(max(line-1, 0))
			hunk(line, line)
		}
	}
	s.Ranges = ranges(hunks, after)
	slices.Sort(touched)
	touched = slices.Compact(touched)
	titles := headingTexts(after)
	for _, h := range touched {
		if len(s.Headings) == maxHeadings {
			break
		}
		s.Headings = append(s.Headings, titles[h])
	}
	return s
}

// ranges turns hunks into ranges with their heading anchors. Beyond maxRanges the two
// neighbours with the smallest gap merge until maxRanges are left.
func ranges(hunks [][2]int, after string) []Range {
	for len(hunks) > maxRanges {
		best := 0
		for i := 1; i+1 < len(hunks); i++ {
			if hunks[i+1][0]-hunks[i][1] < hunks[best+1][0]-hunks[best][1] {
				best = i
			}
		}
		hunks[best][1] = hunks[best+1][1]
		hunks = slices.Delete(hunks, best+1, best+2)
	}
	headingOf := headingsByLine(after)
	titles := headingTexts(after)
	out := make([]Range, 0, len(hunks))
	for _, h := range hunks {
		r := Range{Line: h[0] + 1, Count: h[1] - h[0], Offset: h[0]}
		// removed lines at the end belong to the heading of the last line
		at := min(h[0], len(headingOf)-1)
		if at >= 0 && headingOf[at] >= 0 {
			hl := headingOf[at]
			title := titles[hl]
			r.Heading = &title
			r.Offset = h[0] - hl
			for l, t := range titles {
				if t == title && l <= hl {
					r.HeadingOccurrence++
				}
			}
		}
		out = append(out, r)
	}
	return out
}

// firstLineRune is the rune of the first distinct line. Runes from there on are no surrogates
// and leave room for a million distinct lines.
const firstLineRune = 0x10000

// lineDiff compares two texts line by line. Each rune of the texts of the result is one line.
// A missing line break at the end does not count as a change of the last line.
// DiffLinesToChars of go-diff is not used: in v1.3.1 it encodes lines as decimal numbers that
// the diff then splits.
func lineDiff(before string, after string) []diffmatchpatch.Diff {
	ids := map[string]rune{}
	next := rune(firstLineRune)
	encode := func(text string) []rune {
		lines := splitLines(text)
		out := make([]rune, len(lines))
		for i, l := range lines {
			id, ok := ids[l]
			if !ok {
				id = next
				next++
				ids[l] = id
			}
			out[i] = id
		}
		return out
	}
	a, b := encode(before), encode(after)
	return dmp.DiffMainRunes(a, b, false)
}

// splitLines splits a text into lines without their line breaks
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

// headingTexts maps the line numbers of headings to their texts. Lines in fenced code blocks
// and in the front matter are no headings.
func headingTexts(text string) map[int]string {
	out := map[int]string{}
	fence := ""
	frontMatter := wiki.FrontMatterLines(text)
	for i, l := range splitLines(text) {
		if i < frontMatter {
			continue
		}
		if m := fencePattern.FindStringSubmatch(l); m != nil {
			switch fence {
			case "":
				fence = m[1]
			case m[1]:
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		if m := headingPattern.FindStringSubmatch(strings.TrimRight(l, "\r")); m != nil {
			if title := strings.TrimSpace(m[1]); title != "" {
				out[i] = title
			}
		}
	}
	return out
}

// headingsByLine returns for each line the line number of the heading it is below, -1 for the
// lines above the first heading
func headingsByLine(text string) []int {
	titles := headingTexts(text)
	lines := splitLines(text)
	out := make([]int, len(lines))
	current := -1
	for i := range lines {
		if _, ok := titles[i]; ok {
			current = i
		}
		out[i] = current
	}
	return out
}
