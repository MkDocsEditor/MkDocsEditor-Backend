package api

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/collab"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/references"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	"github.com/labstack/echo/v5"
)

type (
	// MovedResponse is the answer of GET /moved/{id}/
	MovedResponse struct {
		OldID   string              `json:"oldId"`
		Type    string              `json:"type"`
		Item    *references.ItemRef `json:"item"`
		MovedAt time.Time           `json:"movedAt"`
	}

	// MovedListResponse is the answer of GET /moved/?id=…&id=…
	MovedListResponse struct {
		Moved []*MovedResponse `json:"moved"`
	}
)

// maxMovedLookupIDs is the limit of ids in one GET /moved/ request. At 20 digits per id the
// request line stays below the 8 KiB that common reverse proxies allow.
const maxMovedLookupIDs = 200

// POST /move/
func (rs *RestService) postMove(c *echo.Context) error {
	req := new(collab.MoveRequest)
	if err := c.Bind(req); err != nil {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "invalid JSON", nil)
	}
	result, err := rs.performMove(c, req)
	if err != nil {
		return rs.moveFailed(c, req.ID, err)
	}
	return c.JSONPretty(http.StatusOK, result, jsonIndent)
}

// moveFailed answers a failed move
func (rs *RestService) moveFailed(c *echo.Context, id string, err error) error {
	var me *collab.MoveError
	switch {
	case errors.As(err, &me):
		switch me.Kind {
		case collab.MoveNotFound:
			return rs.ReturnNotFound(c, id)
		case collab.MoveConflict:
			return apiError(c, http.StatusConflict, CodeConflict, me.Message, me.Details)
		}
		return apiError(c, http.StatusBadRequest, CodeBadRequest, me.Message, me.Details)
	case errors.Is(err, wiki.ErrNotFound):
		return rs.ReturnNotFound(c, id)
	}
	log.Printf("Move of %s failed: %v", id, err)
	return apiError(c, http.StatusInternalServerError, CodeInternal, "the move failed and was undone", nil)
}

// renameViaMove renames an item in place with reference rewriting and answers the renamed
// item, like the rename endpoints always did
func (rs *RestService) renameViaMove(c *echo.Context, itemType string, name string) error {
	id := c.Param(urlParamID)
	lookup := references.NewTreeLookup(rs.treeManager.Root())
	item := lookup.Item(id)
	if item == nil || item.Type != itemType {
		return rs.ReturnNotFound(c, id)
	}
	currentName := path.Base(item.Path)
	if itemType == wiki.TypeDocument {
		currentName = strings.TrimSuffix(currentName, wiki.MarkdownFileExtension)
	}
	if name == currentName {
		return rs.describeItem(itemType)(c)
	}
	result, err := rs.performMove(c, &collab.MoveRequest{Type: itemType, ID: id, Name: name})
	if err != nil {
		return rs.moveFailed(c, id, err)
	}
	return c.JSONPretty(http.StatusOK, result.Item, jsonIndent)
}

// performMove runs a move and, unless it is a dry run, commits and audits it
func (rs *RestService) performMove(c *echo.Context, req *collab.MoveRequest) (*collab.MoveResult, error) {
	return rs.performMoveAs(UserFromContext(c), requestInfo(c), req)
}

// performMoveAs runs a move of a user and, unless it is a dry run, commits and audits it
func (rs *RestService) performMoveAs(user *auth.User, info auth.RequestInfo, req *collab.MoveRequest) (*collab.MoveResult, error) {
	result, completed, err := rs.sync.Move(req, user)
	if err != nil || completed == nil {
		return result, err
	}
	result.Commit = rs.commitMove(completed, len(result.Config) > 0)
	rs.auditMove(info, completed, result)
	rs.itemMoved(completed)
	return result, nil
}

// commitMove commits the move with the rewritten documents and mkdocs.yml
func (rs *RestService) commitMove(m *collab.CompletedMove, configChanged bool) string {
	if rs.git == nil {
		return ""
	}
	verb := "Move"
	if m.Rename {
		verb = "Rename"
	}
	paths := []string{m.OldRel, m.NewRel}
	var listed []string
	for _, p := range m.ChangedPaths {
		paths = append(paths, p)
		listed = append(listed, p)
	}
	sort.Strings(listed)
	if configChanged {
		if rel, err := filepath.Rel(rs.treeManager.RootPath(), rs.treeManager.ConfigFile()); err == nil {
			paths = append(paths, filepath.ToSlash(rel))
			listed = append(listed, filepath.Base(rel))
		}
	}
	body := ""
	if len(listed) > 0 {
		noun := "documents"
		if len(m.ChangedPaths) == 1 {
			noun = "document"
		}
		body = fmt.Sprintf("Update references in %d %s:\n- %s", len(m.ChangedPaths), noun, strings.Join(listed, "\n- "))
	}
	return rs.git.CommitTreeChangeWithBody(m.User, fmt.Sprintf("%s %s to %s", verb, m.OldRel, m.NewRel), body, paths...)
}

// auditMove writes the audit log entry of a move
func (rs *RestService) auditMove(info auth.RequestInfo, m *collab.CompletedMove, result *collab.MoveResult) {
	eventType := auth.AuditItemMoved
	if m.Rename {
		eventType = auth.AuditItemRenamed
	}
	actorID := ""
	if m.User != nil {
		actorID = m.User.ID
	}
	rs.authenticator.Audit(auth.AuditEvent{Type: eventType, ActorID: actorID, IP: info.IP, UserAgent: info.UserAgent,
		Details: map[string]any{
			"type":      m.ItemType,
			"oldPath":   m.OldRel,
			"newPath":   m.NewRel,
			"items":     len(result.Moved),
			"documents": len(result.Documents),
		}})
}
