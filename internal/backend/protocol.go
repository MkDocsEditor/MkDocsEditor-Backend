package backend

import "encoding/json"

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
	}

	// UserInfo describes a user to other users
	UserInfo struct {
		ClientID    string `json:"clientId,omitempty"`
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
		Type        string          `json:"type"`
		DocumentID  string          `json:"documentId"`
		ClientID    string          `json:"clientId,omitempty"`
		Username    string          `json:"username,omitempty"`
		DisplayName string          `json:"displayName,omitempty"`
		Color       string          `json:"color,omitempty"`
		ActorID     string          `json:"actorId,omitempty"`
		Selections  json.RawMessage `json:"selections"`
	}

	PresenceSnapshotMessage struct {
		Type       string            `json:"type"`
		DocumentID string            `json:"documentId"`
		Peers      []PresenceMessage `json:"peers"`
	}

	PresenceLeaveMessage struct {
		Type       string `json:"type"`
		DocumentID string `json:"documentId"`
		ClientID   string `json:"clientId"`
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

// presence selections are a JSON array of {anchor, head} objects with string cursors
type selection struct {
	Anchor string `json:"anchor"`
	Head   string `json:"head"`
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
