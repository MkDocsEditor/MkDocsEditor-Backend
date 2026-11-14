package collab

import (
	"strings"
	"testing"
)

func TestNotFoundHint(t *testing.T) {
	text := "# Setup\n\nInstall the tool.\r\nRun it with --fast.  \nDone."
	for _, tc := range []struct{ old, want string }{
		{"Install the tool.\r\nRun", ""}, // occurs: no hint needed, not called
		{"# Setup\n\nInstall\n", "without its trailing whitespace or newline once"},
		{"\n\nDone.", "without its leading whitespace or newline once"},
		{"Install the tool.\nRun", "line endings"},
		{"Run it with --fast.\nDone.", "line endings"},
		{"Run it with --slow.", `line 4: "Run it with --fast."`},
		{"Something else entirely", ""},
	} {
		if strings.Contains(text, tc.old) {
			continue
		}
		got := notFoundHint(text, tc.old)
		if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
			t.Errorf("hint(%q) = %q, want %q", tc.old, got, tc.want)
		}
	}
	// the hint reaches the error of ApplyEdits
	_, err := placeEdits("a line\n", []TextEdit{{Old: "a line\n\n", New: "x"}})
	if err == nil || !strings.Contains(err.Error(), "trailing whitespace") {
		t.Errorf("error = %v", err)
	}
}
