// Package cli wires together the gitraft command tree.
package cli

import "github.com/spf13/cobra"

var verbose int

// NewRoot builds the root cobra command with all subcommands attached.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "gitraft",
		Short: "Multi-provider git repository migration tool",
		Long:  "gitraft migrates git repositories across hosting providers while preserving all history, branches, and tags.",
	}
	root.PersistentFlags().CountVarP(&verbose, "verbose", "v", "increase log verbosity (-v, -vv)")
	root.AddCommand(newMigrateCmd())
	return root
}
