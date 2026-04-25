// Package cli wires together the gitraft command tree.
package cli

import "github.com/spf13/cobra"

// Package-level state for flags that apply globally across subcommands.
// These vars must be assigned ONLY by Cobra's flag-parsing machinery (via
// the BindVar/CountVar bindings in [NewRoot]) — no other package code
// should mutate them, since they affect cross-command behavior and can't
// safely be reset between in-process command invocations.
//
// No locking: the values are written exactly once per process invocation,
// during cobra.Execute, on the main goroutine before any worker spawns.
// All reads happen later, so the absence of synchronization is safe under
// the current single-shot CLI lifecycle. If gitraft ever grows a long-
// running mode (daemon, in-process command replay), this assumption no
// longer holds and the globals must be replaced by an immutable Config
// snapshot threaded through callers.
//
// TODO (post-v1): three globals is the threshold where threading config
// through callers becomes worth doing. The proposed shape: NewRoot
// returns (*cobra.Command, *Config) with Config carrying typed accessors
// (`JSONMode() bool`, `NonInteractive() bool`, `Verbose() int`); cmd/gitraft
// receives the Config back and passes it where needed instead of
// importing the package-level getter. See type-design review on
// commit 9a51ee9.
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
		"emit NDJSON events on stdout (all log levels including ERROR; stderr is unused under --json) instead of human-readable logs on stderr; "+
			"behaves like --non-interactive (no wizard, no spinner). "+
			"Note: --help and --version still produce plain text — they predate the JSON contract.")
	root.AddCommand(newMigrateCmd())
	return root
}
