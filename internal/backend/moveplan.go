package backend

import (
	"iter"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	automerge "github.com/automerge/automerge-go"
	"gopkg.in/yaml.v3"
)

// textEdit replaces text[start:end] (byte offsets) with replacement
type textEdit struct {
	start       int
	end         int
	replacement string
	// line is the 1-based line of start
	line int
}

// LineEdit is a changed line, before and after a move
type LineEdit struct {
	Line   int    `json:"line"`
	Before string `json:"before"`
	After  string `json:"after"`
}

// ConfigEdit is a changed line of mkdocs.yml
type ConfigEdit struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Before string `json:"before"`
	After  string `json:"after"`
}

// referenceEdits computes the edits of the references of a document text for a move, see
// docs/references.md section 6. oldSource and newSource are the document's paths before and
// after the move, lookup is the tree before the move, mapPath maps a path through the move.
func referenceEdits(text string, oldSource string, newSource string, lookup *treeLookup, mapPath func(string) string) []textEdit {
	var edits []textEdit
	for _, r := range parseReferences(text) {
		raw := r.destinationText()
		d, ok := parseDestination(oldSource, raw)
		if !ok || d.outside || lookup.resolve(d.path) == nil {
			// external, same-document and broken references stay as they are
			continue
		}
		newTarget := mapPath(d.path)
		if newSource == oldSource && newTarget == d.path {
			continue
		}
		// still pointing to the same place from the new location?
		if same, ok := parseDestination(newSource, raw); ok && !same.outside && same.path == newTarget {
			continue
		}
		replacement := r.escapeReplacement(rewriteDestination(d, newSource, newTarget))
		edits = append(edits, textEdit{start: r.Start, end: r.End, replacement: replacement, line: r.Line})
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	return edits
}

// applyEdits returns the text with the edits applied. Edits are sorted and do not overlap.
func applyEdits(text string, edits []textEdit) string {
	var b strings.Builder
	last := 0
	for _, e := range edits {
		b.WriteString(text[last:e.start])
		b.WriteString(e.replacement)
		last = e.end
	}
	b.WriteString(text[last:])
	return b.String()
}

// lineEdits describes the changed lines of a text, with line numbers of the text before
func lineEdits(text string, edits []textEdit) []LineEdit {
	var out []LineEdit
	for i := 0; i < len(edits); {
		lineStart := strings.LastIndexByte(text[:edits[i].start], '\n') + 1
		lineEnd := len(text)
		if n := strings.IndexByte(text[edits[i].start:], '\n'); n >= 0 {
			lineEnd = edits[i].start + n
		}
		j := i
		for j < len(edits) && edits[j].start < lineEnd {
			j++
		}
		line := text[lineStart:lineEnd]
		shifted := make([]textEdit, 0, j-i)
		for _, e := range edits[i:j] {
			shifted = append(shifted, textEdit{start: e.start - lineStart, end: e.end - lineStart, replacement: e.replacement})
		}
		out = append(out, LineEdit{
			Line:   edits[i].line,
			Before: strings.TrimSuffix(line, "\r"),
			After:  strings.TrimSuffix(applyEdits(line, shifted), "\r"),
		})
		i = j
	}
	return out
}

// spliceEdits applies the edits to an Automerge text whose content is text, as the smallest
// splices: only the part of each destination that differs is replaced, so concurrent offline
// edits next to it merge cleanly.
func spliceEdits(t *automerge.Text, text string, edits []textEdit) error {
	for _, e := range slices.Backward(edits) {

		old := text[e.start:e.end]
		prefix := commonPrefix(old, e.replacement)
		suffix := commonSuffix(old[prefix:], e.replacement[prefix:])
		start := utf8.RuneCountInString(text[:e.start+prefix])
		deleted := utf8.RuneCountInString(old[prefix : len(old)-suffix])
		inserted := e.replacement[prefix : len(e.replacement)-suffix]
		if deleted > 0 {
			if err := t.Delete(start, deleted); err != nil {
				return err
			}
		}
		if inserted != "" {
			if err := t.Insert(start, inserted); err != nil {
				return err
			}
		}
	}
	return nil
}

// commonPrefix returns the length in bytes of the common prefix, at a rune boundary
func commonPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) {
		ra, sa := utf8.DecodeRuneInString(a[n:])
		rb, sb := utf8.DecodeRuneInString(b[n:])
		if ra != rb || sa != sb {
			break
		}
		n += sa
	}
	return n
}

// commonSuffix returns the length in bytes of the common suffix, at a rune boundary
func commonSuffix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) {
		ra, sa := utf8.DecodeLastRuneInString(a[:len(a)-n])
		rb, sb := utf8.DecodeLastRuneInString(b[:len(b)-n])
		if ra != rb || sa != sb {
			break
		}
		n += sa
	}
	return n
}

// plainYAMLPath matches paths that can stay unquoted in YAML
var plainYAMLPath = regexp.MustCompile(`^[\p{L}\p{N}_./(][\p{L}\p{N} _./()+,=@~&%-]*$`)

// configEdits computes the changes of mkdocs.yml for a move: the paths in nav, theme.logo,
// theme.favicon and extra_javascript. Only the scalars change, the rest of the file (comments,
// formatting, tags) stays as it is.
func configEdits(content string, fileName string, mapPath func(string) string) (string, []ConfigEdit) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(content), &root); err != nil || len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return content, nil
	}
	lineStarts := []int{0}
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' {
			lineStarts = append(lineStarts, i+1)
		}
	}
	var edits []textEdit
	for _, n := range configPathNodes(root.Content[0]) {
		if e, ok := configNodeEdit(content, lineStarts, n, mapPath); ok {
			edits = append(edits, e)
		}
	}
	if len(edits) == 0 {
		return content, nil
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	var out []ConfigEdit
	for _, le := range lineEdits(content, edits) {
		out = append(out, ConfigEdit{File: fileName, Line: le.Line, Before: le.Before, After: le.After})
	}
	return applyEdits(content, edits), out
}

// mappingPairs iterates over the keys and values of a mapping node
func mappingPairs(n *yaml.Node) iter.Seq2[*yaml.Node, *yaml.Node] {
	return func(yield func(*yaml.Node, *yaml.Node) bool) {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if !yield(n.Content[i], n.Content[i+1]) {
				return
			}
		}
	}
}

// configPathNodes returns the scalars of mkdocs.yml that hold paths of items
func configPathNodes(top *yaml.Node) []*yaml.Node {
	var nodes []*yaml.Node
	for key, value := range mappingPairs(top) {
		switch key.Value {
		case "nav":
			nodes = appendNavNodes(nodes, value)
		case "theme":
			if value.Kind == yaml.MappingNode {
				for k, v := range mappingPairs(value) {
					if (k.Value == "logo" || k.Value == "favicon") && v.Kind == yaml.ScalarNode {
						nodes = append(nodes, v)
					}
				}
			}
		case "extra_javascript":
			if value.Kind == yaml.SequenceNode {
				for _, c := range value.Content {
					nodes = appendExtraJavascriptNode(nodes, c)
				}
			}
		}
	}
	return nodes
}

// appendNavNodes appends all scalar values of a nav tree (not the titles, which are keys)
func appendNavNodes(nodes []*yaml.Node, n *yaml.Node) []*yaml.Node {
	switch n.Kind {
	case yaml.ScalarNode:
		nodes = append(nodes, n)
	case yaml.SequenceNode:
		for _, c := range n.Content {
			nodes = appendNavNodes(nodes, c)
		}
	case yaml.MappingNode:
		for _, v := range mappingPairs(n) {
			nodes = appendNavNodes(nodes, v)
		}
	}
	return nodes
}

// appendExtraJavascriptNode appends the path of an entry of extra_javascript: a plain path or a
// mapping with a "path" key
func appendExtraJavascriptNode(nodes []*yaml.Node, n *yaml.Node) []*yaml.Node {
	switch n.Kind {
	case yaml.ScalarNode:
		nodes = append(nodes, n)
	case yaml.MappingNode:
		for k, v := range mappingPairs(n) {
			if k.Value == "path" && v.Kind == yaml.ScalarNode {
				nodes = append(nodes, v)
			}
		}
	}
	return nodes
}

// configNodeEdit computes the edit of a scalar of mkdocs.yml, if its path changes
func configNodeEdit(content string, lineStarts []int, n *yaml.Node, mapPath func(string) string) (textEdit, bool) {
	if n.Tag != "!!str" || n.Value == "" || urlSchemePattern.MatchString(n.Value) {
		return textEdit{}, false
	}
	relative := strings.TrimPrefix(n.Value, "/")
	mapped := mapPath(relative)
	if mapped == relative || n.Line < 1 || n.Line > len(lineStarts) {
		return textEdit{}, false
	}
	if relative != n.Value {
		mapped = "/" + mapped
	}
	start, end, ok := scalarSpan(content, lineStarts[n.Line-1], n)
	if !ok {
		return textEdit{}, false
	}
	return textEdit{start: start, end: end, replacement: yamlScalar(mapped, n.Style), line: n.Line}, true
}

// scalarSpan finds the bytes of a scalar node (with its quotes) in content
func scalarSpan(content string, lineStart int, n *yaml.Node) (int, int, bool) {
	lineEnd := len(content)
	if i := strings.IndexByte(content[lineStart:], '\n'); i >= 0 {
		lineEnd = lineStart + i
	}
	line := content[lineStart:lineEnd]
	// yaml.v3 columns count characters
	offset := 0
	for col := 1; col < n.Column && offset < len(line); col++ {
		_, size := utf8.DecodeRuneInString(line[offset:])
		offset += size
	}
	rest := line[offset:]
	switch {
	case n.Style&yaml.DoubleQuotedStyle != 0 && strings.HasPrefix(rest, `"`):
		for i := 1; i < len(rest); i++ {
			switch rest[i] {
			case '\\':
				i++
			case '"':
				return lineStart + offset, lineStart + offset + i + 1, true
			}
		}
	case n.Style&yaml.SingleQuotedStyle != 0 && strings.HasPrefix(rest, "'"):
		for i := 1; i < len(rest); i++ {
			if rest[i] == '\'' {
				if i+1 < len(rest) && rest[i+1] == '\'' {
					i++
					continue
				}
				return lineStart + offset, lineStart + offset + i + 1, true
			}
		}
	case n.Style == 0 && strings.HasPrefix(rest, n.Value):
		return lineStart + offset, lineStart + offset + len(n.Value), true
	}
	return 0, 0, false
}

// yamlScalar writes a string in the given style, quoting a plain scalar when necessary
func yamlScalar(s string, style yaml.Style) string {
	switch {
	case style&yaml.SingleQuotedStyle != 0:
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	case style&yaml.DoubleQuotedStyle != 0 || !plainYAMLPath.MatchString(s) || strings.HasSuffix(s, " ") ||
		strings.Contains(s, ": ") || strings.Contains(s, " #"):
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
	}
	return s
}
