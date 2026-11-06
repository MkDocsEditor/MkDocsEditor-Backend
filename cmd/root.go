package cmd

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/cmd/global"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/api"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/collab"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/gitstore"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/metrics"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/nativemem"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
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

// shutdownTimeout limits the time requests get to finish on shutdown
const shutdownTimeout = 10 * time.Second

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "mkdocsrest",
	Short: "REST Server for MkDocs projects.",
	Long:  `mkdocsrest is the backend companion for the MkDocsEditor project.`,
	// this is the default command to run when no subcommand is specified
	RunE: runServer,
	// Execute prints the error
	SilenceErrors: true,
}

// runServer serves until SIGINT or SIGTERM
func runServer(cmd *cobra.Command, _ []string) error {
	// errors from here on are no usage errors
	cmd.SilenceUsage = true

	if err := configuration.InitConfig(global.CfgFile); err != nil {
		return err
	}
	setupUI()
	printStartupInfo()

	conf := configuration.CurrentConfig
	// before the first document is loaded
	defer nativemem.Start()()
	treeManager := wiki.NewTreeManager(conf.MkDocs)

	if err := collab.CheckStateDir(conf.MkDocs.StateDir, conf.MkDocs.DocsPath); err != nil {
		return fmt.Errorf("invalid state directory: %w", err)
	}
	if _, err := api.ParseTrustedProxies(conf.Server.TrustedProxies); err != nil {
		return fmt.Errorf("invalid server.trustedProxies: %w", err)
	}
	authenticator, err := openAuthenticator(conf)
	if err != nil {
		return err
	}
	defer func() { _ = authenticator.Close() }()

	syncServer, err := collab.NewSyncServer(treeManager, authenticator, conf.MkDocs.StateDir, conf.Server.CORS.AllowedOrigins)
	if err != nil {
		return fmt.Errorf("unable to open the state directory: %w", err)
	}
	defer syncServer.Close()
	if err = collab.ExcludeFromGit(conf.MkDocs.ProjectPath, conf.MkDocs.StateDir); err != nil {
		log.Printf("Unable to add the state directory to .git/info/exclude: %v", err)
	}

	fileWatcher, err := wiki.NewFileWatcher(conf.MkDocs.DocsPath, treeManager.CreateItemTree)
	if err != nil {
		return fmt.Errorf("unable to watch the docs folder: %w", err)
	}
	defer fileWatcher.Close()

	restService := api.NewRestService(conf.Server, treeManager, syncServer, authenticator)

	metricsServer, err := startMetricsServer(conf)
	if err != nil {
		return fmt.Errorf("unable to start the metrics server: %w", err)
	}
	if metricsServer != nil {
		defer metricsServer.Shutdown(context.Background())
	}
	if conf.Server.Metrics.EventLog {
		syncServer.SetEventLog(os.Stdout)
	}

	gitCommitter, err := startGitCommitter(conf, authenticator, syncServer)
	if err != nil {
		return err
	}
	if gitCommitter != nil {
		restService.SetGitCommitter(gitCommitter)
	}
	if metricsServer != nil {
		metrics.Registry.MustRegister(api.NewMetricsCollector(syncServer, treeManager, gitCommitter))
	}

	// shut down cleanly on SIGINT/SIGTERM: stop accepting requests, then flush all documents
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		log.Printf("Shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		restService.Shutdown(shutdownCtx)
	}()

	serveErr := restService.Start()
	syncServer.Close()
	log.Printf("All documents saved")
	if gitCommitter != nil {
		gitCommitter.Close()
		log.Printf("All changes committed")
	}
	return serveErr
}

// openAuthenticator opens the account database and tells the admin what to do if it is empty
func openAuthenticator(conf configuration.Configuration) (*auth.Service, error) {
	authenticator, err := auth.Open(auth.Options{StateDir: conf.MkDocs.StateDir, Server: conf.Server})
	if err != nil {
		return nil, fmt.Errorf("unable to open the account database: %w", err)
	}
	switch {
	case !authenticator.Enabled():
		_, _ = color.New(color.FgYellow).Println("WARNING: server.auth.disabled is set: there is no authentication, everyone has full access to all files in the document path!")
	case authenticator.SetupRequired():
		log.Printf("No account exists yet. Create the first admin with this one-time setup link (valid 24 hours, `mkdocsrest setup-link` prints a new one):")
		if err = authenticator.PrintSetupLink(); err != nil {
			_ = authenticator.Close()
			return nil, fmt.Errorf("unable to create a setup link: %w", err)
		}
	}
	return authenticator, nil
}

// startMetricsServer serves the metrics if they are enabled. It returns nil if not.
func startMetricsServer(conf configuration.Configuration) (*metrics.Server, error) {
	if !conf.Server.Metrics.Enabled {
		return nil, nil
	}
	server, err := metrics.Listen(net.JoinHostPort(conf.Server.Metrics.Host, strconv.Itoa(conf.Server.Metrics.Port)))
	if err != nil {
		return nil, err
	}
	metrics.SetBuildInfo(global.Version, global.Commit)
	go server.Serve()
	log.Printf("Serving metrics at http://%s/metrics", server.Addr())
	return server, nil
}

// startGitCommitter starts committing changes if git is enabled. It returns nil if not.
func startGitCommitter(conf configuration.Configuration, authenticator *auth.Service, syncServer *collab.SyncServer) (*gitstore.Committer, error) {
	if !conf.Git.Enabled {
		return nil, nil
	}
	repo, err := gitstore.Open(conf.MkDocs.DocsPath, gitstore.Signature{Name: conf.Git.CommitterName, Email: conf.Git.CommitterEmail})
	if err != nil {
		return nil, fmt.Errorf("git is enabled, but: %w", err)
	}
	gitCommitter, err := gitstore.NewCommitter(repo, authenticator, conf.MkDocs.StateDir, gitstore.CommitterOptions{
		AfterLeave:  time.Duration(conf.Git.CommitAfterLeave) * time.Minute,
		AfterIdle:   time.Duration(conf.Git.CommitAfterIdle) * time.Minute,
		MaxInterval: time.Duration(conf.Git.CommitMaxInterval) * time.Minute,
		Push:        conf.Git.Push,
		Remote:      conf.Git.Remote,
		Branch:      conf.Git.Branch,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to start the git committer: %w", err)
	}
	syncServer.SetChangeRecorder(gitCommitter)
	gitCommitter.Start(syncServer)
	log.Printf("git commits enabled")
	return gitCommitter, nil
}

func setupUI() {
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
