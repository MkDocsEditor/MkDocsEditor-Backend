package collab

import (
	"unicode/utf8"

	automerge "github.com/automerge/automerge-go"
	"github.com/sergi/go-diff/diffmatchpatch"
)

var dmp = diffmatchpatch.New()

// SpliceDiff changes text from oldText to newText with the smallest splices diff-match-patch finds.
// Positions are Unicode code points, as automerge-go expects. text must currently equal oldText.
func SpliceDiff(text *automerge.Text, oldText string, newText string) error {
	_, err := spliceDiff(text, oldText, newText)
	return err
}

// spliceDiff is SpliceDiff, and returns the splices it applied, in order
func spliceDiff(text *automerge.Text, oldText string, newText string) ([]splice, error) {
	if oldText == newText {
		return nil, nil
	}
	diffs := dmp.DiffMain(oldText, newText, false)
	diffs = dmp.DiffCleanupSemantic(diffs)

	var out []splice
	pos := 0
	for _, d := range diffs {
		n := utf8.RuneCountInString(d.Text)
		switch d.Type {
		case diffmatchpatch.DiffEqual:
			pos += n
		case diffmatchpatch.DiffDelete:
			if err := text.Delete(pos, n); err != nil {
				return nil, err
			}
			out = append(out, splice{pos: pos, del: n})
		case diffmatchpatch.DiffInsert:
			if err := text.Insert(pos, d.Text); err != nil {
				return nil, err
			}
			out = append(out, splice{pos: pos, insert: d.Text})
			pos += n
		}
	}
	return out, nil
}

// focusOfSplices is where a change of splices leaves the agent, in the text after all of them:
// the end of the last inserted range in the order of the splices, or the place of the last
// splice if none inserted. false for no splices.
func focusOfSplices(splices []splice) (int, bool) {
	if len(splices) == 0 {
		return 0, false
	}
	last := len(splices) - 1
	at := splices[last].pos
	for i := len(splices) - 1; i >= 0; i-- {
		if splices[i].insert != "" {
			last = i
			at = splices[i].pos + utf8.RuneCountInString(splices[i].insert)
			break
		}
	}
	// the splices after it move it
	for _, sp := range splices[last+1:] {
		switch {
		case sp.pos >= at:
		case sp.pos+sp.del <= at:
			at += utf8.RuneCountInString(sp.insert) - sp.del
		default:
			at = sp.pos + utf8.RuneCountInString(sp.insert)
		}
	}
	return at, true
}
