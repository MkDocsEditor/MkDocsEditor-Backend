package auth

import (
	"database/sql"
	"errors"
	"time"
)

// The Forgejo connection for git push: who connected which Forgejo user, and the OAuth2
// tokens, sealed with a key of the server key. See docs/sync-protocol.md section 10.

// gitRemoteSealOwner is the authenticated owner of the sealed tokens
const gitRemoteSealOwner = "git-remote"

type (
	// GitRemote is the stored Forgejo connection
	GitRemote struct {
		Login     string
		IsAdmin   bool
		AvatarURL string
		Repo      string
		// AccessToken and RefreshToken are the OAuth2 tokens
		AccessToken      string
		RefreshToken     string
		AccessExpiresAt  time.Time
		RefreshExpiresAt time.Time
		ConnectedAt      time.Time
		// ConnectedBy is the account id of the admin who connected it
		ConnectedBy   string
		LastRefreshAt time.Time
		// ErrorAt and ErrorMessage are the last failed refresh, zero after a successful one;
		// ErrorPermanent means the grant is gone (invalid_grant)
		ErrorAt        time.Time
		ErrorMessage   string
		ErrorPermanent bool
	}
)

// ErrNoGitRemote means no Forgejo connection is stored
var ErrNoGitRemote = errors.New("no Forgejo connection")

// GitRemote returns the stored Forgejo connection, ErrNoGitRemote if there is none
func (s *Service) GitRemote() (*GitRemote, error) {
	var g GitRemote
	var isAdmin, permanent int
	var access, refresh []byte
	var accessExp, refreshExp, connected, lastRefresh, errorAt int64
	err := s.db.QueryRow(`SELECT login, is_admin, avatar_url, repo, access_token, refresh_token, access_expires_at,
		refresh_expires_at, connected_at, connected_by, last_refresh_at, error_at, error_message, error_permanent
		FROM git_remote WHERE id = 1`).Scan(&g.Login, &isAdmin, &g.AvatarURL, &g.Repo, &access, &refresh, &accessExp,
		&refreshExp, &connected, &g.ConnectedBy, &lastRefresh, &errorAt, &g.ErrorMessage, &permanent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoGitRemote
	}
	if err != nil {
		return nil, err
	}
	a, err := unseal(s.keys.gitRemote, access, gitRemoteSealOwner)
	if err != nil {
		return nil, err
	}
	r, err := unseal(s.keys.gitRemote, refresh, gitRemoteSealOwner)
	if err != nil {
		return nil, err
	}
	g.AccessToken, g.RefreshToken = string(a), string(r)
	g.IsAdmin, g.ErrorPermanent = isAdmin != 0, permanent != 0
	g.AccessExpiresAt, g.RefreshExpiresAt = fromMillis(accessExp), fromMillis(refreshExp)
	g.ConnectedAt, g.LastRefreshAt, g.ErrorAt = fromMillis(connected), fromMillis(lastRefresh), fromMillis(errorAt)
	return &g, nil
}

// SaveGitRemote stores a Forgejo connection, replacing the one before
func (s *Service) SaveGitRemote(g *GitRemote) error {
	access, err := seal(s.keys.gitRemote, []byte(g.AccessToken), gitRemoteSealOwner)
	if err != nil {
		return err
	}
	refresh, err := seal(s.keys.gitRemote, []byte(g.RefreshToken), gitRemoteSealOwner)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT OR REPLACE INTO git_remote (id, login, is_admin, avatar_url, repo, access_token, refresh_token,
		access_expires_at, refresh_expires_at, connected_at, connected_by, last_refresh_at, error_at, error_message, error_permanent)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		g.Login, boolInt(g.IsAdmin), g.AvatarURL, g.Repo, access, refresh, toMillis(g.AccessExpiresAt), toMillis(g.RefreshExpiresAt),
		toMillis(g.ConnectedAt), g.ConnectedBy, toMillis(g.LastRefreshAt), toMillis(g.ErrorAt), g.ErrorMessage, boolInt(g.ErrorPermanent))
	return err
}

// SetGitRemoteError records a failed refresh of the stored connection
func (s *Service) SetGitRemoteError(at time.Time, message string, permanent bool) error {
	_, err := s.db.Exec("UPDATE git_remote SET error_at = ?, error_message = ?, error_permanent = ? WHERE id = 1",
		toMillis(at), message, boolInt(permanent))
	return err
}

// DeleteGitRemote removes the stored connection. It reports whether there was one.
func (s *Service) DeleteGitRemote() (bool, error) {
	res, err := s.db.Exec("DELETE FROM git_remote WHERE id = 1")
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
