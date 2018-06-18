package main

import (
	"mkdocsrest/backend"
	"mkdocsrest/frontend"
	"mkdocsrest/config"
	"fmt"
	"github.com/fatih/color"
)

const banner = `       _     _                         _   
 _____| |_ _| |___ ___ ___ ___ ___ ___| |_ 
|     | '_| . | . |  _|_ -|  _| -_|_ -|  _|
|_|_|_|_,_|___|___|___|___|_| |___|___|_|  

     REST Server for MkDocs projects.
===========================================

Listening at: %s:%d
Document path: %s

`

var green = color.New(color.FgGreen).PrintfFunc()
var warning = color.New(color.FgYellow).PrintfFunc()

// main entry point
func main() {
	printStartupInfo()

	backend.CreateItemTree()

	frontend.SetupRestService()
}

func printStartupInfo() {
	green(banner, config.CurrentConfig.Server.Host, config.CurrentConfig.Server.Port, config.CurrentConfig.MkDocs.DocsPath)

	var auth = config.CurrentConfig.Server.BasicAuth
	if auth.User == "" && auth.Password == "" {
		warning("WARNING: No basic auth values set in config, unauthorized access to all files in document path is possible!")
	}

	fmt.Println("")
}
