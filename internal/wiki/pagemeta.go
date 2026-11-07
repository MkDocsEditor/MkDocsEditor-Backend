package wiki

import (
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// maxMetaRead is how much of a document is read for its metadata
const maxMetaRead = 1 << 20

// PageMeta are the front matter fields the clients show, see docs/page-meta.md
type PageMeta struct {
	Title         *string  `json:"title"`
	Description   *string  `json:"description"`
	Icon          *string  `json:"icon"`
	Status        *string  `json:"status"`
	Tags          []string `json:"tags"`
	SearchExclude bool     `json:"searchExclude"`
	// QR are the valid QR code keys of the page, see docs/qr.md
	QR []QRCode `json:"qr"`
	// QRInvalid are the qr values that are no valid key, as written
	QRInvalid []string `json:"-"`
}

var (
	atxH1Pattern    = regexp.MustCompile(`^ {0,3}#(?:[ \t]+(.*?))?(?:[ \t]+#+)?[ \t]*$`)
	setextH1Pattern = regexp.MustCompile(`^ {0,3}=+[ \t]*$`)
	fenceStart      = regexp.MustCompile("^ {0,3}(```|~~~)")
)

// splitFrontMatter returns the YAML front matter of a text and the text after it. ok is false
// if the text has none.
func splitFrontMatter(text string) (front string, body string, ok bool) {
	text = strings.TrimPrefix(text, "\uFEFF")
	first, rest, found := strings.Cut(text, "\n")
	if !found || strings.TrimRight(first, " \t\r") != "---" {
		return "", text, false
	}
	offset := 0
	for {
		line, after, more := strings.Cut(rest[offset:], "\n")
		if l := strings.TrimRight(line, " \t\r"); l == "---" || l == "..." {
			return rest[:offset], after, true
		}
		if !more {
			return "", text, false
		}
		offset += len(line) + 1
	}
}

// FrontMatterLines returns the number of lines of the front matter with its two delimiter
// lines, 0 if the text has none. The YAML inside is not parsed.
func FrontMatterLines(text string) int {
	front, _, ok := splitFrontMatter(text)
	if !ok {
		return 0
	}
	return strings.Count(front, "\n") + 2
}

// ParsePageMeta reads the front matter of a document. Invalid YAML counts as none.
func ParsePageMeta(text string) (PageMeta, string) {
	meta := PageMeta{Tags: []string{}, QR: []QRCode{}}
	front, body, ok := splitFrontMatter(text)
	if !ok {
		return meta, text
	}
	var raw map[string]any
	if err := yaml.Unmarshal([]byte(front), &raw); err != nil {
		return meta, text
	}
	str := func(v any) *string {
		if v == nil {
			return nil
		}
		s := fmt.Sprint(v)
		return &s
	}
	meta.Title = str(raw["title"])
	meta.Description = str(raw["description"])
	meta.Icon = str(raw["icon"])
	meta.Status = str(raw["status"])
	switch tags := raw["tags"].(type) {
	case []any:
		for _, t := range tags {
			if t != nil {
				meta.Tags = append(meta.Tags, fmt.Sprint(t))
			}
		}
	case nil:
	default:
		meta.Tags = append(meta.Tags, fmt.Sprint(tags))
	}
	if search, ok := raw["search"].(map[string]any); ok {
		meta.SearchExclude, _ = search["exclude"].(bool)
	}
	meta.QR, meta.QRInvalid = parseQRCodes(raw["qr"])
	return meta, body
}

// firstH1 returns the text of the first level-1 heading outside fenced code, "" if there is none
func firstH1(body string) string {
	fence := ""
	prev := ""
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if m := fenceStart.FindStringSubmatch(line); m != nil {
			switch fence {
			case "":
				fence = m[1]
			case m[1]:
				fence = ""
			}
			prev = ""
			continue
		}
		if fence != "" {
			continue
		}
		if m := atxH1Pattern.FindStringSubmatch(line); m != nil {
			if t := strings.TrimSpace(m[1]); t != "" {
				return t
			}
		}
		if setextH1Pattern.MatchString(line) && strings.TrimSpace(prev) != "" && !strings.HasPrefix(strings.TrimSpace(prev), "#") {
			return strings.TrimSpace(prev)
		}
		prev = line
	}
	return ""
}

// titleFromPath is the page name MkDocs derives from the file: index and README pages take the
// folder name, the index of the docs folder is "Home"
func titleFromPath(relPath string) string {
	dir, file := path.Split(relPath)
	name := strings.TrimSuffix(file, MarkdownFileExtension)
	if strings.EqualFold(name, "index") || strings.EqualFold(name, "README") {
		dir = strings.TrimSuffix(dir, "/")
		if dir == "" {
			return "Home"
		}
		name = path.Base(dir)
	}
	name = strings.NewReplacer("-", " ", "_", " ").Replace(name)
	if strings.ToLower(name) == name && name != "" {
		name = strings.ToUpper(name[:1]) + name[1:]
	}
	return name
}

// PageTitle returns the effective page name: the front matter title, the first level-1
// heading, or the name from the file
func PageTitle(meta PageMeta, body string, relPath string) string {
	if meta.Title != nil && strings.TrimSpace(*meta.Title) != "" {
		return *meta.Title
	}
	if h := firstH1(body); h != "" {
		return h
	}
	return titleFromPath(relPath)
}

// metaCache keeps the metadata of documents by path while their size and mtime stay the same
type metaCache struct {
	mu      sync.Mutex
	entries map[string]metaEntry
}

type metaEntry struct {
	size  int64
	mtime time.Time
	meta  PageMeta
	title string
	// seen is the tree build that last used the entry
	seen uint64
}

// get returns the metadata of a document, reading it if it changed
func (c *metaCache) get(absPath string, relPath string, info os.FileInfo, build uint64) (PageMeta, string) {
	c.mu.Lock()
	e, ok := c.entries[absPath]
	c.mu.Unlock()
	if !ok || e.size != info.Size() || !e.mtime.Equal(info.ModTime()) {
		e = metaEntry{size: info.Size(), mtime: info.ModTime()}
		text := ""
		if f, err := os.Open(absPath); err == nil {
			data, _ := io.ReadAll(io.LimitReader(f, maxMetaRead))
			_ = f.Close()
			text = string(data)
		}
		var body string
		e.meta, body = ParsePageMeta(text)
		e.title = PageTitle(e.meta, body, relPath)
	}
	e.seen = build
	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[string]metaEntry{}
	}
	c.entries[absPath] = e
	c.mu.Unlock()
	return e.meta, e.title
}

// prune forgets the documents a tree build did not see
func (c *metaCache) prune(build uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for p, e := range c.entries {
		if e.seen != build {
			delete(c.entries, p)
		}
	}
}
