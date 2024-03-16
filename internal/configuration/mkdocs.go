package configuration

type MkDocsConfiguration struct {
	ProjectPath string `yaml:"projectPath"`
	ConfigFile  string `yaml:"configFile"`
	DocsPath    string `yaml:"docsPath"`
}
