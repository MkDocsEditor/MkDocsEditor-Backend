package configuration

import (
	"fmt"
	"os"
	"path/filepath"

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

// InitConfig reads the configuration file into CurrentConfig
func InitConfig(cfgFile string) error {
	viper.SetConfigName("mkdocsrest")

	if cfgFile != "" {
		// Use config file from the flag.
		viper.SetConfigFile(cfgFile)
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("unable to detect the home directory: %w", err)
		}

		viper.AddConfigPath(".")
		viper.AddConfigPath(home)
		viper.AddConfigPath(filepath.Join(home, ".mkdocsrest"))
		viper.AddConfigPath("/etc/mkdocsrest/")
	}

	viper.AutomaticEnv() // read in environment variables that match

	if err := viper.ReadInConfig(); err != nil {
		return fmt.Errorf("unable to read the configuration file: %w", err)
	}
	if err := viper.Unmarshal(&CurrentConfig); err != nil {
		return fmt.Errorf("unable to decode the configuration file: %w", err)
	}

	setDefaultValues()
	return nil
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
