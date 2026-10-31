package backend

import (
	"unicode/utf8"

	automerge "github.com/automerge/automerge-go"
	"github.com/sergi/go-diff/diffmatchpatch"
)

var dmp = diffmatchpatch.New()

// spliceDiff changes text from oldText to newText with the smallest splices diff-match-patch finds.
// Positions are Unicode code points, as automerge-go expects. text must currently equal oldText.
func spliceDiff(text *automerge.Text, oldText string, newText string) error {
	if oldText == newText {
		return nil
	}
	diffs := dmp.DiffMain(oldText, newText, false)
	diffs = dmp.DiffCleanupSemantic(diffs)

	pos := 0
	for _, d := range diffs {
		n := utf8.RuneCountInString(d.Text)
		switch d.Type {
		case diffmatchpatch.DiffEqual:
			pos += n
		case diffmatchpatch.DiffDelete:
			if err := text.Delete(pos, n); err != nil {
				return err
			}
		case diffmatchpatch.DiffInsert:
			if err := text.Insert(pos, d.Text); err != nil {
				return err
			}
			pos += n
		}
	}
	return nil
}
