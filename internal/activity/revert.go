package activity

import (
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"
)

// Reverting one change in a text that was edited since, line by line. Unlike a three-way merge
// of git, edits next to the change do not conflict: only the lines of the change itself must
// be as the change left them.

// lineRunes encodes the lines of texts (with their line ends) as runes, equal lines as equal runes
func lineRunes(texts ...[]string) [][]rune {
	ids := map[string]rune{}
	next := rune(firstLineRune)
	out := make([][]rune, len(texts))
	for t, lines := range texts {
		out[t] = make([]rune, len(lines))
		for i, l := range lines {
			id, ok := ids[l]
			if !ok {
				id = next
				next++
				ids[l] = id
			}
			out[t][i] = id
		}
	}
	return out
}

// linesOf splits a text after each line end. A missing line end at the end is added, so the
// last line equals the same line in the middle of a text.
func linesOf(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	for i := range lines {
		lines[i] += "\n"
	}
	return lines
}

// RevertChange undoes the change from before to after in current, a later version of after.
// It returns the new text, or the 0-based lines of current where the change conflicts with
// later edits: lines the change touched were changed again, or its place is gone.
func RevertChange(current, before, after string) (string, []int) {
	b, p, l := linesOf(after), linesOf(before), linesOf(current)
	r := lineRunes(b, p, l)

	// where each line of after is in current, -1 if it changed
	inCurrent := make([]int, len(b))
	bi, li := 0, 0
	for _, d := range dmp.DiffMainRunes(r[0], r[2], false) {
		n := len([]rune(d.Text))
		switch d.Type {
		case diffmatchpatch.DiffEqual:
			for range n {
				inCurrent[bi] = li
				bi++
				li++
			}
		case diffmatchpatch.DiffDelete:
			for range n {
				inCurrent[bi] = -1
				bi++
			}
		case diffmatchpatch.DiffInsert:
			li += n
		}
	}
	// near returns a line of current close to line i of after, for conflicts
	near := func(i int) int {
		for k := min(i, len(b)-1); k >= 0; k-- {
			if inCurrent[k] >= 0 {
				return inCurrent[k] + 1
			}
		}
		return 0
	}

	type block struct {
		from, to int // lines of current to replace
		with     []string
	}
	var blocks []block
	var conflicts []int
	// the blocks of the change, as replacements of lines of after by lines of before
	bi, pi := 0, 0
	start, startP := -1, 0
	flush := func() {
		if start < 0 {
			return
		}
		s, e, with := start, bi, p[startP:pi]
		start = -1
		if e > s {
			for k := s; k < e; k++ {
				if inCurrent[k] < 0 || (k > s && inCurrent[k] != inCurrent[k-1]+1) {
					conflicts = append(conflicts, near(k))
					return
				}
			}
			blocks = append(blocks, block{inCurrent[s], inCurrent[e-1] + 1, with})
			return
		}
		// lines to put back between lines s-1 and s of after
		switch {
		case s > 0 && inCurrent[s-1] >= 0:
			blocks = append(blocks, block{inCurrent[s-1] + 1, inCurrent[s-1] + 1, with})
		case s < len(b) && inCurrent[s] >= 0:
			blocks = append(blocks, block{inCurrent[s], inCurrent[s], with})
		case len(b) == 0:
			blocks = append(blocks, block{0, 0, with})
		default:
			conflicts = append(conflicts, near(s))
		}
	}
	for _, d := range dmp.DiffMainRunes(r[0], r[1], false) {
		n := len([]rune(d.Text))
		switch d.Type {
		case diffmatchpatch.DiffEqual:
			flush()
			bi += n
			pi += n
		case diffmatchpatch.DiffDelete:
			if start < 0 {
				start, startP = bi, pi
			}
			bi += n
		case diffmatchpatch.DiffInsert:
			if start < 0 {
				start, startP = bi, pi
			}
			pi += n
		}
	}
	flush()
	if len(conflicts) > 0 {
		return "", conflicts
	}
	out := l
	for i := len(blocks) - 1; i >= 0; i-- {
		k := blocks[i]
		out = append(append(append([]string{}, out[:k.from]...), k.with...), out[k.to:]...)
	}
	text := strings.Join(out, "")
	if current != "" && !strings.HasSuffix(current, "\n") {
		text = strings.TrimSuffix(text, "\n")
	}
	return text, nil
}
