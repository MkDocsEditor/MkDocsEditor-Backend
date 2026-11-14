package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/collab"
	"github.com/labstack/echo/v5"
)

// Content changes over REST: edits by unique search strings and writes of the full text against
// a base revision. See docs/content-api.md.

const (
	CodeEditNotFound          = "edit-not-found"
	CodeEditAmbiguous         = "edit-ambiguous"
	CodeUnknownRevision       = "unknown-revision"
	CodeBaseRevisionRequired  = "base-revision-required"
	headerETag                = "ETag"
	maxContentRequestBytes    = 4 << 20
	maxEditsPerRequest        = 100
	editIndexDetailKey        = "index"
	editReasonDetailKey       = "reason"
	contentWriteRateKeyPrefix = "account:"
)

type (
	// EditRequest is the body of POST /document/{id}/edits/
	EditRequest struct {
		Edits        []collab.TextEdit `json:"edits"`
		BaseRevision string            `json:"baseRevision,omitempty"`
	}

	// WriteContentRequest is the body of PUT /document/{id}/content/
	WriteContentRequest struct {
		Content      *string `json:"content"`
		BaseRevision string  `json:"baseRevision"`
	}

	// RevisionResponse is the answer of a content change
	RevisionResponse struct {
		Revision string `json:"revision"`
	}
)

// setRevision sets the ETag header of a revision
func setRevision(c *echo.Context, revision string) {
	c.Response().Header().Set(headerETag, `"`+revision+`"`)
}

// agentOf returns who changes content with a request
func agentOf(c *echo.Context) collab.Agent {
	agent := collab.Agent{AccountID: UserFromContext(c).ID}
	if t := AppTokenFromContext(c); t != nil {
		agent.TokenID, agent.TokenName = t.ID, t.Name
	}
	return agent
}

// allowContentWrite applies the rate limit of content writes: per app token, else per account
func (rs *RestService) allowContentWrite(c *echo.Context) error {
	key := contentWriteRateKeyPrefix + UserFromContext(c).ID
	if t := AppTokenFromContext(c); t != nil {
		key = "token:" + t.ID
	}
	if err := rs.authenticator.AllowContentWrite(key, c.RealIP()); err != nil {
		return responded(rs.authError(c, err))
	}
	return nil
}

// bindLimitedJSON decodes a JSON body of at most maxContentRequestBytes
func bindLimitedJSON(c *echo.Context, v any) error {
	req := c.Request()
	req.Body = http.MaxBytesReader(c.Response(), req.Body, maxContentRequestBytes)
	if err := json.NewDecoder(req.Body).Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return responded(apiError(c, http.StatusRequestEntityTooLarge, CodePayloadTooLarge, "the request is too large", map[string]any{"maxBytes": maxContentRequestBytes}))
		}
		return responded(apiError(c, http.StatusBadRequest, CodeBadRequest, "invalid JSON: "+err.Error(), nil))
	}
	return nil
}

// contentError answers the errors of the content changes
func (rs *RestService) contentError(c *echo.Context, err error) error {
	var editErr *collab.EditError
	switch {
	case errors.As(err, &editErr):
		details := map[string]any{editIndexDetailKey: editErr.Index, editReasonDetailKey: editErr.Code}
		if editErr.Hint != "" {
			details["hint"] = editErr.Hint
		}
		switch editErr.Code {
		case collab.EditNotFound:
			return apiError(c, http.StatusConflict, CodeEditNotFound, err.Error(), details)
		case collab.EditAmbiguous:
			return apiError(c, http.StatusConflict, CodeEditAmbiguous, err.Error(), details)
		}
		return apiError(c, http.StatusBadRequest, CodeBadRequest, err.Error(), details)
	case errors.Is(err, collab.ErrUnknownRevision):
		return apiError(c, http.StatusConflict, CodeUnknownRevision, err.Error()+", read the document again", nil)
	case errors.Is(err, collab.ErrInvalidRevision):
		return apiError(c, http.StatusBadRequest, CodeBadRequest, err.Error(), nil)
	case errors.Is(err, collab.ErrNotUTF8):
		return apiError(c, http.StatusBadRequest, CodeBadRequest, err.Error(), nil)
	}
	return rs.ReturnError(c, err)
}

// GET /document/{id}/content/: the current text, with the revision in the ETag header
func (rs *RestService) getDocumentContent(c *echo.Context) error {
	id := c.Param(urlParamID)
	d := rs.treeManager.GetDocument(id)
	if d == nil {
		return rs.ReturnNotFound(c, id)
	}
	text, revision, err := rs.sync.ReadContent(d)
	if errors.Is(err, collab.ErrNotUTF8) {
		// not editable: the file as it is, without a revision
		content, err := rs.treeManager.ReadDocumentContent(d)
		if err != nil {
			return rs.ReturnError(c, err)
		}
		return c.String(http.StatusOK, content)
	}
	if err != nil {
		return rs.ReturnError(c, err)
	}
	setRevision(c, revision)
	return c.String(http.StatusOK, text)
}

// POST /document/{id}/edits/: replaces unique search strings, all or none
func (rs *RestService) postDocumentEdits(c *echo.Context) error {
	id := c.Param(urlParamID)
	d := rs.treeManager.GetDocument(id)
	if d == nil {
		return rs.ReturnNotFound(c, id)
	}
	if err := rs.allowContentWrite(c); err != nil {
		return err
	}
	var r EditRequest
	if err := bindLimitedJSON(c, &r); err != nil {
		return err
	}
	if len(r.Edits) == 0 || len(r.Edits) > maxEditsPerRequest {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "edits must have 1 to 100 entries", map[string]any{"maxEdits": maxEditsPerRequest})
	}
	revision, err := rs.sync.ApplyEdits(d, r.Edits, r.BaseRevision, agentOf(c))
	if err != nil {
		return rs.contentError(c, err)
	}
	setRevision(c, revision)
	return c.JSONPretty(http.StatusOK, &RevisionResponse{Revision: revision}, jsonIndent)
}

// PUT /document/{id}/content/: the full text, merged three-way against baseRevision
func (rs *RestService) putDocumentContent(c *echo.Context) error {
	id := c.Param(urlParamID)
	d := rs.treeManager.GetDocument(id)
	if d == nil {
		return rs.ReturnNotFound(c, id)
	}
	if err := rs.allowContentWrite(c); err != nil {
		return err
	}
	var r WriteContentRequest
	if err := bindLimitedJSON(c, &r); err != nil {
		return err
	}
	if r.Content == nil {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "content is required", nil)
	}
	if r.BaseRevision == "" {
		return apiError(c, http.StatusPreconditionRequired, CodeBaseRevisionRequired,
			"baseRevision is required: read the document first (GET /document/{id}/content/, header ETag)", nil)
	}
	revision, err := rs.sync.WriteContent(d, *r.Content, r.BaseRevision, agentOf(c))
	if err != nil {
		return rs.contentError(c, err)
	}
	setRevision(c, revision)
	return c.JSONPretty(http.StatusOK, &RevisionResponse{Revision: revision}, jsonIndent)
}
