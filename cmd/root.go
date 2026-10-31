package cmd

import (
	"context"
	"fmt"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/cmd/global"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/backend"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	"github.com/fatih/color"
	"github.com/pterm/pterm"
	"github.com/spf13/cobra"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
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
		configuration.InitConfig(global.CfgFile)
		setupUi()
		printStartupInfo()

		conf := configuration.CurrentConfig
		treeManager := backend.NewTreeManager()

		authenticator, err := auth.NewAuthenticator(conf.Server)
		if err != nil {
			log.Fatalf("Invalid authentication configuration: %v", err)
		}
		if !authenticator.Enabled() {
			_, _ = color.New(color.FgYellow).Println("WARNING: No users configured (server.auth.users), unauthorized access to all files in document path is possible!")
		}

		syncServer, err := backend.NewSyncServer(treeManager, authenticator, conf.MkDocs.StateDir, conf.Server.CORS.AllowedOrigins)
		if err != nil {
			log.Fatalf("Unable to open the state directory: %v", err)
		}
		if err = backend.ExcludeFromGit(conf.MkDocs.ProjectPath, conf.MkDocs.StateDir); err != nil {
			log.Printf("Unable to add the state directory to .git/info/exclude: %v", err)
		}

		fileWatcher, err := backend.NewFileWatcher(conf.MkDocs.DocsPath, treeManager.CreateItemTree)
		if err != nil {
			log.Fatalf("Unable to watch the docs folder: %v", err)
		}
		defer fileWatcher.Close()

		restService := backend.NewRestService(treeManager, syncServer, authenticator)

		// shut down cleanly on SIGINT/SIGTERM: stop accepting requests, then flush all documents
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		go func() {
			<-ctx.Done()
			log.Printf("Shutting down")
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			restService.Shutdown(shutdownCtx)
		}()

		restService.Start()
		syncServer.Close()
		log.Printf("All documents saved")
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
	green(banner, configuration.CurrentConfig.Server.Host, configuration.CurrentConfig.Server.Port, configuration.CurrentConfig.MkDocs.DocsPath)
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	rootCmd.Version = fmt.Sprintf("%s (commit %s, built %s)", global.Version, global.Commit, global.Date)
	rootCmd.PersistentFlags().StringVarP(&global.CfgFile, "config", "c", "", "path to the configuration file (default: ./mkdocsrest.yaml, ~/mkdocsrest.yaml, ~/.mkdocsrest/, /etc/mkdocsrest/)")
	rootCmd.PersistentFlags().BoolVar(&global.NoColor, "no-color", false, "disable colored output")
	rootCmd.PersistentFlags().BoolVar(&global.NoStyle, "no-style", false, "disable styled output")

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
