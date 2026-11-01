package backend

import (
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

// LinkTarget is the input of the link path algorithm, see docs/references.md section 8
type LinkTarget struct {
	Type string `json:"type"`
	// Path is relative to the docs folder, "" for the root section
	Path string `json:"path"`
	// Index is the path of a section's index document, or ""
	Index string `json:"index,omitempty"`
}

// characters that are percent-encoded in a link path, besides control characters and space
const linkEncodedChars = "\"#%'()<>?[\\]^`{|}"

// LinkPath returns the destination of a link from the document at fromPath to target, as
// described in docs/references.md section 8. anchor is optional, without "#".
func LinkPath(fromPath string, target LinkTarget, anchor string) string {
	return linkPath(fromPath, target, anchor, linkEncoding{})
}

// linkEncoding selects variants of the encoding, used to keep the style of existing references
type linkEncoding struct {
	// nonASCII percent-encodes non-ASCII characters too
	nonASCII bool
	// rawSpaces keeps spaces as they are
	rawSpaces bool
	// spelling maps decoded path segments to how an existing destination wrote them
	spelling map[string]string
}

func linkPath(fromPath string, target LinkTarget, anchor string, enc linkEncoding) string {
	targetPath, folder := target.Path, false
	if target.Type == TypeSection {
		if target.Index != "" {
			targetPath = target.Index
		} else {
			folder = true
		}
	}
	encodedAnchor := ""
	if anchor != "" {
		encodedAnchor = "#" + enc.encode(anchor)
	}
	if !folder && targetPath == fromPath {
		if anchor != "" {
			return encodedAnchor
		}
		return enc.encode(path.Base(fromPath))
	}
	return relativeLink(path.Dir(fromPath), targetPath, folder, enc) + encodedAnchor
}

// relativeLink builds the relative path from the folder fromDir to targetPath (steps 3 to 7)
func relativeLink(fromDir string, targetPath string, folder bool, enc linkEncoding) string {
	from, to := splitPath(fromDir), splitPath(targetPath)
	common := 0
	for common < len(from) && common < len(to) && from[common] == to[common] {
		common++
	}
	segments := make([]string, 0, len(from)-common+len(to)-common)
	for range from[common:] {
		segments = append(segments, "..")
	}
	for _, s := range to[common:] {
		segments = append(segments, enc.segment(s))
	}
	result := strings.Join(segments, "/")
	if folder {
		result += "/"
		if result == "/" {
			result = "./"
		}
	}
	if len(segments) > 0 && strings.Contains(segments[0], ":") {
		result = "./" + result
	}
	return result
}

// splitPath splits a slash separated path into its segments; "" and "." have none
func splitPath(p string) []string {
	if p == "" || p == "." {
		return nil
	}
	return strings.Split(p, "/")
}

// segment encodes a path segment, in the spelling of an existing destination if it had the segment
func (e linkEncoding) segment(s string) string {
	if raw, ok := e.spelling[s]; ok {
		return raw
	}
	return e.encode(s)
}

// encode percent-encodes a path segment or an anchor
func (e linkEncoding) encode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == ' ' && e.rawSpaces:
			b.WriteByte(' ')
		case r < utf8.RuneSelf && (r <= 0x20 || r == 0x7f || strings.ContainsRune(linkEncodedChars, r)):
			writePercent(&b, s[i:i+size])
		case r >= utf8.RuneSelf && e.nonASCII:
			writePercent(&b, s[i:i+size])
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

func writePercent(b *strings.Builder, s string) {
	const hex = "0123456789ABCDEF"
	for i := 0; i < len(s); i++ {
		b.WriteByte('%')
		b.WriteByte(hex[s[i]>>4])
		b.WriteByte(hex[s[i]&0x0f])
	}
}

// percentDecode decodes %XX escapes. Invalid escapes stay as they are, "+" stays a plus sign.
func percentDecode(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			b.WriteByte(unhex(s[i+1])<<4 | unhex(s[i+2]))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

var (
	urlSchemePattern  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.\-]*:`)
	encodedNonASCIIRe = regexp.MustCompile(`%[89A-Fa-f][0-9A-Fa-f]`)
)

// destination is a parsed link destination, see docs/references.md section 3
type destination struct {
	// raw is the destination as written
	raw string
	// target is the part before "?" and "#", as written (encoded)
	target string
	// suffix is "?query#anchor" as written, or ""
	suffix string
	// anchor is the part after "#", or ""
	anchor string
	// absolute is true for destinations starting with "/"
	absolute bool
	// dotSlash is true for destinations starting with "./"
	dotSlash bool
	// folder is true for destinations ending with "/"
	folder bool
	// path is the resolved path relative to the docs folder ("" is the docs folder itself)
	path string
	// outside is true if the path leaves the docs folder
	outside bool
}

// parseDestination classifies and resolves a destination of a reference in the document at
// fromPath. ok is false for external destinations and same-document anchors.
func parseDestination(fromPath string, raw string) (destination, bool) {
	d := destination{raw: raw}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.HasPrefix(trimmed, "//") || urlSchemePattern.MatchString(trimmed) {
		return d, false
	}
	d.target = trimmed
	if i := strings.IndexAny(trimmed, "?#"); i >= 0 {
		d.target, d.suffix = trimmed[:i], trimmed[i:]
		if j := strings.IndexByte(d.suffix, '#'); j >= 0 {
			d.anchor = d.suffix[j+1:]
		}
	}
	if d.target == "" {
		// "#anchor" or "?query": the same document
		return d, false
	}
	decoded := percentDecode(d.target)
	d.absolute = strings.HasPrefix(decoded, "/")
	d.dotSlash = strings.HasPrefix(decoded, "./")
	d.folder = strings.HasSuffix(decoded, "/")
	full := decoded
	if !d.absolute {
		full = path.Dir(fromPath) + "/" + decoded
	}
	d.outside = escapesRoot(full)
	joined := path.Clean("/" + full)
	d.path = strings.TrimPrefix(joined, "/")
	return d, true
}

// escapesRoot reports whether a slash separated path goes above its starting folder
func escapesRoot(p string) bool {
	depth := 0
	for s := range strings.SplitSeq(p, "/") {
		switch s {
		case "", ".":
		case "..":
			depth--
			if depth < 0 {
				return true
			}
		default:
			depth++
		}
	}
	return false
}

// encodingOf returns the encoding variant that keeps the style of an existing destination
func encodingOf(raw string) linkEncoding {
	enc := linkEncoding{
		nonASCII:  encodedNonASCIIRe.MatchString(raw),
		rawSpaces: strings.Contains(raw, " "),
		spelling:  map[string]string{},
	}
	for segment := range strings.SplitSeq(raw, "/") {
		if segment != "" && segment != "." && segment != ".." {
			enc.spelling[percentDecode(segment)] = segment
		}
	}
	return enc
}

// rewriteDestination returns the destination that points from the document at newFromPath to
// the item at newTargetPath, in the style of the old destination d (see docs/references.md
// section 6). Folder destinations stay folder destinations.
func rewriteDestination(d destination, newFromPath string, newTargetPath string) string {
	enc := encodingOf(d.target)
	var target string
	switch {
	case d.absolute:
		segments := splitPath(newTargetPath)
		for i, s := range segments {
			segments[i] = enc.segment(s)
		}
		target = "/" + strings.Join(segments, "/")
		if d.folder && newTargetPath != "" {
			target += "/"
		}
	default:
		target = relativeLink(path.Dir(newFromPath), newTargetPath, d.folder, enc)
		if !d.folder && target == "" {
			// a link to the document itself by file name
			target = enc.segment(path.Base(newTargetPath))
		}
		if d.dotSlash && !strings.HasPrefix(target, "./") && !strings.HasPrefix(target, "../") {
			target = "./" + target
		}
	}
	return target + d.suffix
}
