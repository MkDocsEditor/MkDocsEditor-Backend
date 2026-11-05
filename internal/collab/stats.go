package collab

// Stats is the current state of the sync server
type Stats struct {
	LoadedDocuments int
	DirtyDocuments  int
	Writers         int
	Readers         int
	OnlineUsers     int
	EventListeners  int
}

func (s *SyncServer) Stats() Stats {
	var stats Stats
	users := map[string]struct{}{}
	s.mu.Lock()
	for _, session := range s.sessions {
		session.mu.Lock()
		stats.LoadedDocuments++
		if session.dirty {
			stats.DirtyDocuments++
		}
		for c := range session.conns {
			if c.shadow != nil {
				stats.Readers++
			} else {
				stats.Writers++
			}
			users[c.user.ID] = struct{}{}
		}
		session.mu.Unlock()
	}
	s.mu.Unlock()
	stats.OnlineUsers = len(users)

	s.eventsMu.Lock()
	stats.EventListeners = len(s.eventConns)
	s.eventsMu.Unlock()
	return stats
}
