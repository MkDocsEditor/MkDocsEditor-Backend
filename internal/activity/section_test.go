package activity

import "testing"

func TestReplaceSection(t *testing.T) {
	tests := []struct {
		name, current, old, heading string
		want, missing               string
	}{
		{"middle in both", "# T\n\n## A\na2\n\n## B\nb\n", "# T\n\n## A\na1\n\n## B\nb\n", "A",
			"# T\n\n## A\na1\n\n## B\nb\n", ""},
		{"last at the revision, middle now", "# T\n\n## A\na2\n\n## B\nb\n", "# T\n\n## A\na1\n", "A",
			"# T\n\n## A\na1\n\n## B\nb\n", ""},
		{"last at the revision without line end", "# T\n\n## A\na2\n\n## B\nb\n", "# T\n\n## A\na1", "A",
			"# T\n\n## A\na1\n\n## B\nb\n", ""},
		{"middle at the revision, last now", "## A\na2\n", "## A\na1\n\n## B\nb\n", "A",
			"## A\na1\n", ""},
		{"last now without line end", "## A\na2", "## A\na1\n\n## B\n", "A",
			"## A\na1", ""},
		{"both last without line end", "x\n## A\na2", "## A\na1", "A",
			"x\n## A\na1", ""},
		{"next heading of a higher level", "## A\na2\n\n# Top\nt\n", "## A\na1\n### Sub\ns\n\n## B\n", "A",
			"## A\na1\n### Sub\ns\n\n# Top\nt\n", ""},
		{"subsection now, at the revision too", "## A\na2\n### Sub\ns2\n\n## B\n", "## A\na1\n### Sub\ns1\n## B\n", "A",
			"## A\na1\n### Sub\ns1\n\n## B\n", ""},
		{"blank lines of the revision give way to the current ones", "## A\na2\n## B\n", "## A\na1\n\n\n## B\n", "A",
			"## A\na1\n## B\n", ""},
		{"second occurrence", "## A\nx\n\n## A\ny2\n\n## C\n", "## A\nx\n## A\ny1\n", "A",
			"## A\nx\n\n## A\ny1\n\n## C\n", ""},
		{"missing at the revision", "## A\n", "## B\n", "A", "", "old"},
		{"missing now", "## B\n", "## A\n", "A", "", "current"},
	}
	for _, tt := range tests {
		occurrence := 1
		if tt.name == "second occurrence" {
			occurrence = 2
		}
		got, missing := ReplaceSection(tt.current, tt.old, tt.heading, occurrence)
		if got != tt.want || missing != tt.missing {
			t.Errorf("%s: got %q %q, want %q %q", tt.name, got, missing, tt.want, tt.missing)
		}
	}
}
