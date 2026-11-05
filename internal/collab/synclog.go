package collab

import (
	"encoding/json"
	"io"
	"log"
	"time"
)

// SyncEventType marks the lines of the sync event log, so a log query can select them
const SyncEventType = "sync-event"

// maxLoggedClientName limits the client name of a hello message, which the client chooses
const maxLoggedClientName = 64

// Kinds of sync events, see docs/metrics.md
const (
	SyncEventJoined         = "client-joined"
	SyncEventLeft           = "client-left"
	SyncEventChangesApplied = "changes-applied"
	SyncEventRejected       = "sync-rejected"
	SyncEventExternalChange = "external-change"
	SyncEventFlushed        = "flushed"
	SyncEventFlushFailed    = "flush-failed"
	SyncEventLoaded         = "document-loaded"
	SyncEventLoadFailed     = "document-load-failed"
	SyncEventUnloaded       = "document-unloaded"
	SyncEventDeleted        = "document-deleted"
	SyncEventStateCorrupt   = "state-corrupt"
)

// SyncEvent is one line of the sync event log. It describes what happened to a document and
// never contains document text.
type SyncEvent struct {
	Time string `json:"time"`
	Type string `json:"type"`
	Kind string `json:"kind"`
	// Path of the document, relative to the docs folder. Empty if the event happened before
	// the document was known.
	Path     string `json:"path,omitempty"`
	User     string `json:"user,omitempty"`
	ClientID string `json:"clientId,omitempty"`
	// Client is the name the client sent in its hello message, e.g. web or android
	Client string `json:"client,omitempty"`
	Access string `json:"access,omitempty"`
	// Actor of the last change in a message
	Actor string `json:"actor,omitempty"`
	// Changes is the number of Automerge changes in a sync message
	Changes int `json:"changes,omitempty"`
	// Bytes is the size of the sync message, or of the saved state after a flush
	Bytes int `json:"bytes,omitempty"`
	// Heads is the number of heads of the document after the event. More than one means
	// concurrent edits that no change has merged yet.
	Heads int `json:"heads,omitempty"`
	// Connections is the number of clients connected to the document after the event
	Connections int `json:"connections,omitempty"`
	// DurationMs is how long the server took for the event
	DurationMs float64 `json:"durationMs,omitempty"`
	Reason     string  `json:"reason,omitempty"`
}

// SetEventLog writes the sync events as JSON lines to w. Call it before serving requests.
func (s *SyncServer) SetEventLog(w io.Writer) {
	s.eventLog = log.New(w, "", 0)
}

// logSyncEvent writes an event to the sync event log, if it is enabled
func (s *SyncServer) logSyncEvent(e SyncEvent) {
	if s.eventLog == nil {
		return
	}
	e.Time = time.Now().UTC().Format(time.RFC3339Nano)
	e.Type = SyncEventType
	data, err := json.Marshal(e)
	if err != nil {
		log.Printf("Unable to encode a sync event: %v", err)
		return
	}
	s.eventLog.Println(string(data))
}

// milliseconds returns the time since start for SyncEvent.DurationMs
func milliseconds(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000
}

// shortClientName limits a client name for the log
func shortClientName(name string) string {
	runes := []rune(name)
	if len(runes) > maxLoggedClientName {
		return string(runes[:maxLoggedClientName])
	}
	return name
}

// access describes whether a connection may change its document. Caller holds session.mu.
func (c *wsConn) access() string {
	if c.shadow != nil {
		return "read"
	}
	return "write"
}
