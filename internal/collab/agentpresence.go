package collab

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
)

// defaultAgentPresenceLifetime is how long an agent without a focus stays present in a
// document after its last change
const defaultAgentPresenceLifetime = 60 * time.Second

// defaultAgentFocusLifetime is how long an agent with a focus stays present after its last
// change or focus: a turn of the model often takes longer than a minute
const defaultAgentFocusLifetime = 5 * time.Minute

// Errors of SetAgentFocus
var (
	// ErrNobodyOpen is returned for a focus on a document without connections
	ErrNobodyOpen = errors.New("nobody has this page open")
	// ErrNoAppToken is returned for a focus of a change without an app token
	ErrNoAppToken = errors.New("needs an app token")
	// ErrFocusOutOfRange is returned for a focus beyond the text at its revision
	ErrFocusOutOfRange = errors.New("the focus is outside of the text")
)

// agentPresence is an agent (a change with an app token) shown as a peer of a document
type agentPresence struct {
	agent      Agent
	clientID   string
	instanceID string
	actorID    string
	lastActive time.Time
	timer      *time.Timer
	// focus is where the agent is, nil for none
	focus *AgentFocus
}

// lifetime is how long the presence lasts after its last activity
func (p *agentPresence) lifetime(s *SyncServer) time.Duration {
	if p.focus != nil {
		return s.AgentFocusLifetime
	}
	return s.AgentPresenceLifetime
}

// agentInstanceID derives a stable instance id (32 hex characters) from an app token id
func agentInstanceID(tokenID string) string {
	sum := sha256.Sum256([]byte("agent:" + tokenID))
	return hex.EncodeToString(sum[:16])
}

// touchAgentPresenceLocked shows an agent that just changed the document as a peer, until
// its lifetime after its last activity. Only changes with an app token are agents. focus
// replaces the focus of the agent; nil keeps the one it has. actorID "" keeps the actor.
func (ds *docSession) touchAgentPresenceLocked(agent Agent, actorID string, focus *AgentFocus) {
	if agent.TokenID == "" || ds.closed {
		return
	}
	key := agent.key()
	p := ds.agentPresences[key]
	if p == nil {
		instanceID := agentInstanceID(agent.TokenID)
		p = &agentPresence{agent: agent, clientID: "agent-" + instanceID[:12], instanceID: instanceID}
		if ds.agentPresences == nil {
			ds.agentPresences = map[string]*agentPresence{}
		}
		ds.agentPresences[key] = p
	}
	if actorID != "" {
		p.actorID = actorID
	}
	if focus != nil {
		p.focus = focus
	}
	p.lastActive = time.Now()
	if p.timer != nil {
		p.timer.Stop()
	}
	p.timer = time.AfterFunc(p.lifetime(ds.server), func() { ds.agentPresenceExpired(key, p) })
	ds.broadcastLocked(ds.agentPresenceMessageLocked(p), nil)
}

// agentPresenceExpired ends the presence of an agent that has not changed the document since
func (ds *docSession) agentPresenceExpired(key string, p *agentPresence) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.agentPresences[key] != p || time.Since(p.lastActive) < p.lifetime(ds.server) {
		return
	}
	ds.endAgentPresenceLocked(key, p)
}

// endAgentPresenceLocked removes the presence of an agent, without a ghost
func (ds *docSession) endAgentPresenceLocked(key string, p *agentPresence) {
	if p.timer != nil {
		p.timer.Stop()
	}
	delete(ds.agentPresences, key)
	if !ds.closed {
		ds.broadcastLocked(&PresenceLeaveMessage{Type: MsgPresenceLeave, DocumentID: ds.docID, ClientID: p.clientID, InstanceID: p.instanceID}, nil)
	}
}

// agentPresenceMessageLocked is the presence of an agent: the account's, with the agent and its
// focus, and without selections (automerge-go cannot make cursors)
func (ds *docSession) agentPresenceMessageLocked(p *agentPresence) *PresenceMessage {
	info := ds.server.actorInfo(p.actorID, p.agent.AccountID)
	return &PresenceMessage{
		Type:         MsgPresence,
		DocumentID:   ds.docID,
		ClientID:     p.clientID,
		AccountID:    info.AccountID,
		Username:     info.Username,
		DisplayName:  info.DisplayName,
		Color:        info.Color,
		Avatar:       info.Avatar,
		ActorID:      p.actorID,
		InstanceID:   p.instanceID,
		DeviceName:   p.agent.TokenName,
		LastActiveAt: p.lastActive.UTC().Format(time.RFC3339Nano),
		Selections:   json.RawMessage("[]"),
		Agent:        &AgentInfo{TokenID: p.agent.TokenID, Name: p.agent.TokenName, Focus: p.focus},
	}
}

// agentPeersLocked returns the presences of the agents, for the presence snapshot
func (ds *docSession) agentPeersLocked() []PresenceMessage {
	out := make([]PresenceMessage, 0, len(ds.agentPresences))
	for _, p := range ds.agentPresences {
		out = append(out, *ds.agentPresenceMessageLocked(p))
	}
	return out
}

// SetAgentFocus moves the cursor, selection and viewport of an agent in a document that people
// have open: indexes in code points of the text at revision. It never loads a document, and it
// checks the connections under the same lock as it sets the focus.
func (s *SyncServer) SetAgentFocus(d *wiki.Document, agent Agent, focus AgentFocus) error {
	if agent.TokenID == "" {
		return ErrNoAppToken
	}
	return s.withOpenSession(d, func(ds *docSession) error {
		heads, err := ds.baseHeadsLocked(focus.Revision)
		if err != nil {
			return err
		}
		if heads == nil {
			return ErrInvalidRevision
		}
		fork, err := ds.doc.Fork(heads...)
		if err != nil {
			return err
		}
		text, err := ContentText(fork)
		if err != nil {
			return err
		}
		if !validFocus(focus, utf8.RuneCountInString(text)) {
			return ErrFocusOutOfRange
		}
		focus.Revision = FormatRevision(heads)
		actorID := ""
		if actor, ok := ds.apiActors[agent.key()]; ok {
			actorID = actor.id
		}
		ds.touchAgentPresenceLocked(agent, actorID, &focus)
		return nil
	})
}

// ClearAgentFocus ends the focus and the presence of an agent in a document at once. A document
// nobody has open is ErrNobodyOpen.
func (s *SyncServer) ClearAgentFocus(d *wiki.Document, agent Agent) error {
	if agent.TokenID == "" {
		return ErrNoAppToken
	}
	return s.withOpenSession(d, func(ds *docSession) error {
		if p := ds.agentPresences[agent.key()]; p != nil {
			ds.endAgentPresenceLocked(agent.key(), p)
		}
		return nil
	})
}

// Viewers returns the ids of the accounts with a connection to a document, in no particular
// order and without duplicates. Agents and ghosts have no connection.
func (s *SyncServer) Viewers(d *wiki.Document) []string {
	var out []string
	_ = s.withOpenSession(d, func(ds *docSession) error {
		seen := map[string]bool{}
		for c := range ds.conns {
			if !seen[c.user.ID] {
				seen[c.user.ID] = true
				out = append(out, c.user.ID)
			}
		}
		return nil
	})
	return out
}

// withOpenSession runs f with the session of a document that has connections, holding its lock.
// It never loads a document: without a loaded session or connections it is ErrNobodyOpen.
func (s *SyncServer) withOpenSession(d *wiki.Document, f func(ds *docSession) error) error {
	rel, err := s.relPath(d.Path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	ds := s.sessions[rel]
	s.mu.Unlock()
	if ds == nil {
		return ErrNobodyOpen
	}
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.closed || len(ds.conns) == 0 {
		return ErrNobodyOpen
	}
	return f(ds)
}

// validFocus checks the indexes of a focus against the length of its text
func validFocus(f AgentFocus, length int) bool {
	if f.From < 0 || f.To < f.From || f.To > length {
		return false
	}
	if v := f.Viewport; v != nil {
		if length == 0 {
			return v.Top == 0 && v.Bottom == 0
		}
		return v.Top >= 0 && v.Bottom >= v.Top && v.Bottom < length
	}
	return true
}
