package collab

import (
	"bytes"
	"encoding/json"
	"regexp"
)

// ProtocolVersion is the version of the sync protocol, see docs/sync-protocol.md
const ProtocolVersion = 1

// Websocket message types, see docs/sync-protocol.md
const (
	MsgHello            = "hello"
	MsgInitialContent   = "initial-content"
	MsgSyncRequest      = "sync-request"
	MsgPresence         = "presence"
	MsgPresenceSnapshot = "presence-snapshot"
	MsgPresenceLeave    = "presence-leave"
	MsgActor            = "actor"
	MsgHeartbeat        = "heartbeat"
	MsgDocumentRenamed  = "document-renamed"
	MsgDocumentDeleted  = "document-deleted"
	MsgError            = "error"
)

// Error codes of MsgError
const (
	ErrCodeBadRequest   = "bad-request"
	ErrCodeUnauthorized = "unauthorized"
	ErrCodeForeignActor = "foreign-actor"
	ErrCodeReadOnly     = "read-only"
	ErrCodeInternal     = "internal"
)

// Websocket close codes
const (
	CloseNormal          = 1000
	CloseGoingAway       = 1001
	CloseInternalError   = 1011
	CloseUnauthorized    = 4001
	CloseForbidden       = 4003
	CloseNotFound        = 4004
	CloseHeartbeatTimout = 4008
	CloseRenamed         = 4009
	CloseProtocolError   = 4400
)

type (
	// MessageBase holds the fields every message has
	MessageBase struct {
		Type       string `json:"type"`
		DocumentID string `json:"documentId,omitempty"`
	}

	HelloMessage struct {
		Type       string `json:"type"`
		DocumentID string `json:"documentId"`
		ActorID    string `json:"actorId"`
		Ticket     string `json:"ticket,omitempty"`
		Epoch      string `json:"epoch,omitempty"`
		Client     string `json:"client,omitempty"`
		// InstanceID and InstanceColor are optional, see docs/sync-protocol.md section 6
		InstanceID    string `json:"instanceId,omitempty"`
		InstanceColor string `json:"instanceColor,omitempty"`
	}

	// UserInfo describes a user to other users
	UserInfo struct {
		ClientID string `json:"clientId,omitempty"`
		// InstanceID is the instance id the server uses for this connection
		InstanceID  string `json:"instanceId,omitempty"`
		AccountID   string `json:"accountId"`
		Username    string `json:"username"`
		DisplayName string `json:"displayName"`
		Role        string `json:"role,omitempty"`
		Color       string `json:"color"`
	}

	// ActorInfo maps an actor id to its user
	ActorInfo struct {
		Type        string `json:"type,omitempty"`
		DocumentID  string `json:"documentId,omitempty"`
		ActorID     string `json:"actorId"`
		AccountID   string `json:"accountId"`
		Username    string `json:"username"`
		DisplayName string `json:"displayName"`
		Color       string `json:"color"`
	}

	InitialContentMessage struct {
		Type          string      `json:"type"`
		DocumentID    string      `json:"documentId"`
		Epoch         string      `json:"epoch"`
		DocumentState string      `json:"documentState"`
		Content       string      `json:"content"`
		ReadOnly      bool        `json:"readOnly"`
		You           UserInfo    `json:"you"`
		Actors        []ActorInfo `json:"actors"`
	}

	SyncMessage struct {
		Type        string `json:"type"`
		DocumentID  string `json:"documentId"`
		SyncMessage string `json:"syncMessage"`
	}

	PresenceMessage struct {
		Type        string `json:"type"`
		DocumentID  string `json:"documentId"`
		ClientID    string `json:"clientId,omitempty"`
		AccountID   string `json:"accountId,omitempty"`
		Username    string `json:"username,omitempty"`
		DisplayName string `json:"displayName,omitempty"`
		Color       string `json:"color,omitempty"`
		ActorID     string `json:"actorId,omitempty"`
		// InstanceID and DeviceName are stamped by the server, see docs/sync-protocol.md section 6
		InstanceID string `json:"instanceId,omitempty"`
		DeviceName string `json:"deviceName,omitempty"`
		// InstanceColor is the color of this client instance. Color stays that of the account.
		InstanceColor string `json:"instanceColor,omitempty"`
		// LastActiveAt is stamped by the server, see docs/sync-protocol.md section 6
		LastActiveAt string          `json:"lastActiveAt,omitempty"`
		Selections   json.RawMessage `json:"selections"`
		// Viewport is the visible text. From a client, null clears it and a missing one keeps
		// it. From the server, a missing one means there is none.
		Viewport json.RawMessage `json:"viewport,omitempty"`
	}

	PresenceSnapshotMessage struct {
		Type       string            `json:"type"`
		DocumentID string            `json:"documentId"`
		Peers      []PresenceMessage `json:"peers"`
		// LastPositions says where accounts last had a cursor, see docs/sync-protocol.md section 6
		LastPositions []LastPositionInfo `json:"lastPositions"`
		// Ghosts are the client instances that left recently, see docs/sync-protocol.md section 6
		Ghosts []GhostInfo `json:"ghosts"`
	}

	// GhostInfo is a client instance that left, with the time its cursor stays
	GhostInfo struct {
		InstanceID    string          `json:"instanceId"`
		AccountID     string          `json:"accountId"`
		Username      string          `json:"username"`
		DisplayName   string          `json:"displayName"`
		Color         string          `json:"color"`
		InstanceColor string          `json:"instanceColor,omitempty"`
		DeviceName    string          `json:"deviceName,omitempty"`
		Selections    json.RawMessage `json:"selections"`
		Viewport      json.RawMessage `json:"viewport,omitempty"`
		GhostMs       int64           `json:"ghostMs"`
	}

	// LastPositionInfo is a last position with the account it belongs to
	LastPositionInfo struct {
		AccountID    string          `json:"accountId"`
		Username     string          `json:"username"`
		DisplayName  string          `json:"displayName"`
		Color        string          `json:"color"`
		Selections   json.RawMessage `json:"selections"`
		Viewport     json.RawMessage `json:"viewport,omitempty"`
		LastActiveAt string          `json:"lastActiveAt"`
	}

	PresenceLeaveMessage struct {
		Type       string `json:"type"`
		DocumentID string `json:"documentId"`
		ClientID   string `json:"clientId"`
		InstanceID string `json:"instanceId,omitempty"`
		// these are only set if the instance leaves a ghost, see docs/sync-protocol.md section 6
		AccountID     string          `json:"accountId,omitempty"`
		Selections    json.RawMessage `json:"selections,omitempty"`
		Viewport      json.RawMessage `json:"viewport,omitempty"`
		DeviceName    string          `json:"deviceName,omitempty"`
		InstanceColor string          `json:"instanceColor,omitempty"`
		GhostMs       int64           `json:"ghostMs,omitempty"`
	}

	DocumentRenamedMessage struct {
		Type          string `json:"type"`
		DocumentID    string `json:"documentId"`
		NewDocumentID string `json:"newDocumentId"`
	}

	ErrorMessage struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
)

var (
	instanceIDFormat    = regexp.MustCompile(`^[0-9a-f]{32}$`)
	instanceColorFormat = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

// validInstanceID checks an instance id of a client. It may be missing.
func validInstanceID(id string) bool {
	return id == "" || instanceIDFormat.MatchString(id)
}

// validInstanceColor checks an instance color of a client. It may be missing.
func validInstanceColor(color string) bool {
	return color == "" || instanceColorFormat.MatchString(color)
}

// parseInstanceColor reads the instance color of a presence message. The result is nil if the
// color stays as it is, and points to an empty string if it is cleared.
func parseInstanceColor(data []byte) (*string, bool) {
	var m struct {
		InstanceColor json.RawMessage `json:"instanceColor"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, false
	}
	if m.InstanceColor == nil {
		return nil, true
	}
	color := ""
	if bytes.Equal(bytes.TrimSpace(m.InstanceColor), jsonNull) {
		return &color, true
	}
	if err := json.Unmarshal(m.InstanceColor, &color); err != nil || color == "" || !validInstanceColor(color) {
		return nil, false
	}
	return &color, true
}

// presence selections are a JSON array of {anchor, head} objects with string cursors
type selection struct {
	Anchor string `json:"anchor"`
	Head   string `json:"head"`
}

// viewport is the visible text of a client, as cursors
type viewport struct {
	Top    string `json:"top"`
	Bottom string `json:"bottom"`
}

// jsonNull is a viewport that a client clears
var jsonNull = json.RawMessage("null")

// cleanViewport checks a viewport and returns it without anything else the client sent. A
// missing viewport stays nil.
func cleanViewport(raw json.RawMessage) (json.RawMessage, bool) {
	if raw == nil {
		return nil, true
	}
	if bytes.Equal(bytes.TrimSpace(raw), jsonNull) {
		return jsonNull, true
	}
	var v viewport
	if err := json.Unmarshal(raw, &v); err != nil || v.Top == "" || v.Bottom == "" {
		return nil, false
	}
	clean, err := json.Marshal(v)
	return clean, err == nil
}

// hasSelections reports whether valid selections name at least one position
func hasSelections(raw json.RawMessage) bool {
	var sel []selection
	return json.Unmarshal(raw, &sel) == nil && len(sel) > 0
}

// validSelections checks the format of the selections of a presence message
func validSelections(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var sel []selection
	if err := json.Unmarshal(raw, &sel); err != nil {
		return false
	}
	return len(sel) <= 100
}
