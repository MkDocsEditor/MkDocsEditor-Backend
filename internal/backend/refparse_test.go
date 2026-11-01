package backend

import (
	"reflect"
	"testing"
)

func TestParseReferences(t *testing.T) {
	text := "---\n" +
		"image: front/matter.png\n" +
		"---\n" +
		"# Titel\n" +
		"\n" +
		"Siehe [Einrichtung](guide/setup.md) und ![Logo](img/logo.png \"Logo\").\n" +
		"[![Bild](img/a.png)](seite.md#oben) und [leer]() und [extern](https://example.org)\n" +
		"Mit `[code](nicht.md)` und ``doppelt `[x](y.md)` code`` und \\[kein](link.md).\n" +
		"<img src=\"img/b.png\" alt=\"b\"> <a href='a.md'>a</a> <video src=c.mp4 data-src=\"no.mp4\">\n" +
		"<!-- [auskommentiert](weg.md)\n" +
		"[noch](weg2.md) -->[danach](da.md)\n" +
		"```markdown\n" +
		"[im Code](code.md)\n" +
		"```\n" +
		"!!! note\n" +
		"    Eingerückt: [Admonition](adm.md)\n" +
		"\n" +
		"~~~~\n" +
		"[tilde](tilde.md)\n" +
		"~~~\n" +
		"[immer noch code](code2.md)\n" +
		"~~~~\n" +
		"[ref]: <mit leerzeichen.md> \"Titel\"\n" +
		"  [ref2]: ../oben.md\n" +
		"[^1]: [fußnote](fn.md)\n" +
		"[Klammern](datei_(1).md) [spitz](<a b.md>) [titel](t.md 'x') [paren](p.md (y))\n" +
		"Umlaute: [Käse](Käse.md)\n"
	refs := parseReferences(text)

	type ref struct {
		Kind string
		Line int
		Col  int
		Raw  string
	}
	var got []ref
	for _, r := range refs {
		if text[r.Start:r.End] != r.Raw {
			t.Errorf("offsets of %q point to %q", r.Raw, text[r.Start:r.End])
		}
		got = append(got, ref{r.Kind, r.Line, r.Column, r.Raw})
	}
	want := []ref{
		{RefKindLink, 6, 21, "guide/setup.md"},
		{RefKindImage, 6, 49, "img/logo.png"},
		{RefKindImage, 7, 10, "img/a.png"},
		{RefKindLink, 7, 22, "seite.md#oben"},
		{RefKindLink, 7, 63, "https://example.org"},
		{RefKindHTML, 9, 11, "img/b.png"},
		{RefKindHTML, 9, 40, "a.md"},
		{RefKindHTML, 9, 63, "c.mp4"},
		{RefKindLink, 11, 29, "da.md"},
		{RefKindLink, 16, 30, "adm.md"},
		{RefKindDefinition, 23, 9, "mit leerzeichen.md"},
		{RefKindDefinition, 24, 11, "../oben.md"},
		{RefKindLink, 25, 17, "fn.md"},
		{RefKindLink, 26, 12, "datei_(1).md"},
		{RefKindLink, 26, 35, "a b.md"},
		{RefKindLink, 26, 52, "t.md"},
		{RefKindLink, 26, 70, "p.md"},
		{RefKindLink, 27, 17, "Käse.md"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got\n%v\nwant\n%v", got, want)
	}
}

func TestParseReferencesCRLF(t *testing.T) {
	refs := parseReferences("[a](a.md)\r\n[b]: b.md\r\n")
	if len(refs) != 2 || refs[0].Raw != "a.md" || refs[1].Raw != "b.md" {
		t.Errorf("got %+v", refs)
	}
}
