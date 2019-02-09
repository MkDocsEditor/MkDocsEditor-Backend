package config

import (
	"github.com/spf13/viper"
	"log"
	"path/filepath"
)

const mkdocsConfigFileDefaultName = "mkdocs.yaml"

type Configuration struct {
	Server ServerConfiguration
	MkDocs MkDocsConfiguration
}

var CurrentConfig Configuration

// one time setup for the configuration file
func init() {
	viper.SetConfigName("config")

	viper.AddConfigPath(".")
	viper.AddConfigPath("/etc/mkdocsrest/")
	viper.AddConfigPath("$HOME/.mkdocsrest")

	if err := viper.ReadInConfig(); err != nil {
		log.Fatalf("Error reading config file, %s", err)
	}
	err := viper.Unmarshal(&CurrentConfig)
	if err != nil {
		log.Fatalf("unable to decode into struct, %v", err)
	}

	setDefaultValues()
}

func setDefaultValues() {
	viper.SetDefault("MkDocs.ConfigFile", filepath.Join(CurrentConfig.MkDocs.ProjectPath, mkdocsConfigFileDefaultName))

	if CurrentConfig.MkDocs.DocsPath == "" {
		CurrentConfig.MkDocs.DocsPath = filepath.Join(CurrentConfig.MkDocs.ProjectPath, "docs")
	}
	if CurrentConfig.MkDocs.ConfigFile == "" {
		CurrentConfig.MkDocs.ConfigFile = filepath.Join(CurrentConfig.MkDocs.ProjectPath, mkdocsConfigFileDefaultName)
	}
}
