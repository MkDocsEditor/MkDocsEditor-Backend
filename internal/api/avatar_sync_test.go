package api

import (
	"testing"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/collab"
)

// A changed avatar reaches the clients that are connected: presence, actors and the online list.
// Ghosts and last positions carry it too.
func TestSyncAvatarReachesConnectedClients(t *testing.T) {
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")
	bobID := e.authn.UserByUsername("bob").ID
	ev := dialEvents(t, e, "carol", false)

	a := newTestClient(t, e, "alice")
	a.connect(id)
	b := newTestClient(t, e, "bob")
	b.connect(id)
	waitPresence(a, "bob without an avatar", func(m map[string]any) bool {
		v, ok := m["avatar"]
		return m["username"] == "bob" && ok && v == nil
	})
	b.mu.Lock()
	if v, ok := b.you["avatar"]; !ok || v != nil || b.you["accountId"] != bobID {
		t.Errorf("you = %v", b.you)
	}
	b.mu.Unlock()

	u, err := e.authn.SetAvatar(bobID, []byte("an image"), nil, auth.RequestInfo{})
	if err != nil {
		t.Fatal(err)
	}
	avatar := *u.Avatar
	e.ss.AccountChanged(bobID)

	waitPresence(a, "bob's new avatar", func(m map[string]any) bool { return m["username"] == "bob" && m["avatar"] == avatar })
	a.waitFor("bob's actor with the avatar", func() bool {
		for _, m := range a.messages {
			if m["type"] == collab.MsgActor && m["actorId"] == b.actorID && m["avatar"] == avatar {
				return true
			}
		}
		return false
	})
	eventually(t, "the online list with the avatar", func() bool {
		m := ev.last(collab.MsgOnline)
		if m == nil {
			return false
		}
		for _, d := range m["documents"].([]any) {
			for _, u := range d.(map[string]any)["users"].([]any) {
				user := u.(map[string]any)
				if user["username"] == "bob" && user["accountId"] == bobID && user["avatar"] == avatar {
					return true
				}
			}
		}
		return false
	})

	// the ghost of bob, in the leave and for a client that comes later
	b.send(map[string]any{"type": collab.MsgPresence, "documentId": id, "selections": []map[string]string{{"anchor": "cursor-1", "head": "cursor-1"}}})
	waitPresence(a, "bob's position", selectionAnchor("cursor-1"))
	b.disconnect()
	leave := lastLeave(a, 1)
	if leave["avatar"] != avatar || leave["ghostMs"] == nil {
		t.Errorf("leave = %v", leave)
	}
	late := newTestClient(t, e, "carol")
	late.connect(id)
	if g := ghostOf(late, leave["instanceId"].(string)); g == nil || g["avatar"] != avatar {
		t.Errorf("ghost = %v", g)
	}
	if p := lastPositionFor(late, bobID); p == nil || p["avatar"] != avatar {
		t.Errorf("last position = %v", p)
	}
}
