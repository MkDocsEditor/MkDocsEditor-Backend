package auth

import (
	"bytes"
	"testing"
	"time"
)

func TestAccessRequestTokensAreNotStoredInClear(t *testing.T) {
	s := openTest(t, t.TempDir(), users())
	now := time.Now().UTC()
	s.SetClock(func() time.Time { return now })
	admin := &User{ID: "admin"}
	a, token, err := s.CreateAccessRequest(NewAccessRequest{Name: "Anna"}, RequestInfo{})
	if err != nil {
		t.Fatal(err)
	}
	_, _, invite, err := s.ApproveAccessRequest(admin, a.ID, RoleReader, 0, RequestInfo{})
	if err != nil {
		t.Fatal(err)
	}

	var hash, sealed []byte
	if err = s.db.QueryRow("SELECT token_hash, invite_token FROM access_requests WHERE id = ?", a.ID).Scan(&hash, &sealed); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(hash, []byte(token)) || !bytes.Equal(hash, tokenHash(token)) {
		t.Error("the request token is not stored as its hash")
	}
	if bytes.Contains(sealed, []byte(invite)) || len(sealed) == 0 {
		t.Error("the invite token is not encrypted")
	}
	// bound to its request: the sealed token of one request does not open for another
	if _, err = unseal(s.keys.accessRequestInvite, sealed, "another-request"); err == nil {
		t.Error("the invite token opens for another request")
	}
	if status, got, err := s.AccessRequestStatus(token); err != nil || status != AccessRequestApproved || got != invite {
		t.Errorf("status = %s %s %v", status, got, err)
	}
}

func TestAccessRequestsArePruned(t *testing.T) {
	s := openTest(t, t.TempDir(), users())
	now := time.Now().UTC()
	s.SetClock(func() time.Time { return now })
	_, token, err := s.CreateAccessRequest(NewAccessRequest{Name: "Anna"}, RequestInfo{})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(accessRequestLifetime + accessRequestKeep - time.Hour)
	s.pruneExpired()
	if status, _, err := s.AccessRequestStatus(token); err != nil || status != AccessRequestExpired {
		t.Errorf("before the pruning: %s %v", status, err)
	}
	now = now.Add(2 * time.Hour)
	s.pruneExpired()
	if _, _, err = s.AccessRequestStatus(token); err != ErrInvalidAccessRequest {
		t.Errorf("after the pruning: %v", err)
	}
}
