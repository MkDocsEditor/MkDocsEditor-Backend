package api

import (
	"errors"
	"net/http"
	"sort"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/activity"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/push"
	"github.com/labstack/echo/v5"
)

const (
	// FeaturePush means the server sends Web Push messages, see docs/push.md
	FeaturePush = "push"

	CodeEndpointNotAllowed = "endpoint-not-allowed"
)

type (
	// PushInfo is the push part of GET /server-info/
	PushInfo struct {
		VAPIDPublicKey string `json:"vapidPublicKey"`
	}

	// PushSubscriptionRequest is the body of POST /push/subscriptions/, the format of the
	// PushSubscription of browsers and of UnifiedPush
	PushSubscriptionRequest struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256DH string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	}

	// PushEndpointRequest is the body of DELETE /push/subscriptions/
	PushEndpointRequest struct {
		Endpoint string `json:"endpoint"`
	}

	PushSubscriptionResponse struct {
		Endpoint   string  `json:"endpoint"`
		CreatedAt  string  `json:"createdAt"`
		LastPushAt *string `json:"lastPushAt"`
		// Current is true for the subscription of the requesting session
		Current bool `json:"current"`
	}

	PushSubscriptionsResponse struct {
		Subscriptions []PushSubscriptionResponse `json:"subscriptions"`
	}
)

// SetPush makes the server send pushes. Call it before Start; without it push is off.
func (rs *RestService) SetPush(n *push.Notifier) {
	rs.push = n
}

// pushInfo is the push part of the server info, nil when push is off
func (rs *RestService) pushInfo() *PushInfo {
	if rs.push == nil {
		return nil
	}
	return &PushInfo{VAPIDPublicKey: rs.push.Sender().PublicKey()}
}

// pushOff answers the push endpoints of a server without push
func (rs *RestService) pushOff(c *echo.Context) error {
	return apiError(c, http.StatusNotFound, CodeNotFound, "push is not enabled on this server", nil)
}

// GET /push/subscriptions/
func (rs *RestService) getPushSubscriptions(c *echo.Context) error {
	if rs.push == nil {
		return rs.pushOff(c)
	}
	return rs.writePushSubscriptions(c)
}

func (rs *RestService) writePushSubscriptions(c *echo.Context) error {
	subs, err := rs.sync.Activity().Subscriptions(UserFromContext(c).ID)
	if err != nil {
		return rs.ReturnError(c, err)
	}
	sessionID := ""
	if s := SessionFromContext(c); s != nil {
		sessionID = s.ID
	}
	out := &PushSubscriptionsResponse{Subscriptions: make([]PushSubscriptionResponse, 0, len(subs))}
	for _, s := range subs {
		r := PushSubscriptionResponse{Endpoint: s.Endpoint, CreatedAt: formatTime(s.CreatedAt), Current: s.SessionID == sessionID}
		if !s.LastPushAt.IsZero() {
			t := formatTime(s.LastPushAt)
			r.LastPushAt = &t
		}
		out.Subscriptions = append(out.Subscriptions, r)
	}
	return c.JSONPretty(http.StatusOK, out, jsonIndent)
}

// POST /push/subscriptions/
func (rs *RestService) postPushSubscription(c *echo.Context) error {
	if rs.push == nil {
		return rs.pushOff(c)
	}
	r := new(PushSubscriptionRequest)
	if err := c.Bind(r); err != nil {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "invalid JSON", nil)
	}
	sender := rs.push.Sender()
	if r.Endpoint == "" {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "endpoint is missing", nil)
	}
	if err := sender.CheckEndpoint(r.Endpoint); err != nil {
		hosts := sender.AllowedHosts()
		sort.Strings(hosts)
		return apiError(c, http.StatusBadRequest, CodeEndpointNotAllowed, err.Error(), map[string]any{"allowedHosts": hosts})
	}
	p256dh, auth, err := push.DecodeKeys(r.Keys.P256DH, r.Keys.Auth)
	if errors.Is(err, push.ErrInvalidKeys) {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, err.Error(), nil)
	}
	session := SessionFromContext(c)
	if session == nil {
		// the auth middleware lets only sessions through
		return apiError(c, http.StatusForbidden, CodeForbidden, "this endpoint needs a session", nil)
	}
	if err = rs.sync.Activity().AddSubscription(activity.Subscription{Endpoint: r.Endpoint, AccountID: UserFromContext(c).ID,
		SessionID: session.ID, P256DH: p256dh, Auth: auth}); err != nil {
		return rs.ReturnError(c, err)
	}
	return rs.writePushSubscriptions(c)
}

// DELETE /push/subscriptions/
func (rs *RestService) deletePushSubscription(c *echo.Context) error {
	if rs.push == nil {
		return rs.pushOff(c)
	}
	r := new(PushEndpointRequest)
	if err := c.Bind(r); err != nil || r.Endpoint == "" {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "the body must be {\"endpoint\": \"…\"}", nil)
	}
	if _, err := rs.sync.Activity().RemoveSubscription(UserFromContext(c).ID, r.Endpoint); err != nil {
		return rs.ReturnError(c, err)
	}
	return rs.writePushSubscriptions(c)
}
