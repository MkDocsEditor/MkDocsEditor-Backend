package backend

import (
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	"gopkg.in/yaml.v3"
	"os"
)

type MkDocsConfigThemePalette struct {
	Primary string `yaml:"primary"`
	Accent  string `yaml:"accent"`
}

type MkDocsConfigTheme struct {
	Name      string                   `yaml:"name"`
	Palette   MkDocsConfigThemePalette `yaml:"palette"`
	CustomDir string                   `yaml:"custom_dir"`
}

type MkDocsConfig struct {
	Copyright          string                 `yaml:"copyright"`
	EditUri            string                 `yaml:"edit_uri"`
	Extra              map[string]interface{} `yaml:",inline"`
	ExtraCss           []string               `yaml:"extra_css"`
	MarkdownExtensions []interface{}          `yaml:"markdown_extensions"`

	RepoName string `yaml:"repo_name"`
	RepoUrl  string `yaml:"repo_url"`

	SiteAuthor      string `yaml:"site_author"`
	SiteDescription string `yaml:"site_description"`
	SiteDir         string `yaml:"site_dir"`
	SiteName        string `yaml:"site_name"`
	SiteUrl         string `yaml:"site_url"`

	Theme MkDocsConfigTheme `yaml:"theme"`
}

func readMkDocsConfig() (MkDocsConfig, error) {
	mkDocsConfigFileContent, err := os.ReadFile(configuration.CurrentConfig.MkDocs.ConfigFile)

	var mkDocsConfig MkDocsConfig
	// Unmarshal the YAML data into the map
	err = yaml.Unmarshal(mkDocsConfigFileContent, &mkDocsConfig)
	return mkDocsConfig, err
}
