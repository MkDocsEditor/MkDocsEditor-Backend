package api

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	"github.com/labstack/echo/v5"
)

// Full text search over the documents, see docs/content-api.md

const (
	defaultSearchLimit  = 20
	maxSearchLimit      = 100
	maxSearchQueryRunes = 200
	maxSearchSnippets   = 3
	// snippetRunes is the length of the text around a match in a snippet
	snippetRunes = 160
	// titleMatchWeight ranks a match in the title or path over matches in the text
	titleMatchWeight = 10
)

type (
	// SearchSnippet is a line of a document with a match
	SearchSnippet struct {
		// Line is the line number, starting at 1
		Line int    `json:"line"`
		Text string `json:"text"`
	}

	// SearchResult is a document that matches a search
	SearchResult struct {
		ID       string          `json:"id"`
		Path     string          `json:"path"`
		Title    string          `json:"title"`
		Snippets []SearchSnippet `json:"snippets"`
		score    int
	}

	// SearchResponse is the answer of GET /search/
	SearchResponse struct {
		Results []*SearchResult `json:"results"`
		// Total is the number of matching documents, also those beyond the limit
		Total int `json:"total"`
	}
)

// GET /search/?q=…&limit=…: documents that contain every word of q (case-insensitive) in their
// text, title or path, the best matches first
func (rs *RestService) getSearch(c *echo.Context) error {
	q := strings.TrimSpace(c.QueryParam("q"))
	if q == "" || utf8.RuneCountInString(q) > maxSearchQueryRunes {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "q must have 1 to 200 characters", nil)
	}
	limit := defaultSearchLimit
	if v := c.QueryParam("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxSearchLimit {
			return apiError(c, http.StatusBadRequest, CodeBadRequest, "limit must be between 1 and 100", nil)
		}
		limit = n
	}
	terms := strings.Fields(strings.ToLower(q))

	results := []*SearchResult{}
	for section := range rs.treeManager.Root().Walk() {
		for _, d := range section.Documents {
			text, ok := rs.sync.CurrentText(d)
			if !ok {
				var err error
				if text, err = rs.treeManager.ReadDocumentContent(d); err != nil {
					continue
				}
			}
			if r := searchDocument(d, text, terms); r != nil {
				results = append(results, r)
			}
		}
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].score != results[j].score {
			return results[i].score > results[j].score
		}
		return results[i].Path < results[j].Path
	})
	response := &SearchResponse{Results: results, Total: len(results)}
	if len(results) > limit {
		response.Results = results[:limit]
	}
	return c.JSONPretty(http.StatusOK, response, jsonIndent)
}

// searchDocument returns the result of a document, or nil if a term is neither in its text nor
// in its title or path
func searchDocument(d *wiki.Document, text string, terms []string) *SearchResult {
	lowerText := strings.ToLower(text)
	head := strings.ToLower(d.Title + "\n" + d.RelPath)
	score := 0
	for _, t := range terms {
		inText := strings.Count(lowerText, t)
		inHead := strings.Count(head, t)
		if inText == 0 && inHead == 0 {
			return nil
		}
		score += inText + titleMatchWeight*inHead
	}
	return &SearchResult{ID: d.ID, Path: d.RelPath, Title: d.Title, Snippets: snippets(text, terms), score: score}
}

// snippets returns the first lines that contain a term, shortened around the first match
func snippets(text string, terms []string) []SearchSnippet {
	out := []SearchSnippet{}
	for i, line := range strings.Split(text, "\n") {
		lower := strings.ToLower(line)
		at := -1
		for _, t := range terms {
			if k := strings.Index(lower, t); k >= 0 && (at < 0 || k < at) {
				at = k
			}
		}
		if at < 0 {
			continue
		}
		out = append(out, SearchSnippet{Line: i + 1, Text: around(line, lower, at)})
		if len(out) == maxSearchSnippets {
			break
		}
	}
	return out
}

// around shortens a line to snippetRunes around the byte offset at of its lowercase form. The
// offset is mapped by runes, since lowercasing can change the byte length.
func around(line string, lower string, at int) string {
	runes := []rune(strings.TrimSpace(line))
	if len(runes) <= snippetRunes {
		return string(runes)
	}
	pos := utf8.RuneCountInString(lower[:at]) - (len([]rune(line)) - len([]rune(strings.TrimLeft(line, " \t"))))
	start := max(0, min(pos-snippetRunes/3, len(runes)-snippetRunes))
	end := start + snippetRunes
	s := string(runes[start:end])
	if start > 0 {
		s = "…" + s
	}
	if end < len(runes) {
		s += "…"
	}
	return s
}
