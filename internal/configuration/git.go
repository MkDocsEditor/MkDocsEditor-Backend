package configuration

import (
	"fmt"
	"os"
	"strings"
)

// GitConfiguration controls the git commits of the backend, see docs/sync-protocol.md section 10
type GitConfiguration struct {
	// Enabled turns on commits (default: false). The docs folder must be inside a git repository.
	Enabled bool `yaml:"enabled"`
	// CommitAfterLeave commits a document when all users have left it for this many minutes (default: 5)
	CommitAfterLeave int `yaml:"commitAfterLeave"`
	// CommitAfterIdle commits a document after this many minutes without a change, even while
	// users are connected (default: 10)
	CommitAfterIdle int `yaml:"commitAfterIdle"`
	// CommitMaxInterval commits a document at least every this many minutes while it is edited
	// continuously (default: 60)
	CommitMaxInterval int `yaml:"commitMaxInterval"`
	// CommitterName and CommitterEmail identify the backend as the committer
	// (default: "MkDocsEditor", "mkdocseditor@mkdocseditor.invalid")
	CommitterName  string `yaml:"committerName"`
	CommitterEmail string `yaml:"committerEmail"`
	// Push runs "git push" after each commit (default: false). Credentials come from OAuth or
	// the environment (GIT_SSH_COMMAND, a credential helper), never from this file.
	Push bool `yaml:"push"`
	// Remote and Branch are passed to "git push" when set (default: git's own defaults)
	Remote string `yaml:"remote"`
	Branch string `yaml:"branch"`
	// OAuth gets the push credential from an OAuth2 application of the git host, connected by an
	// admin in the web app. See docs/sync-protocol.md section 10.
	OAuth GitOAuthConfiguration `yaml:"oauth"`
}

// GitOAuthConfiguration is the OAuth2 application of the git host the push credential comes from
type GitOAuthConfiguration struct {
	// Provider is the kind of git host: "forgejo" (Forgejo and Gitea). Empty turns the connection off.
	Provider string `yaml:"provider"`
	// URL is the base URL of the git host, like https://git.example.com
	URL string `yaml:"url"`
	// ClientID of the OAuth2 application
	ClientID string `yaml:"clientId"`
	// ClientSecretFile holds the client secret. The environment variable
	// MKDOCSREST_GIT_OAUTH_CLIENT_SECRET takes precedence.
	ClientSecretFile string `yaml:"clientSecretFile"`
	// User is the login on the git host that must approve; grants of other users are refused
	// (recommended: a user that may only push to the wiki repository)
	User string `yaml:"user"`
	// AllowAdmin accepts an admin of the git host as the user (default: false). A Forgejo grant
	// has all rights of its user, Forgejo has no scopes.
	AllowAdmin bool `yaml:"allowAdmin"`
	// Repo is owner/name of the repository (default: from the URL of the push remote)
	Repo string `yaml:"repo"`
	// RefreshTokenLifetime is the lifetime of refresh tokens in hours, for Forgejo its
	// REFRESH_TOKEN_EXPIRATION_TIME (default: 730)
	RefreshTokenLifetime int `yaml:"refreshTokenLifetime"`
}

// GitOAuthClientSecretEnv holds the client secret of the OAuth2 application
const GitOAuthClientSecretEnv = "MKDOCSREST_GIT_OAUTH_CLIENT_SECRET" //nolint:gosec // the name of a variable, not a secret

// DefaultGitOAuthRefreshTokenLifetime is the default REFRESH_TOKEN_EXPIRATION_TIME of Forgejo, in hours
const DefaultGitOAuthRefreshTokenLifetime = 730

// Enabled reports whether the OAuth connection is configured
func (o GitOAuthConfiguration) Enabled() bool {
	return o.Provider != ""
}

// ClientSecret reads the client secret from the environment or the file
func (o GitOAuthConfiguration) ClientSecret() (string, error) {
	if v := os.Getenv(GitOAuthClientSecretEnv); v != "" {
		return v, nil
	}
	if o.ClientSecretFile == "" {
		return "", fmt.Errorf("git.oauth needs a client secret: set $%s or git.oauth.clientSecretFile", GitOAuthClientSecretEnv)
	}
	data, err := os.ReadFile(o.ClientSecretFile) //nolint:gosec // path from the configuration
	if err != nil {
		return "", fmt.Errorf("unable to read git.oauth.clientSecretFile: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

const (
	DefaultGitCommitAfterLeave  = 5
	DefaultGitCommitAfterIdle   = 10
	DefaultGitCommitMaxInterval = 60
	DefaultGitCommitterName     = "MkDocsEditor"
	DefaultGitCommitterEmail    = "mkdocseditor@mkdocseditor.invalid"
)

func (g *GitConfiguration) setDefaults() {
	if g.CommitAfterLeave <= 0 {
		g.CommitAfterLeave = DefaultGitCommitAfterLeave
	}
	if g.CommitAfterIdle <= 0 {
		g.CommitAfterIdle = DefaultGitCommitAfterIdle
	}
	if g.CommitMaxInterval <= 0 {
		g.CommitMaxInterval = DefaultGitCommitMaxInterval
	}
	if g.CommitterName == "" {
		g.CommitterName = DefaultGitCommitterName
	}
	if g.CommitterEmail == "" {
		g.CommitterEmail = DefaultGitCommitterEmail
	}
	g.OAuth.URL = strings.TrimRight(g.OAuth.URL, "/")
	if g.OAuth.RefreshTokenLifetime <= 0 {
		g.OAuth.RefreshTokenLifetime = DefaultGitOAuthRefreshTokenLifetime
	}
}
