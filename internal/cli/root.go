// Package cli wires together the gitraft command tree.
package cli

import "github.com/spf13/cobra"

// Package-level state for flags that apply globally across subcommands.
// These vars must be assigned ONLY by Cobra's flag-parsing machinery (via
// the BindVar/CountVar bindings in [NewRoot]) — no other package code
// should mutate them, since they affect cross-command behavior and can't
// safely be reset between in-process command invocations.
var (
	verbose        int
	nonInteractive bool
	jsonOutput     bool
)

// JSONMode reports whether the user requested machine-readable JSON output.
// Exposed for cmd/gitraft/main.go so it can format the final exit error as
// a JSON object on stdout (instead of the human-readable "gitraft: ..."
// line on stderr) without re-parsing flags.
func JSONMode() bool { return jsonOutput }

// NewRoot builds the root cobra command with all subcommands attached.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "gitraft",
		Short: "Multi-provider git repository migration tool",
		Long:  "gitraft migrates git repositories across hosting providers while preserving all history, branches, and tags.",
	}
	root.PersistentFlags().CountVarP(&verbose, "verbose", "v", "enable debug logs (-v); repeating is accepted but has no extra effect")
	root.PersistentFlags().BoolVar(&nonInteractive, "non-interactive", false,
		"disable interactive prompts (auto-disabled when stdin/stdout is not a TTY, when CI is set, or when TERM=dumb; cannot be forced on)")
	root.PersistentFlags().BoolVar(&jsonOutput, "json", false,
		"emit NDJSON events on stdout instead of human-readable logs on stderr; implies --non-interactive (no wizard, no spinner)")
	root.AddCommand(newMigrateCmd())
	return root
}
