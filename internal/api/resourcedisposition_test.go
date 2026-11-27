package api

import "testing"

func TestInlineDisposition(t *testing.T) {
	for name, want := range map[string]string{
		"logo.svg":       `inline; filename="logo.svg"; filename*=UTF-8''logo.svg`,
		"Größe 100%.png": `inline; filename="Gr__e 100_.png"; filename*=UTF-8''Gr%C3%B6%C3%9Fe%20100%25.png`,
		`a"b\c.txt`:      `inline; filename="a_b_c.txt"; filename*=UTF-8''a%22b%5Cc.txt`,
		"it's (1).svg":   `inline; filename="it's (1).svg"; filename*=UTF-8''it%27s%20%281%29.svg`,
	} {
		if got := inlineDisposition(name); got != want {
			t.Errorf("%q: got %s, want %s", name, got, want)
		}
	}
}
