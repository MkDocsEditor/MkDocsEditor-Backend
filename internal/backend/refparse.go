package backend

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Kinds of references, see docs/references.md section 2
const (
	RefKindLink       = "link"
	RefKindImage      = "image"
	RefKindDefinition = "definition"
	RefKindHTML       = "html"
)

// parsedRef is a reference found in a markdown text
type parsedRef struct {
	Kind string
	// Line is 1-based, Column is 1-based in Unicode code points
	Line   int
	Column int
	// Start and End are the byte offsets of the destination in the text
	Start int
	End   int
	// Raw is the destination as written
	Raw string
	// HTML is true for HTML attribute values (entities may be used)
	HTML bool
}

var (
	fencePattern      = regexp.MustCompile("^[ \t>]*(`{3,}|~{3,})")
	definitionPattern = regexp.MustCompile(`^[ \t]*\[([^\]]+)\]:[ \t]*(<[^>\n]*>|[^ \t\r\n]+)`)
	htmlTagPattern    = regexp.MustCompile(`(?i)^<(a|img|source|video|audio)\b[^>]*>`)
	htmlAttrPattern   = regexp.MustCompile(`(?i)[\s/](href|src)[ \t]*=[ \t]*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+))`)
)

// parseReferences finds the links, images, reference definitions and HTML references of a
// markdown text. Code blocks, code spans, HTML comments and a front matter block are skipped.
func parseReferences(text string) []parsedRef {
	p := refParser{text: text}
	p.run()
	return p.refs
}

type refParser struct {
	text string
	refs []parsedRef

	// state across lines
	fence     string
	inComment bool
}

func (p *refParser) run() {
	lineNo := 0
	offset := 0
	frontMatter := strings.HasPrefix(p.text, "---\n") || strings.HasPrefix(p.text, "---\r\n")
	for offset <= len(p.text) {
		end := strings.IndexByte(p.text[offset:], '\n')
		var line string
		if end < 0 {
			line = p.text[offset:]
		} else {
			line = p.text[offset : offset+end]
		}
		lineNo++

		switch {
		case frontMatter:
			if lineNo > 1 {
				trimmed := strings.TrimRight(line, " \t\r")
				if trimmed == "---" || trimmed == "..." {
					frontMatter = false
				}
			}
		case p.fence != "":
			if m := fencePattern.FindStringSubmatch(line); m != nil && m[1][0] == p.fence[0] && len(m[1]) >= len(p.fence) &&
				strings.TrimSpace(strings.TrimLeft(line, " \t>")[len(m[1]):]) == "" {
				p.fence = ""
			}
		case !p.inComment && fencePattern.MatchString(line):
			m := fencePattern.FindStringSubmatch(line)
			// a backtick fence's info string must not contain backticks, otherwise it is inline code
			rest := strings.TrimLeft(line, " \t>")[len(m[1]):]
			if m[1][0] != '`' || !strings.Contains(rest, "`") {
				p.fence = m[1]
			} else {
				p.scanLine(line, offset, lineNo)
			}
		default:
			if !p.inComment {
				if m := definitionPattern.FindStringSubmatchIndex(line); m != nil {
					label := line[m[2]:m[3]]
					if !strings.HasPrefix(label, "^") {
						start, stop := m[4], m[5]
						if line[start] == '<' {
							start, stop = start+1, stop-1
						}
						p.add(RefKindDefinition, line, offset, lineNo, start, stop, false)
						break
					}
				}
			}
			p.scanLine(line, offset, lineNo)
		}

		if end < 0 {
			break
		}
		offset += end + 1
	}
}

// add records a reference whose destination is line[start:stop]
func (p *refParser) add(kind string, line string, lineOffset int, lineNo int, start int, stop int, html bool) {
	p.refs = append(p.refs, parsedRef{
		Kind:   kind,
		Line:   lineNo,
		Column: utf8.RuneCountInString(line[:start]) + 1,
		Start:  lineOffset + start,
		End:    lineOffset + stop,
		Raw:    line[start:stop],
		HTML:   html,
	})
}

// scanLine finds inline links, images and HTML tags in one line
func (p *refParser) scanLine(line string, lineOffset int, lineNo int) {
	type bracket struct {
		pos   int
		image bool
	}
	var brackets []bracket
	for i := 0; i < len(line); i++ {
		if p.inComment {
			end := strings.Index(line[i:], "-->")
			if end < 0 {
				return
			}
			p.inComment = false
			i += end + 2
			continue
		}
		switch line[i] {
		case '\\':
			i++
		case '`':
			n := countRun(line[i:], '`')
			if closing := findBacktickRun(line[i+n:], n); closing >= 0 {
				i += n + closing + n - 1
			} else {
				i += n - 1
			}
		case '<':
			if strings.HasPrefix(line[i:], "<!--") {
				p.inComment = true
				i += 3
				continue
			}
			if m := htmlTagPattern.FindStringIndex(line[i:]); m != nil {
				tag := line[i : i+m[1]]
				for _, a := range htmlAttrPattern.FindAllStringSubmatchIndex(tag, -1) {
					for g := 2; g <= 4; g++ {
						if a[2*g] >= 0 {
							p.add(RefKindHTML, line, lineOffset, lineNo, i+a[2*g], i+a[2*g+1], true)
						}
					}
				}
				i += m[1] - 1
			}
		case '[':
			brackets = append(brackets, bracket{pos: i, image: i > 0 && line[i-1] == '!' && (i < 2 || line[i-2] != '\\')})
		case ']':
			if len(brackets) == 0 {
				continue
			}
			open := brackets[len(brackets)-1]
			brackets = brackets[:len(brackets)-1]
			if i+1 < len(line) && line[i+1] == '(' {
				start, stop, closing, ok := parseInlineDestination(line, i+2)
				if !ok {
					continue
				}
				kind := RefKindLink
				if open.image {
					kind = RefKindImage
				}
				if stop > start {
					p.add(kind, line, lineOffset, lineNo, start, stop, false)
				}
				i = closing
			}
		}
	}
}

func countRun(s string, c byte) int {
	n := 0
	for n < len(s) && s[n] == c {
		n++
	}
	return n
}

// findBacktickRun returns the offset of the next run of exactly n backticks, or -1
func findBacktickRun(s string, n int) int {
	for i := 0; i < len(s); {
		if s[i] != '`' {
			i++
			continue
		}
		run := countRun(s[i:], '`')
		if run == n {
			return i
		}
		i += run
	}
	return -1
}

// parseInlineDestination parses "dest", "<dest>", "dest \"title\"" and similar after "](" at
// position pos. It returns the destination's byte range and the position of the closing ")".
func parseInlineDestination(line string, pos int) (start int, stop int, closing int, ok bool) {
	i := skipSpaces(line, pos)
	if i < len(line) && line[i] == '<' {
		end := strings.IndexAny(line[i+1:], "<>")
		if end < 0 || line[i+1+end] != '>' {
			return 0, 0, 0, false
		}
		start, stop = i+1, i+1+end
		i = stop + 1
	} else {
		start = i
		depth := 0
	loop:
		for i < len(line) {
			switch c := line[i]; {
			case c == '\\' && i+1 < len(line):
				i += 2
				continue
			case c == ' ' || c == '\t' || c == '\r':
				break loop
			case c == '(':
				depth++
			case c == ')':
				if depth == 0 {
					break loop
				}
				depth--
			case c < 0x20:
				return 0, 0, 0, false
			}
			i++
		}
		stop = i
	}
	i = skipSpaces(line, i)
	if i < len(line) && (line[i] == '"' || line[i] == '\'' || line[i] == '(') {
		closer := line[i]
		if closer == '(' {
			closer = ')'
		}
		j := i + 1
		for j < len(line) && line[j] != closer {
			if line[j] == '\\' {
				j++
			}
			j++
		}
		if j >= len(line) {
			return 0, 0, 0, false
		}
		i = skipSpaces(line, j+1)
	}
	if i >= len(line) || line[i] != ')' {
		return 0, 0, 0, false
	}
	return start, stop, i, true
}

func skipSpaces(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\r') {
		i++
	}
	return i
}
