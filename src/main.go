package main

import (
	"MkDocsEditor-Backend/src/backend"
	"MkDocsEditor-Backend/src/config"
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

// main entry point
func main() {
	printStartupInfo()

	echoRest := backend.CreateRestService()
	var serverConf = config.CurrentConfig.Server
	echoRest.Logger.Fatal(echoRest.Start(fmt.Sprintf("%s:%d", serverConf.Host, serverConf.Port)))
}

func printStartupInfo() {
	green := color.New(color.FgGreen).PrintfFunc()
	warning := color.New(color.FgYellow).PrintfFunc()

	green(banner, config.CurrentConfig.Server.Host, config.CurrentConfig.Server.Port, config.CurrentConfig.MkDocs.DocsPath)

	var auth = config.CurrentConfig.Server.BasicAuth
	if auth.User == "" && auth.Password == "" {
		warning("WARNING: No basic auth values set in config, unauthorized access to all files in document path is possible!")
	}

	fmt.Println("")
}
