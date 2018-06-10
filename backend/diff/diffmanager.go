package diff

import (
	"github.com/sergi/go-diff/diffmatchpatch"
)

var (
	dmp = diffmatchpatch.New()
)

// creates a diff of text1 to text2 and returns a string representation
func CreateDiff(text1 string, text2 string, something bool) (diff string) {
	diffs := dmp.DiffMain(text1, text2, false)

	return dmp.DiffText1(diffs)
}

// applies a list of patches in the form of a string to a given text
func ApplyPatch(textToPatch string, patchesText string) (patched string, err error) {
	p, err := dmp.PatchFromText(patchesText)
	patched, _ = dmp.PatchApply(p, textToPatch)
	return patched, err
}
