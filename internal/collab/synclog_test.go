package collab

import "testing"

func TestEventLogIsOffByDefault(t *testing.T) {
	var s SyncServer
	if s.eventLog != nil {
		t.Fatal("the event log is on")
	}
	s.logSyncEvent(SyncEvent{Kind: SyncEventJoined})
}
