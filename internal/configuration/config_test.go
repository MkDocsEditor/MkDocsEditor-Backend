package configuration

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mkdocsrest.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInitConfigDefaults(t *testing.T) {
	path := writeConfig(t, `
server:
  host: "127.0.0.1"
  port: 7413
  basicAuth:
    user: "a"
    password: "b"
  cors:
    allowedOrigins:
      - "https://wiki.example.com"
mkdocs:
  projectPath: "/wiki"
  blacklist:
    - "stylesheets"
`)
	CurrentConfig = Configuration{}
	InitConfig(path)

	c := CurrentConfig
	if c.Server.Host != "127.0.0.1" || c.Server.Port != 7413 {
		t.Errorf("server = %+v", c.Server)
	}
	if c.Server.BasicAuth.User != "a" || c.Server.BasicAuth.Password != "b" {
		t.Errorf("basic auth = %+v", c.Server.BasicAuth)
	}
	if len(c.Server.CORS.AllowedOrigins) != 1 || c.Server.CORS.AllowedOrigins[0] != "https://wiki.example.com" {
		t.Errorf("cors = %+v", c.Server.CORS)
	}
	if c.MkDocs.DocsPath != "/wiki/docs" {
		t.Errorf("docs path = %q", c.MkDocs.DocsPath)
	}
	if c.MkDocs.ConfigFile != "/wiki/mkdocs.yml" {
		t.Errorf("config file = %q", c.MkDocs.ConfigFile)
	}
	if c.MkDocs.StateDir != "/wiki/.mkdocseditor" {
		t.Errorf("state dir = %q", c.MkDocs.StateDir)
	}
	if len(c.MkDocs.Blacklist) != 1 {
		t.Errorf("blacklist = %v", c.MkDocs.Blacklist)
	}
	if c.Git.Enabled || c.Git.CommitAfterLeave != 5 || c.Git.CommitAfterIdle != 10 || c.Git.CommitMaxInterval != 60 ||
		c.Git.CommitterName != "MkDocsEditor" || c.Git.Push {
		t.Errorf("git = %+v", c.Git)
	}
}

func TestInitConfigGit(t *testing.T) {
	path := writeConfig(t, `
mkdocs:
  projectPath: "/wiki"
git:
  enabled: true
  commitAfterLeave: 1
  commitAfterIdle: 2
  commitMaxInterval: 3
  committerName: "Wiki"
  committerEmail: "wiki@example.com"
  push: true
  remote: "origin"
  branch: "main"
`)
	CurrentConfig = Configuration{}
	InitConfig(path)
	want := GitConfiguration{Enabled: true, CommitAfterLeave: 1, CommitAfterIdle: 2, CommitMaxInterval: 3,
		CommitterName: "Wiki", CommitterEmail: "wiki@example.com", Push: true, Remote: "origin", Branch: "main"}
	if CurrentConfig.Git != want {
		t.Errorf("git = %+v", CurrentConfig.Git)
	}
}

func TestInitConfigUsers(t *testing.T) {
	path := writeConfig(t, `
server:
  auth:
    users:
      - username: "markus"
        passwordHash: "$2a$12$abc"
        displayName: "Markus"
        email: "markus@example.com"
        role: "admin"
        color: "#e8590c"
      - username: "kid"
        passwordHash: "$2a$12$def"
        role: "reader"
mkdocs:
  projectPath: "/wiki"
`)
	CurrentConfig = Configuration{}
	InitConfig(path)

	users := CurrentConfig.Server.Auth.Users
	if len(users) != 2 {
		t.Fatalf("users = %+v", users)
	}
	want := UserConfiguration{Username: "markus", PasswordHash: "$2a$12$abc", DisplayName: "Markus",
		Email: "markus@example.com", Role: "admin", Color: "#e8590c"}
	if users[0] != want {
		t.Errorf("user = %+v, want %+v", users[0], want)
	}
	if users[1].Role != "reader" {
		t.Errorf("user = %+v", users[1])
	}
}
