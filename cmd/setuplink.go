package cmd

import (
	"errors"
	"fmt"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/cmd/global"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	"github.com/spf13/cobra"
)

var setupLinkCmd = &cobra.Command{
	Use:   "setup-link",
	Short: "Print a one-time link to create the first admin account",
	Long: `Creates a new setup link (valid 24 hours) and prints it. Older setup links stop working.
It works while the server is running and fails when an account exists already.

Output:

  setup link: <clientUrl>/setup#mdl_…   (only with server.auth.clientUrl)
  setup token: mdl_…`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := configuration.InitConfig(global.CfgFile); err != nil {
			return err
		}
		conf := configuration.CurrentConfig
		if conf.Server.Auth.Disabled {
			return errors.New("server.auth.disabled is set, there are no accounts")
		}
		s, err := auth.Open(auth.Options{StateDir: conf.MkDocs.StateDir, Server: conf.Server})
		if err != nil {
			return err
		}
		defer func() { _ = s.Close() }()
		if err = s.PrintSetupLink(); errors.Is(err, auth.ErrSetupDone) {
			return fmt.Errorf("%w; ask an admin for an invite link instead", err)
		}
		return err
	},
}

func init() {
	rootCmd.AddCommand(setupLinkCmd)
}
