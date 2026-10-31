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
	if len(c.MkDocs.Blacklist) != 1 {
		t.Errorf("blacklist = %v", c.MkDocs.Blacklist)
	}
}
