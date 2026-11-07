package api

import (
	"errors"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/activity"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/collab"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	"github.com/labstack/echo/v5"
)

const (
	// FeaturePins means the server has the pins of docs/dashboard.md
	FeaturePins = "pins"
	// FeatureFeed means the server has the change feed of docs/dashboard.md
	FeatureFeed = "feed"
	// FeaturePinOrder means the pins can be reordered, see docs/dashboard.md
	FeaturePinOrder = "pin-order"

	feedDefaultLimit = 50
	feedMaxLimit     = 200

	// pseudoAuthorColor is the color of the server's pseudo users
	pseudoAuthorColor = "#868e96"
)

type (
	// Author is who pinned a document or made a feed entry
	Author struct {
		AccountID   string  `json:"accountId,omitempty"`
		Username    string  `json:"username,omitempty"`
		DisplayName string  `json:"displayName"`
		Color       string  `json:"color"`
		Avatar      *string `json:"avatar"`
	}

	// PinResponse is a pinned document
	PinResponse struct {
		DocumentID string `json:"documentId"`
		Path       string `json:"path"`
		Name       string `json:"name"`
		PinnedBy   Author `json:"pinnedBy"`
		PinnedAt   string `json:"pinnedAt"`
	}

	// PinOrderRequest is the body of PUT /pins/{family|personal}/order/
	PinOrderRequest struct {
		DocumentIDs []string `json:"documentIds"`
	}

	// PinsResponse is the answer of GET /pins/ and of the pin changes
	PinsResponse struct {
		Family   []PinResponse `json:"family"`
		Personal []PinResponse `json:"personal"`
	}

	// FeedEntryResponse is an entry of the feed
	FeedEntryResponse struct {
		ID         string            `json:"id"`
		Time       string            `json:"time"`
		Kind       string            `json:"kind"`
		ItemType   string            `json:"itemType"`
		DocumentID string            `json:"documentId,omitempty"`
		Path       string            `json:"path"`
		OldPath    string            `json:"oldPath,omitempty"`
		Authors    []Author          `json:"authors"`
		Summary    *activity.Summary `json:"summary,omitempty"`
	}

	// FeedResponse is the answer of GET /feed/
	FeedResponse struct {
		Entries []FeedEntryResponse `json:"entries"`
		Next    *string             `json:"next"`
	}
)

// author describes an account id or one of the server's pseudo users as it is now
func (rs *RestService) author(subject string) Author {
	switch subject {
	case collab.ActorUserDisk:
		return Author{DisplayName: "External change", Color: pseudoAuthorColor}
	case collab.ActorUserServer:
		return Author{DisplayName: "Server", Color: pseudoAuthorColor}
	}
	if u := rs.authenticator.UserByID(subject); u != nil {
		return Author{AccountID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Color: u.Color, Avatar: u.Avatar}
	}
	return Author{AccountID: subject, DisplayName: subject, Color: auth.ColorForUsername(subject)}
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// documentsByPath maps the paths of all documents to them
func (rs *RestService) documentsByPath() map[string]*wiki.Document {
	out := map[string]*wiki.Document{}
	for s := range rs.treeManager.Root().Walk() {
		for _, d := range s.Documents {
			out[d.RelPath] = d
		}
	}
	return out
}

// pinsOf lists the pins of an account. Pins of documents that do not exist are left out: they
// come back when the document does.
func (rs *RestService) pinsOf(accountID string) (*PinsResponse, error) {
	family, personal, err := rs.sync.Activity().Pins(accountID)
	if err != nil {
		return nil, err
	}
	docs := rs.documentsByPath()
	convert := func(pins []activity.Pin) []PinResponse {
		out := []PinResponse{}
		for _, p := range pins {
			d := docs[p.Path]
			if d == nil {
				continue
			}
			out = append(out, PinResponse{DocumentID: d.ID, Path: d.RelPath, Name: d.Name,
				PinnedBy: rs.author(p.PinnedBy), PinnedAt: formatTime(p.PinnedAt)})
		}
		return out
	}
	return &PinsResponse{Family: convert(family), Personal: convert(personal)}, nil
}

// GET /pins/
func (rs *RestService) getPins(c *echo.Context) error {
	pins, err := rs.pinsOf(UserFromContext(c).ID)
	if err != nil {
		return rs.ReturnError(c, err)
	}
	return c.JSONPretty(http.StatusOK, pins, jsonIndent)
}

// changePin answers PUT and DELETE of /pins/family/{documentId}/ and /pins/personal/{documentId}/
func (rs *RestService) changePin(family bool, add bool) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user := UserFromContext(c)
		if family && !user.CanWrite() {
			return apiError(c, http.StatusForbidden, CodeForbidden, "only editors and admins may change the family pins", nil)
		}
		id := c.Param(urlParamID)
		d := rs.treeManager.GetDocument(id)
		if d == nil {
			return rs.ReturnNotFound(c, id)
		}
		owner := user.ID
		if family {
			owner = activity.Family
		}
		var (
			changed bool
			err     error
		)
		if add {
			changed, err = rs.sync.Activity().AddPin(owner, d.RelPath, user.ID)
		} else {
			changed, err = rs.sync.Activity().RemovePin(owner, d.RelPath)
		}
		if err != nil {
			return rs.ReturnError(c, err)
		}
		if changed {
			rs.sync.PinsChanged(family, user.ID)
		}
		return rs.getPins(c)
	}
}

// changePinOrder answers PUT /pins/family/order/ and /pins/personal/order/
func (rs *RestService) changePinOrder(family bool) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user := UserFromContext(c)
		if family && !user.CanWrite() {
			return apiError(c, http.StatusForbidden, CodeForbidden, "only editors and admins may change the family pins", nil)
		}
		r := new(PinOrderRequest)
		if err := c.Bind(r); err != nil {
			return apiError(c, http.StatusBadRequest, CodeBadRequest, "the body must be {\"documentIds\": [\"…\"]}", nil)
		}
		paths := make([]string, 0, len(r.DocumentIDs))
		for _, id := range r.DocumentIDs {
			if d := rs.treeManager.GetDocument(id); d != nil {
				paths = append(paths, d.RelPath)
			}
		}
		owner := user.ID
		if family {
			owner = activity.Family
		}
		changed, err := rs.sync.Activity().ReorderPins(owner, paths)
		if err != nil {
			return rs.ReturnError(c, err)
		}
		if changed {
			rs.sync.PinsChanged(family, user.ID)
		}
		return rs.getPins(c)
	}
}

// GET /feed/
func (rs *RestService) getFeed(c *echo.Context) error {
	limit := feedDefaultLimit
	if v := c.QueryParam("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > feedMaxLimit {
			return apiError(c, http.StatusBadRequest, CodeBadRequest, "limit must be a number from 1 to 200", nil)
		}
		limit = n
	}
	entries, next, err := rs.sync.Activity().Feed(c.QueryParam("before"), limit)
	if errors.Is(err, activity.ErrInvalidCursor) {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "invalid cursor", nil)
	}
	if err != nil {
		return rs.ReturnError(c, err)
	}
	docs := rs.documentsByPath()
	authors := map[string]Author{}
	out := &FeedResponse{Entries: make([]FeedEntryResponse, 0, len(entries))}
	for _, e := range entries {
		r := FeedEntryResponse{ID: strconv.FormatInt(e.ID, 10), Time: formatTime(e.Time), Kind: e.Kind, ItemType: e.ItemType,
			Path: e.Path, OldPath: e.OldPath, Authors: make([]Author, 0, len(e.Authors)), Summary: e.Summary}
		if d := docs[e.Path]; d != nil && e.ItemType == wiki.TypeDocument {
			r.DocumentID = d.ID
		}
		for _, a := range e.Authors {
			info, ok := authors[a]
			if !ok {
				info = rs.author(a)
				authors[a] = info
			}
			r.Authors = append(r.Authors, info)
		}
		out.Entries = append(out.Entries, r)
	}
	if next != "" {
		out.Next = &next
	}
	return c.JSONPretty(http.StatusOK, out, jsonIndent)
}

// relOf returns the path of an item relative to the docs folder, "" if it is outside
func (rs *RestService) relOf(absPath string) string {
	rel, err := filepath.Rel(rs.treeManager.RootPath(), absPath)
	if err != nil || !filepath.IsLocal(rel) {
		return ""
	}
	return filepath.ToSlash(rel)
}

// itemCreated adds a created document or section to the feed
func (rs *RestService) itemCreated(c *echo.Context, itemType string, absPath string) {
	if rel := rs.relOf(absPath); rel != "" {
		rs.sync.Activity().RecordItem(activity.KindCreated, itemType, rel, "", UserFromContext(c).ID)
	}
}

// itemDeleted adds a deleted document or section to the feed and removes its pins
func (rs *RestService) itemDeleted(c *echo.Context, itemType string, absPath string) {
	rel := rs.relOf(absPath)
	if rel == "" || itemType == wiki.TypeResource {
		return
	}
	rs.sync.Activity().RecordItem(activity.KindDeleted, itemType, rel, "", UserFromContext(c).ID)
	changes, err := rs.sync.Activity().DeletePins(rel)
	if err != nil {
		log.Printf("Unable to remove the pins of %s: %v", rel, err)
	}
	rs.pinsChanged(changes)
}

// itemMoved adds a move to the feed and moves the pins
func (rs *RestService) itemMoved(m *collab.CompletedMove) {
	if m.ItemType == wiki.TypeResource {
		return
	}
	author := ""
	if m.User != nil {
		author = m.User.ID
	}
	rs.sync.Activity().RecordItem(activity.KindMoved, m.ItemType, m.NewRel, m.OldRel, author)
	changes, err := rs.sync.Activity().MovePins(m.OldRel, m.NewRel)
	if err != nil {
		log.Printf("Unable to move the pins of %s: %v", m.OldRel, err)
	}
	rs.pinsChanged(changes)
}

func (rs *RestService) pinsChanged(changes activity.PinChanges) {
	if !changes.Empty() {
		rs.sync.PinsChanged(changes.Family, changes.Accounts...)
	}
}
