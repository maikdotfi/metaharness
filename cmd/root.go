package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// newRootCmd is the base command. With no subcommand it prints help, which lists
// all available subcommands.
func newRootCmd() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "metaharness",
		Short: "Meta Harness command-line interface",
		Long:  "metaharness is the command-line interface for the Meta Harness application.",
		// Without a subcommand, show the help (which lists subcommands).
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}

	rootCmd.AddCommand(newServeCmd())
	rootCmd.AddCommand(newAgentCmd())

	return rootCmd
}

// Execute runs the root command, exiting non-zero on error. main calls this.
func Execute() {
	setupLogging()
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
