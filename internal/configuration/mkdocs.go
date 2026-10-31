package configuration

type MkDocsConfiguration struct {
	ProjectPath string   `yaml:"projectPath"`
	ConfigFile  string   `yaml:"configFile"`
	DocsPath    string   `yaml:"docsPath"`
	Blacklist   []string `yaml:"blacklist"`
	// StateDir holds the Automerge state of the documents and the actor bindings.
	// It must not be inside DocsPath. Default: <projectPath>/.mkdocseditor
	StateDir string `yaml:"stateDir"`
}
