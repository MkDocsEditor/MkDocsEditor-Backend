package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/cmd/global"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/backend"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	"github.com/fatih/color"
	"github.com/pterm/pterm"
	"github.com/spf13/cobra"
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
		setupUI()
		printStartupInfo()

		conf := configuration.CurrentConfig
		treeManager := backend.NewTreeManager(conf.MkDocs)

		if err := backend.CheckStateDir(conf.MkDocs.StateDir, conf.MkDocs.DocsPath); err != nil {
			log.Fatalf("Invalid state directory: %v", err)
		}
		if _, err := backend.ParseTrustedProxies(conf.Server.TrustedProxies); err != nil {
			log.Fatalf("Invalid server.trustedProxies: %v", err)
		}
		authenticator, err := auth.Open(auth.Options{StateDir: conf.MkDocs.StateDir, Server: conf.Server})
		if err != nil {
			log.Fatalf("Unable to open the account database: %v", err)
		}
		defer func() { _ = authenticator.Close() }()
		switch {
		case !authenticator.Enabled():
			_, _ = color.New(color.FgYellow).Println("WARNING: server.auth.disabled is set: there is no authentication, everyone has full access to all files in the document path!")
		case authenticator.SetupRequired():
			log.Printf("No account exists yet. Create the first admin with this one-time setup link (valid 24 hours, `mkdocsrest setup-link` prints a new one):")
			if err := authenticator.PrintSetupLink(); err != nil {
				log.Fatalf("Unable to create a setup link: %v", err)
			}
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

		restService := backend.NewRestService(conf.Server, treeManager, syncServer, authenticator)

		var gitCommitter *backend.GitCommitter
		if conf.Git.Enabled {
			repo, err := backend.OpenGitRepo(conf.MkDocs.DocsPath, backend.GitSignature{Name: conf.Git.CommitterName, Email: conf.Git.CommitterEmail})
			if err != nil {
				log.Fatalf("git is enabled, but: %v", err)
			}
			gitCommitter, err = backend.NewGitCommitter(repo, authenticator, conf.MkDocs.StateDir, backend.GitCommitterOptions{
				AfterLeave:  time.Duration(conf.Git.CommitAfterLeave) * time.Minute,
				AfterIdle:   time.Duration(conf.Git.CommitAfterIdle) * time.Minute,
				MaxInterval: time.Duration(conf.Git.CommitMaxInterval) * time.Minute,
				Push:        conf.Git.Push,
				Remote:      conf.Git.Remote,
				Branch:      conf.Git.Branch,
			})
			if err != nil {
				log.Fatalf("Unable to start the git committer: %v", err)
			}
			gitCommitter.Start(syncServer)
			restService.SetGitCommitter(gitCommitter)
			log.Printf("git commits enabled")
		}

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
		if gitCommitter != nil {
			gitCommitter.Close()
			log.Printf("All changes committed")
		}
	},
}

func setupUI() {
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
