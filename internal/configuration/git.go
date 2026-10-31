package configuration

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
	// Push runs "git push" after each commit (default: false). Credentials come from the
	// environment (GIT_SSH_COMMAND, a credential helper), never from this file.
	Push bool `yaml:"push"`
	// Remote and Branch are passed to "git push" when set (default: git's own defaults)
	Remote string `yaml:"remote"`
	Branch string `yaml:"branch"`
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
}
