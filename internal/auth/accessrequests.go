package auth

import (
	"database/sql"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Access request states, see docs/auth.md section 8
const (
	AccessRequestPending  = "pending"
	AccessRequestApproved = "approved"
	AccessRequestRejected = "rejected"
	AccessRequestExpired  = "expired"
)

const (
	accessRequestLifetime   = 7 * 24 * time.Hour
	maxPendingAccessRequest = 50
	maxAccessRequestName    = 80
	maxAccessRequestMessage = 500
	maxAccessRequestClaim   = 128
	// accessRequestKeep is longer than the longest invite, so an approved request still
	// knows its invite until that has ended
	accessRequestKeep = 60 * 24 * time.Hour

	settingAccessRequestsEnabled = "access_requests_enabled"
)

// Errors of access requests
var (
	ErrAccessRequestsDisabled    = errors.New("access requests are not accepted")
	ErrInvalidAccessRequest      = errors.New("the access request is unknown")
	ErrTooManyAccessRequests     = errors.New("too many access requests are pending, try again later")
	ErrInvalidAccessRequestName  = errors.New("the name is required and has at most 80 characters, without control characters")
	ErrInvalidAccessRequestText  = errors.New("the message has at most 500 characters, without control characters except line breaks")
	ErrInvalidAccessRequestClaim = errors.New("documentId and invitedBy have at most 128 characters")
	ErrAccessRequestDecided      = errors.New("the access request is already decided or expired")
)

// AccessRequest is the request of a person without an account for an invite.
// DocumentID and InvitedBy are claims of that person: nothing has checked them.
type AccessRequest struct {
	ID         string
	Name       string
	Message    string
	DocumentID string
	InvitedBy  string
	Status     string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	linkID     string
	sealedLink []byte
}

// NewAccessRequest holds the fields a client sends
type NewAccessRequest struct {
	Name       string
	Message    string
	DocumentID string
	InvitedBy  string
}

// AccessRequestsAvailable reports whether the public endpoints accept requests. Without an
// account nobody could answer them.
func (s *Service) AccessRequestsAvailable() bool {
	return !s.cfg.disabled && s.AccessRequestsEnabled() && !s.SetupRequired()
}

// AccessRequestsEnabled returns the instance setting
func (s *Service) AccessRequestsEnabled() bool {
	v, err := s.setting(settingAccessRequestsEnabled)
	return err == nil && v != "false"
}

// SetAccessRequestsEnabled changes the instance setting
func (s *Service) SetAccessRequestsEnabled(on bool, by *User, ri RequestInfo) error {
	if err := setSetting(s.db, settingAccessRequestsEnabled, strconv.FormatBool(on)); err != nil {
		return err
	}
	s.Audit(AuditEvent{Type: AuditSettingsChanged, ActorID: idOf(by), IP: ri.IP, UserAgent: ri.UserAgent,
		Details: map[string]any{"accessRequestsEnabled": on}})
	return nil
}

// OnAccessRequestsChanged registers a listener for changes of the pending requests. It is
// called synchronously and must not block.
func (s *Service) OnAccessRequestsChanged(f func()) {
	s.listenersMu.Lock()
	s.accessRequestListeners = append(s.accessRequestListeners, f)
	s.listenersMu.Unlock()
}

func (s *Service) notifyAccessRequests() {
	s.listenersMu.Lock()
	listeners := append([]func(){}, s.accessRequestListeners...)
	s.listenersMu.Unlock()
	for _, f := range listeners {
		f()
	}
}

// PendingAccessRequests returns the number of pending requests
func (s *Service) PendingAccessRequests() (int, error) {
	if s.cfg.disabled {
		return 0, nil
	}
	return countPendingAccessRequests(s.db, s.now())
}

func countPendingAccessRequests(db querier, now time.Time) (int, error) {
	var n int
	err := db.QueryRow("SELECT COUNT(*) FROM access_requests WHERE status = ? AND expires_at > ?", AccessRequestPending, toMillis(now)).Scan(&n)
	return n, err
}

func cleanAccessRequest(r NewAccessRequest) (NewAccessRequest, error) {
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" || utf8.RuneCountInString(r.Name) > maxAccessRequestName || !utf8.ValidString(r.Name) || strings.ContainsFunc(r.Name, unicode.IsControl) {
		return r, ErrInvalidAccessRequestName
	}
	r.Message = strings.TrimSpace(strings.ReplaceAll(r.Message, "\r\n", "\n"))
	if utf8.RuneCountInString(r.Message) > maxAccessRequestMessage || !utf8.ValidString(r.Message) ||
		strings.ContainsFunc(r.Message, func(c rune) bool { return unicode.IsControl(c) && c != '\n' }) {
		return r, ErrInvalidAccessRequestText
	}
	if len(r.DocumentID) > maxAccessRequestClaim || len(r.InvitedBy) > maxAccessRequestClaim {
		return r, ErrInvalidAccessRequestClaim
	}
	return r, nil
}

// CreateAccessRequest stores a request and returns its token, which is shown only once.
// documentId and invitedBy are stored unchecked, so the result cannot reveal whether they exist.
func (s *Service) CreateAccessRequest(r NewAccessRequest, ri RequestInfo) (*AccessRequest, string, error) {
	r, err := cleanAccessRequest(r)
	if err != nil {
		return nil, "", err
	}
	now := s.now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback() }()
	pending, err := countPendingAccessRequests(tx, now)
	if err != nil {
		return nil, "", err
	}
	if pending >= maxPendingAccessRequest {
		return nil, "", ErrTooManyAccessRequests
	}
	a := &AccessRequest{ID: newID(), Name: r.Name, Message: r.Message, DocumentID: r.DocumentID, InvitedBy: r.InvitedBy,
		Status: AccessRequestPending, CreatedAt: now, ExpiresAt: now.Add(accessRequestLifetime)}
	token := newToken(PrefixAccessRequest)
	if _, err = tx.Exec(`INSERT INTO access_requests (id, token_hash, name, message, document_id, invited_by, status, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, a.ID, tokenHash(token), a.Name, a.Message, a.DocumentID, a.InvitedBy, a.Status, toMillis(now), toMillis(a.ExpiresAt)); err != nil {
		return nil, "", err
	}
	if err = insertAudit(tx, now, AuditEvent{Type: AuditAccessRequestCreated, IP: ri.IP, UserAgent: ri.UserAgent,
		Details: map[string]any{"requestId": a.ID}}); err != nil {
		return nil, "", err
	}
	if err = tx.Commit(); err != nil {
		return nil, "", err
	}
	s.notifyAccessRequests()
	log.Printf("Access request %s created", a.ID)
	return a, token, nil
}

const accessRequestColumns = "id, name, message, document_id, invited_by, status, created_at, expires_at, link_id, invite_token"

// scanAccessRequest reads a request and works out its current state, which depends on the
// clock and on its invite
func (s *Service) scanAccessRequest(row interface{ Scan(...any) error }) (*AccessRequest, error) {
	var a AccessRequest
	var created, expires int64
	if err := row.Scan(&a.ID, &a.Name, &a.Message, &a.DocumentID, &a.InvitedBy, &a.Status, &created, &expires, &a.linkID, &a.sealedLink); err != nil {
		return nil, err
	}
	a.CreatedAt, a.ExpiresAt = fromMillis(created), fromMillis(expires)
	if a.Status == AccessRequestPending && !s.now().Before(a.ExpiresAt) {
		a.Status = AccessRequestExpired
	}
	return &a, nil
}

// resolveApproved applies the state of the invite to an approved request. It returns whether
// the invite can still be redeemed.
func (s *Service) resolveApproved(db querier, a *AccessRequest) (bool, error) {
	if a.Status != AccessRequestApproved {
		return false, nil
	}
	l, err := s.scanLink(db.QueryRow("SELECT "+linkColumns+" FROM links WHERE id = ?", a.linkID))
	if errors.Is(err, sql.ErrNoRows) {
		a.Status = AccessRequestExpired
		return false, nil
	}
	if err != nil {
		return false, err
	}
	switch l.Status {
	case LinkExpired:
		a.Status = AccessRequestExpired
	case LinkRevoked:
		a.Status = AccessRequestRejected
	}
	return l.Status == LinkPending, nil
}

// AccessRequestStatus returns the state of the request of a token, and the invite token while
// the request is approved and the invite can be redeemed.
func (s *Service) AccessRequestStatus(token string) (string, string, error) {
	if !validTokenFormat(token, PrefixAccessRequest) {
		return "", "", ErrInvalidAccessRequest
	}
	a, err := s.scanAccessRequest(s.db.QueryRow("SELECT "+accessRequestColumns+" FROM access_requests WHERE token_hash = ?", tokenHash(token)))
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrInvalidAccessRequest
	}
	if err != nil {
		return "", "", err
	}
	redeemable, err := s.resolveApproved(s.db, a)
	if err != nil {
		return "", "", err
	}
	if !redeemable {
		return a.Status, "", nil
	}
	invite, err := unseal(s.keys.accessRequestInvite, a.sealedLink, a.ID)
	if err != nil {
		return "", "", err
	}
	return a.Status, string(invite), nil
}

// AccessRequests lists the requests, newest first. status filters by state when not empty.
func (s *Service) AccessRequests(status string) ([]*AccessRequest, error) {
	rows, err := s.db.Query("SELECT " + accessRequestColumns + " FROM access_requests ORDER BY created_at DESC, id")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	all := []*AccessRequest{}
	for rows.Next() {
		a, err := s.scanAccessRequest(rows)
		if err != nil {
			return nil, err
		}
		all = append(all, a)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := []*AccessRequest{}
	for _, a := range all {
		if _, err = s.resolveApproved(s.db, a); err != nil {
			return nil, err
		}
		if status == "" || a.Status == status {
			out = append(out, a)
		}
	}
	return out, nil
}

// accessRequestForDecision returns a request in a transaction, ErrNotFound if it does not exist
func (s *Service) accessRequestForDecision(tx *sql.Tx, id string) (*AccessRequest, error) {
	a, err := s.scanAccessRequest(tx.QueryRow("SELECT "+accessRequestColumns+" FROM access_requests WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// ApproveAccessRequest creates a normal invite link with the name of the request as its note.
func (s *Service) ApproveAccessRequest(by *User, id, role string, lifetime time.Duration, ri RequestInfo) (*AccessRequest, *Link, string, error) {
	if err := ValidateRole(role); err != nil {
		return nil, nil, "", err
	}
	if lifetime <= 0 {
		lifetime = DefaultInviteLifetime
	}
	if lifetime > MaxInviteLifetime {
		lifetime = MaxInviteLifetime
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, nil, "", err
	}
	defer func() { _ = tx.Rollback() }()
	a, err := s.accessRequestForDecision(tx, id)
	if err != nil {
		return nil, nil, "", err
	}
	if a.Status != AccessRequestPending {
		return nil, nil, "", ErrAccessRequestDecided
	}
	now := s.now().UTC()
	l := &Link{ID: newID(), Kind: LinkInvite, Role: role, Note: a.Name, CreatedAt: now, ExpiresAt: now.Add(lifetime)}
	token, err := s.insertLink(tx, l, by.ID)
	if err != nil {
		return nil, nil, "", err
	}
	sealed, err := seal(s.keys.accessRequestInvite, []byte(token), a.ID)
	if err != nil {
		return nil, nil, "", err
	}
	if _, err = tx.Exec("UPDATE access_requests SET status = ?, decided_at = ?, decided_by = ?, link_id = ?, invite_token = ? WHERE id = ?",
		AccessRequestApproved, toMillis(now), by.ID, l.ID, sealed, a.ID); err != nil {
		return nil, nil, "", err
	}
	if err = insertAudit(tx, now, AuditEvent{Type: AuditInviteCreated, ActorID: by.ID, IP: ri.IP, UserAgent: ri.UserAgent,
		Details: map[string]any{"linkId": l.ID, "role": role, "expiresAt": l.ExpiresAt}}); err != nil {
		return nil, nil, "", err
	}
	if err = insertAudit(tx, now, AuditEvent{Type: AuditAccessRequestApproved, ActorID: by.ID, IP: ri.IP, UserAgent: ri.UserAgent,
		Details: map[string]any{"requestId": a.ID, "linkId": l.ID, "role": role}}); err != nil {
		return nil, nil, "", err
	}
	if err = tx.Commit(); err != nil {
		return nil, nil, "", err
	}
	s.notifyAccessRequests()
	a.Status = AccessRequestApproved
	l.CreatedBy = s.userRef(by.ID)
	l.Status = LinkPending
	return a, l, token, nil
}

// RejectAccessRequest rejects a pending request. Rejecting it again does nothing.
func (s *Service) RejectAccessRequest(by *User, id string, ri RequestInfo) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	a, err := s.accessRequestForDecision(tx, id)
	if err != nil {
		return err
	}
	if a.Status == AccessRequestRejected {
		return nil
	}
	if a.Status != AccessRequestPending {
		return ErrAccessRequestDecided
	}
	now := s.now().UTC()
	if _, err = tx.Exec("UPDATE access_requests SET status = ?, decided_at = ?, decided_by = ? WHERE id = ?",
		AccessRequestRejected, toMillis(now), by.ID, a.ID); err != nil {
		return err
	}
	if err = insertAudit(tx, now, AuditEvent{Type: AuditAccessRequestRejected, ActorID: by.ID, IP: ri.IP, UserAgent: ri.UserAgent,
		Details: map[string]any{"requestId": a.ID}}); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.notifyAccessRequests()
	return nil
}
