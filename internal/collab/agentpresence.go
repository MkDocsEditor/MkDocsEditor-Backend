package collab

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// defaultAgentPresenceLifetime is how long an agent stays present in a document after its
// last change
const defaultAgentPresenceLifetime = 60 * time.Second

// agentPresence is an agent (a change with an app token) shown as a peer of a document
type agentPresence struct {
	agent      Agent
	clientID   string
	instanceID string
	actorID    string
	lastActive time.Time
	timer      *time.Timer
}

// agentInstanceID derives a stable instance id (32 hex characters) from an app token id
func agentInstanceID(tokenID string) string {
	sum := sha256.Sum256([]byte("agent:" + tokenID))
	return hex.EncodeToString(sum[:16])
}

// touchAgentPresenceLocked shows an agent that just changed the document as a peer, until
// AgentPresenceLifetime after its last change. Only changes with an app token are agents.
func (ds *docSession) touchAgentPresenceLocked(agent Agent, actorID string) {
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
	p.actorID = actorID
	p.lastActive = time.Now()
	if p.timer != nil {
		p.timer.Stop()
	}
	p.timer = time.AfterFunc(ds.server.AgentPresenceLifetime, func() { ds.agentPresenceExpired(key, p) })
	ds.broadcastLocked(ds.agentPresenceMessageLocked(p), nil)
}

// agentPresenceExpired ends the presence of an agent that has not changed the document since
func (ds *docSession) agentPresenceExpired(key string, p *agentPresence) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.agentPresences[key] != p || time.Since(p.lastActive) < ds.server.AgentPresenceLifetime {
		return
	}
	delete(ds.agentPresences, key)
	if !ds.closed {
		ds.broadcastLocked(&PresenceLeaveMessage{Type: MsgPresenceLeave, DocumentID: ds.docID, ClientID: p.clientID, InstanceID: p.instanceID}, nil)
	}
}

// agentPresenceMessageLocked is the presence of an agent: the account's, with the agent and
// without a cursor (automerge-go cannot make cursors)
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
		Agent:        &AgentInfo{TokenID: p.agent.TokenID, Name: p.agent.TokenName},
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
