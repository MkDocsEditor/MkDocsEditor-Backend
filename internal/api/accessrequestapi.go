package api

import (
	"net/http"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/references"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	"github.com/labstack/echo/v4"
)

// request and response bodies of access requests, see docs/auth.md section 8
type (
	CreateAccessRequestRequest struct {
		Name       string `json:"name"`
		Message    string `json:"message"`
		DocumentID string `json:"documentId"`
		InvitedBy  string `json:"invitedBy"`
	}

	CreatedAccessRequestResponse struct {
		RequestID string    `json:"requestId"`
		Token     string    `json:"token"`
		Status    string    `json:"status"`
		ExpiresAt time.Time `json:"expiresAt"`
	}

	AccessRequestStatusRequest struct {
		Token string `json:"token"`
	}

	AccessRequestStatusResponse struct {
		Status      string `json:"status"`
		InviteToken string `json:"inviteToken,omitempty"`
	}

	// AccessRequestView is what admins see of a request. The claims of the requester are
	// resolved only here, and are null if they name nothing that exists.
	AccessRequestView struct {
		ID                 string    `json:"id"`
		Name               string    `json:"name"`
		Message            string    `json:"message"`
		Status             string    `json:"status"`
		CreatedAt          time.Time `json:"createdAt"`
		ExpiresAt          time.Time `json:"expiresAt"`
		DocumentID         *string   `json:"documentId"`
		DocumentPath       *string   `json:"documentPath"`
		InvitedByAccountID *string   `json:"invitedByAccountId"`
		InvitedByName      *string   `json:"invitedByName"`
	}

	ApproveAccessRequestRequest struct {
		Role           string `json:"role"`
		ExpiresInHours int    `json:"expiresInHours"`
	}

	ApprovedAccessRequestResponse struct {
		Request *AccessRequestView `json:"request"`
		Invite  *auth.Link         `json:"invite"`
		Token   string             `json:"token"`
		URL     *string            `json:"url"`
	}
)

// The features of GET /server-info/ for join links and access requests
const (
	FeatureJoinPresence   = "join-presence"
	FeatureAccessRequests = "access-requests"
	// FeatureInstancePresence means presence and ghosts are per client instance
	FeatureInstancePresence = "instance-presence"
)

func (rs *RestService) accessRequestsDisabled(c echo.Context) error {
	return apiError(c, http.StatusForbidden, CodeAccessRequestsDisabled, "access requests are not accepted", nil)
}

// POST /auth/access-requests/
func (rs *RestService) createAccessRequest(c echo.Context) error {
	if !rs.authenticator.AccessRequestsAvailable() {
		return rs.accessRequestsDisabled(c)
	}
	if err := rs.authenticator.AllowAccessRequestCreate(c.RealIP()); err != nil {
		return rs.authError(c, err)
	}
	var r CreateAccessRequestRequest
	if err := decodeJSON(c, &r); err != nil {
		return err
	}
	a, token, err := rs.authenticator.CreateAccessRequest(auth.NewAccessRequest{Name: r.Name, Message: r.Message, DocumentID: r.DocumentID, InvitedBy: r.InvitedBy}, requestInfo(c))
	if err != nil {
		return rs.authError(c, err)
	}
	return c.JSONPretty(http.StatusCreated, &CreatedAccessRequestResponse{RequestID: a.ID, Token: token, Status: a.Status, ExpiresAt: a.ExpiresAt}, jsonIndent)
}

// POST /auth/access-requests/status/
func (rs *RestService) accessRequestStatus(c echo.Context) error {
	if !rs.authenticator.AccessRequestsAvailable() {
		return rs.accessRequestsDisabled(c)
	}
	if err := rs.authenticator.AllowAccessRequestStatus(c.RealIP()); err != nil {
		return rs.authError(c, err)
	}
	var r AccessRequestStatusRequest
	if err := decodeJSON(c, &r); err != nil {
		return err
	}
	status, invite, err := rs.authenticator.AccessRequestStatus(r.Token)
	if err != nil {
		return rs.authError(c, err)
	}
	return c.JSONPretty(http.StatusOK, &AccessRequestStatusResponse{Status: status, InviteToken: invite}, jsonIndent)
}

// accessRequestDocument resolves the claimed document, also after a move
func (rs *RestService) accessRequestDocument(id string) *references.ItemRef {
	if id == "" {
		return nil
	}
	if d := rs.treeManager.GetDocument(id); d != nil {
		return references.DocumentRef(d)
	}
	if item, _, ok := rs.sync.ResolveRedirect(id, ""); ok && item.Type == wiki.TypeDocument {
		return item
	}
	return nil
}

func (rs *RestService) accessRequestView(a *auth.AccessRequest) *AccessRequestView {
	v := &AccessRequestView{ID: a.ID, Name: a.Name, Message: a.Message, Status: a.Status, CreatedAt: a.CreatedAt, ExpiresAt: a.ExpiresAt}
	if d := rs.accessRequestDocument(a.DocumentID); d != nil {
		v.DocumentID, v.DocumentPath = &d.ID, &d.Path
	}
	if a.InvitedBy != "" {
		if u := rs.authenticator.UserByID(a.InvitedBy); u != nil && u.Status != auth.StatusDeleted {
			v.InvitedByAccountID, v.InvitedByName = &u.ID, &u.DisplayName
		}
	}
	return v
}

// GET /admin/access-requests/
func (rs *RestService) adminAccessRequests(c echo.Context) error {
	status := c.QueryParam("status")
	switch status {
	case "", auth.AccessRequestPending, auth.AccessRequestApproved, auth.AccessRequestRejected, auth.AccessRequestExpired:
	default:
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "status must be pending, approved, rejected or expired", nil)
	}
	requests, err := rs.authenticator.AccessRequests(status)
	if err != nil {
		return rs.authError(c, err)
	}
	views := make([]*AccessRequestView, 0, len(requests))
	for _, a := range requests {
		views = append(views, rs.accessRequestView(a))
	}
	return c.JSONPretty(http.StatusOK, views, jsonIndent)
}

// POST /admin/access-requests/{id}/approve/
func (rs *RestService) adminApproveAccessRequest(c echo.Context) error {
	var r ApproveAccessRequestRequest
	if err := decodeJSON(c, &r); err != nil {
		return err
	}
	if r.ExpiresInHours < 0 || r.ExpiresInHours > int(auth.MaxInviteLifetime/time.Hour) {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "expiresInHours must be between 1 and 720", nil)
	}
	if auth.ValidateRole(r.Role) != nil {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "role must be admin, editor or reader", nil)
	}
	a, l, token, err := rs.authenticator.ApproveAccessRequest(UserFromContext(c), c.Param(urlParamID), r.Role, time.Duration(r.ExpiresInHours)*time.Hour, requestInfo(c))
	if err != nil {
		return rs.authError(c, err)
	}
	return c.JSONPretty(http.StatusOK, &ApprovedAccessRequestResponse{Request: rs.accessRequestView(a), Invite: l, Token: token, URL: rs.authenticator.LinkURL(auth.LinkInvite, token)}, jsonIndent)
}

// POST /admin/access-requests/{id}/reject/
func (rs *RestService) adminRejectAccessRequest(c echo.Context) error {
	if err := rs.authenticator.RejectAccessRequest(UserFromContext(c), c.Param(urlParamID), requestInfo(c)); err != nil {
		return rs.authError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}
