package collab

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Hints for edits whose old text does not occur: agents often get whitespace wrong (a trailing
// newline, CRLF, trailing spaces) or quote a line that changed since they read it. The hint
// saves them a round trip to read the document again.

const (
	// maxHintLine caps the length of a line in a hint
	maxHintLine = 200
	// maxHintCompare caps the runes compared per line when the closest line is searched
	maxHintCompare = 300
)

// whitespaceVariants are changes to old that may make it match, with their description
var whitespaceVariants = []struct {
	fix  func(string) string
	what string
}{
	{func(s string) string { return strings.TrimRight(s, " \t\r\n") }, "without its trailing whitespace or newline"},
	{func(s string) string { return strings.TrimLeft(s, " \t\r\n") }, "without its leading whitespace or newline"},
	{strings.TrimSpace, "without its leading and trailing whitespace"},
}

// normalizeLineEnds makes \r\n \n and removes spaces and tabs at the ends of lines. It is
// applied to old and the text: either may have the extra whitespace.
func normalizeLineEnds(s string) string {
	return trimLineEnds(strings.ReplaceAll(s, "\r\n", "\n"))
}

// trimLineEnds removes spaces and tabs at the end of every line
func trimLineEnds(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.Join(lines, "\n")
}

// notFoundHint explains why old does not occur in text: a whitespace variant of old that
// occurs, else the line of the text closest to the first line of old. "" if nothing helps.
func notFoundHint(text, old string) string {
	for _, v := range whitespaceVariants {
		fixed := v.fix(old)
		if fixed == "" || fixed == old {
			continue
		}
		if n := strings.Count(text, fixed); n > 0 {
			unique := "once"
			if n > 1 {
				unique = fmt.Sprintf("%d times (add context to make it unique)", n)
			}
			return fmt.Sprintf("old occurs %s %s", v.what, unique)
		}
	}
	if fixed := normalizeLineEnds(old); strings.TrimSpace(fixed) != "" {
		normalized := normalizeLineEnds(text)
		for _, candidate := range []string{fixed, strings.TrimSpace(fixed)} {
			if strings.Contains(normalized, candidate) {
				return "old occurs if line endings (\\r\\n) and spaces at line ends are ignored: copy the exact text, read_document or GET content shows it"
			}
		}
	}
	first := firstLine(old)
	if first == "" {
		return ""
	}
	line, number, ok := closestLine(text, first)
	if !ok {
		return ""
	}
	return fmt.Sprintf("the closest line to the first line of old is line %d: %q", number, clip(line))
}

// firstLine is the first line of s that is not blank, trimmed
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			return t
		}
	}
	return ""
}

// closestLine finds the line of text with the smallest edit distance to want. It reports
// false when no line is similar: the distance must be below half the length of want.
func closestLine(text, want string) (string, int, bool) {
	w := []rune(want)
	if len(w) > maxHintCompare {
		w = w[:maxHintCompare]
	}
	best, bestNumber, bestDist := "", 0, -1
	for i, l := range strings.Split(text, "\n") {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		r := []rune(t)
		if len(r) > maxHintCompare {
			r = r[:maxHintCompare]
		}
		// the distance is at least the difference of the lengths
		if bestDist >= 0 && abs(len(r)-len(w)) >= bestDist {
			continue
		}
		if d := editDistance(w, r); bestDist < 0 || d < bestDist {
			best, bestNumber, bestDist = l, i+1, d
		}
	}
	if bestDist < 0 || bestDist*2 >= len(w) {
		return "", 0, false
	}
	return best, bestNumber, true
}

// editDistance is the Levenshtein distance of two rune slices
func editDistance(a, b []rune) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// clip shortens a line for a hint
func clip(s string) string {
	s = strings.TrimRightFunc(s, unicode.IsSpace)
	if utf8.RuneCountInString(s) <= maxHintLine {
		return s
	}
	return string([]rune(s)[:maxHintLine]) + "…"
}
