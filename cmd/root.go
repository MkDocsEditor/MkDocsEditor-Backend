package cmd

import (
	"fmt"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/cmd/global"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/backend"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	"github.com/fatih/color"
	"github.com/pterm/pterm"
	"github.com/spf13/cobra"
	"os"
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

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "mkdocsrest",
	Short: "REST Server for MkDocs projects.",
	Long:  `mkdocsrest is the backend companion for the MkDocsEditor project.`,
	// this is the default command to run when no subcommand is specified
	Run: func(cmd *cobra.Command, args []string) {
		setupUi()
		printStartupInfo()

		echoRest := backend.CreateRestService()
		var serverConf = configuration.CurrentConfig.Server
		echoRest.Logger.Fatal(echoRest.Start(fmt.Sprintf("%s:%d", serverConf.Host, serverConf.Port)))
	},
}

func setupUi() {
	//ui.SetDebugEnabled(global.Verbose)

	if global.NoColor {
		pterm.DisableColor()
	}
	if global.NoStyle {
		pterm.DisableStyling()
	}
}

func printStartupInfo() {
	green := color.New(color.FgGreen).PrintfFunc()
	warning := color.New(color.FgYellow).PrintfFunc()

	green(banner, configuration.CurrentConfig.Server.Host, configuration.CurrentConfig.Server.Port, configuration.CurrentConfig.MkDocs.DocsPath)

	var auth = configuration.CurrentConfig.Server.BasicAuth
	if auth.User == "" && auth.Password == "" {
		warning("WARNING: No basic auth values set in configuration, unauthorized access to all files in document path is possible!")
	}

	fmt.Println("")
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	cobra.OnInitialize(func() {
		configuration.InitConfig(global.CfgFile)
	})

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
