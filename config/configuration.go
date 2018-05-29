package config

import (
	"github.com/spf13/viper"
	"log"
)

type Configuration struct {
	Server   ServerConfiguration
	MkDocs   MkDocsConfiguration
	Database DatabaseConfiguration
}

var CurrentConfig Configuration

// one time setup for the configuration file
func Setup() {
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

	log.Printf("database uri is %s", CurrentConfig.Database.ConnectionUri)
	log.Printf("port for this application is %d", CurrentConfig.Server.Port)
}
