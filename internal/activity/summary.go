package activity

import (
	"regexp"
	"slices"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"
)

// maxHeadings is the number of headings a summary names at most
const maxHeadings = 5

// Summary tells what an edit changed
type Summary struct {
	LinesAdded   int `json:"linesAdded"`
	LinesRemoved int `json:"linesRemoved"`
	// Headings are the texts of the headings above the changed lines, in document order
	Headings []string `json:"headings"`
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
	s := Summary{Headings: []string{}}
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
			line += n
		case diffmatchpatch.DiffDelete:
			s.LinesRemoved += n
			// the removed lines were below the line before them
			touch(max(line-1, 0))
		}
	}
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
// are no headings.
func headingTexts(text string) map[int]string {
	out := map[int]string{}
	fence := ""
	for i, l := range splitLines(text) {
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
