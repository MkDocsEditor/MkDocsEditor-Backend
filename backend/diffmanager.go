package backend

import (
	"github.com/sergi/go-diff/diffmatchpatch"
)

var (
	dmp = diffmatchpatch.New()
)

// applies a list of patches in the form of a string to a given text
func ApplyPatch(textToPatch string, patchesText string) (patched string, err error) {
	p, err := dmp.PatchFromText(patchesText)
	patched, _ = dmp.PatchApply(p, textToPatch)
	return patched, err
}

// detect diff and create patch from diff
func CreatePatch(oldText string, newText string) (patches string, err error) {
	diffs := dmp.DiffMain(oldText, newText, true)
	patchArray := dmp.PatchMake(diffs)
	patches = dmp.PatchToText(patchArray)
	return patches, nil
}
