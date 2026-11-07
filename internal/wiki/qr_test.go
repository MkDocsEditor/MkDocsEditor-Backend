package wiki

import (
	"fmt"
	"strings"
	"testing"
)

func TestParseQRCodes(t *testing.T) {
	for _, tc := range []struct {
		front   string
		codes   string
		invalid string
	}{
		{"qr: Waschmaschine", "waschmaschine", ""},
		{"qr: 42", "42", ""},
		{"qr: [waschmaschine, trockner, WASCHMASCHINE]", "waschmaschine,trockner", ""},
		{"qr: [{key: waschmaschine, label: Keller}, trockner]", "waschmaschine=Keller,trockner", ""},
		{"qr: {key: heizung, label: Flur}", "heizung=Flur", ""},
		{"qr: [-bad, \"with space\", ok-1, {label: x}]", "ok-1", "-bad|with space|map[label:x]"},
		{"qr: " + strings.Repeat("a", 65), "", strings.Repeat("a", 65)},
		{"title: x", "", ""},
	} {
		meta, _ := ParsePageMeta("---\n" + tc.front + "\n---\n")
		var codes []string
		for _, c := range meta.QR {
			s := c.Key
			if c.Label != nil {
				s += "=" + *c.Label
			}
			codes = append(codes, s)
		}
		if got := strings.Join(codes, ","); got != tc.codes || meta.QR == nil {
			t.Errorf("%s: codes = %q, want %q", tc.front, got, tc.codes)
		}
		if got := strings.Join(meta.QRInvalid, "|"); got != tc.invalid {
			t.Errorf("%s: invalid = %q, want %q", tc.front, got, tc.invalid)
		}
	}
}

func TestQRCodesOfTree(t *testing.T) {
	doc := func(path string, keys ...string) *Document {
		d := &Document{RelPath: path, Meta: PageMeta{QR: []QRCode{}}}
		for _, k := range keys {
			d.Meta.QR = append(d.Meta.QR, QRCode{Key: k})
		}
		return d
	}
	root := &Section{Documents: []*Document{doc("b.md", "x"), doc("a.md", "x", "y")},
		Subsections: []*Section{{Documents: []*Document{doc("s/c.md", "z")}}}}
	root.Documents[0].Meta.QRInvalid = []string{"-no"}
	byKey, invalid := QRCodes(root)
	got := fmt.Sprintf("%d %s %d %s %d", len(byKey["x"]), byKey["x"][0].Document.RelPath, len(byKey["y"]), byKey["z"][0].Document.RelPath, len(invalid))
	if got != "2 a.md 1 s/c.md 1" {
		t.Errorf("got %s", got)
	}
	if byKey, _ := QRCodes(nil); len(byKey) != 0 {
		t.Errorf("nil tree: %v", byKey)
	}
}
