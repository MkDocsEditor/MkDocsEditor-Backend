package activity

import (
	"encoding/json"
	"testing"
)

func TestHunks(t *testing.T) {
	before := "# Title\n\nIntro\n\n## Setup\n\na\nb\nc\nd\ne\nf\ng\nh\ni\nj\n\n## Setup\n\nold line\n"
	after := "# Title\n\nIntro\n\n## Setup\n\na\nb\nC\nd\ne\nf\ng\nh\ni\nj\n\n## Setup\n\nnew line\nmore\n"
	hunks := Hunks(before, after)
	if len(hunks) != 2 {
		t.Fatalf("%d hunks: %+v", len(hunks), hunks)
	}
	h := hunks[0]
	if h.Heading == nil || *h.Heading != "Setup" || h.HeadingOccurrence != 1 || h.OldStart != 6 || h.OldLines != 7 || h.NewStart != 6 || h.NewLines != 7 {
		t.Errorf("first hunk %+v heading %v", h, h.Heading)
	}
	if h.Offset != 1 {
		t.Errorf("offset %d, want 1: the hunk starts a line below the heading", h.Offset)
	}
	if h.Lines[3] != (DiffLine{OpRemove, "c"}) || h.Lines[4] != (DiffLine{OpAdd, "C"}) {
		t.Errorf("first hunk lines %+v", h.Lines)
	}
	h = hunks[1]
	if h.Heading == nil || *h.Heading != "Setup" || h.HeadingOccurrence != 2 || h.OldLines != 4 || h.NewLines != 5 {
		data, _ := json.Marshal(h)
		t.Errorf("second hunk %s", data)
	}

	// a new file: one hunk of additions, old side empty
	hunks = Hunks("", "# New\ntext\n")
	if len(hunks) != 1 || hunks[0].OldStart != 0 || hunks[0].OldLines != 0 || hunks[0].NewStart != 1 || hunks[0].NewLines != 2 {
		t.Errorf("new file %+v", hunks)
	}
	// a removal at the end belongs to the heading above
	hunks = Hunks("# A\nx\n# B\ny\nz\n", "# A\nx\n# B\ny\n")
	if len(hunks) != 1 || *hunks[0].Heading != "B" || hunks[0].NewLines != 3 || hunks[0].OldLines != 4 {
		t.Errorf("removal %+v", hunks)
	}
	if got := Hunks("same\n", "same"); len(got) != 0 {
		t.Errorf("no change %+v", got)
	}
	// lines in a code block are no headings
	hunks = Hunks("# Real\n```\n# not\n```\nx\n", "# Real\n```\n# not\n```\ny\n")
	if *hunks[0].Heading != "Real" {
		t.Errorf("fence %v", *hunks[0].Heading)
	}
}
