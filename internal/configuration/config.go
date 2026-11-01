package configuration

import (
	"log"
	"path/filepath"

	"github.com/mitchellh/go-homedir"
	"github.com/spf13/viper"
)

// default name of the MkDocs configuration file inside the project folder
const mkdocsConfigFileDefaultName = "mkdocs.yml"

// DefaultStateDirName is the name of the state folder inside the project folder
const DefaultStateDirName = ".mkdocseditor"

type Configuration struct {
	Server ServerConfiguration `yaml:"server"`
	MkDocs MkDocsConfiguration `yaml:"mkdocs"`
	Git    GitConfiguration    `yaml:"git"`
}

var CurrentConfig Configuration

// InitConfig does a one time setup for the configuration file
func InitConfig(cfgFile string) {
	viper.SetConfigName("mkdocsrest")

	if cfgFile != "" {
		// Use config file from the flag.
		viper.SetConfigFile(cfgFile)
	} else {
		// Find home directory.
		home, err := homedir.Dir()
		if err != nil {
			//ui.ErrorAndNotify("Path Error", "Couldn't detect home directory: %v", err)
			log.Fatalf("Couldn't detect home directory: %v", err)
		}

		viper.AddConfigPath(".")
		viper.AddConfigPath(home)
		viper.AddConfigPath(home + "/.mkdocsrest")
		viper.AddConfigPath("/etc/mkdocsrest/")
	}

	viper.AutomaticEnv() // read in environment variables that match

	if err := viper.ReadInConfig(); err != nil {
		log.Fatalf("Error reading configuration file, %s", err)
	}
	err := viper.Unmarshal(&CurrentConfig)
	if err != nil {
		log.Fatalf("unable to decode into struct, %v", err)
	}

	setDefaultValues()
}

func setDefaultValues() {
	CurrentConfig.Git.setDefaults()
	if CurrentConfig.MkDocs.DocsPath == "" {
		CurrentConfig.MkDocs.DocsPath = filepath.Join(CurrentConfig.MkDocs.ProjectPath, "docs")
	}
	if CurrentConfig.MkDocs.StateDir == "" {
		CurrentConfig.MkDocs.StateDir = filepath.Join(CurrentConfig.MkDocs.ProjectPath, DefaultStateDirName)
	}
	if CurrentConfig.MkDocs.ConfigFile == "" {
		CurrentConfig.MkDocs.ConfigFile = filepath.Join(CurrentConfig.MkDocs.ProjectPath, mkdocsConfigFileDefaultName)
	}
}
