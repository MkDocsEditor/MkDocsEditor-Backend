package gitstore

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// the credential of a push replaces the helpers of the git configuration, only for its host
func TestCredentialHelper(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	// a helper of the deployment, like the one of the environment before the OAuth2 connection
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "credential.https://git.example.com.helper")
	t.Setenv("GIT_CONFIG_VALUE_0", `!f() { echo username=old; echo password=stale; }; f`)

	cred := &Credential{URL: "https://git.example.com", Username: "mkdocseditor", Password: "s3cret token"}
	args, env := cred.credentialArgs()
	fill := func(host string) string {
		t.Helper()
		cmd := exec.Command("git", append(args, "credential", "fill")...)
		cmd.Env = append(os.Environ(), append(env, "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false")...)
		cmd.Stdin = strings.NewReader("protocol=https\nhost=" + host + "\n\n")
		out, _ := cmd.CombinedOutput()
		return string(out)
	}
	if out := fill("git.example.com"); !strings.Contains(out, "username=mkdocseditor\n") || !strings.Contains(out, "password=s3cret token\n") {
		t.Errorf("fill = %q", out)
	}
	// other hosts never get the token
	if out := fill("evil.example.org"); strings.Contains(out, "s3cret") {
		t.Errorf("other host got the token: %q", out)
	}
}
