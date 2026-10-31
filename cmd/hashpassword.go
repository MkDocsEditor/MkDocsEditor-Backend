package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
)

var hashPasswordCmd = &cobra.Command{
	Use:   "hash-password",
	Short: "Create a bcrypt password hash for server.auth.users[].passwordHash",
	Long: `Reads a password and prints its bcrypt hash for the passwordHash field of a user.

In a terminal the password is read without echo (twice, to confirm). Otherwise the first
line of stdin is used, for example:

  printf '%s' 'my password' | mkdocsrest hash-password`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		password, err := readPassword()
		if err != nil {
			return err
		}
		if password == "" {
			return errors.New("the password is empty")
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
		if err != nil {
			return err
		}
		fmt.Println(string(hash))
		return nil
	},
}

func readPassword() (string, error) {
	fd := int(os.Stdin.Fd()) // #nosec G115 -- file descriptors fit into an int
	if term.IsTerminal(fd) {
		fmt.Fprint(os.Stderr, "Password: ")
		first, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		fmt.Fprint(os.Stderr, "Repeat password: ")
		second, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		if string(first) != string(second) {
			return "", errors.New("the passwords do not match")
		}
		return string(first), nil
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func init() {
	rootCmd.AddCommand(hashPasswordCmd)
}
