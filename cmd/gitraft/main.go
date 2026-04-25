// Command gitraft mirrors a git repository between hosting providers,
// preserving full history (branches, tags, LFS objects) and auto-creating
// the destination repo when needed. See README for usage and the
// internal/cli package for command wiring.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Ogguz/gitraft/internal/cli"
	"github.com/Ogguz/gitraft/internal/redact"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := cli.NewRoot()
	root.Version = fmt.Sprintf("%s (commit %s, built %s)", version, commit, date)
	root.SilenceUsage = true  // usage spam on runtime errors is noise
	root.SilenceErrors = true // we own the error printing below

	if err := root.ExecuteContext(ctx); err != nil {
		// In --json mode, emit a single NDJSON event on stdout so scripts
		// can `jq` the failure out of the same stream as the rest of the
		// run's events. The schema lives in [cli.ExitErrorEvent] so it's
		// grep-able and renames are loud (typed struct with JSON tags).
		//
		// If the encoder itself fails — most commonly a closed-pipe write
		// when `gitraft --json | head -n 1` exited early — fall back to
		// stderr so the user still gets *some* signal before exit. The
		// redaction wrap is applied a second time on the fallback path
		// because [cli.WriteExitError] only redacted the bytes that
		// failed to land; the fallback works from the original error.
		if cli.JSONMode() {
			if encErr := cli.WriteExitError(os.Stdout, err); encErr != nil {
				_, _ = fmt.Fprintln(os.Stderr, "gitraft:", redact.String(err.Error()))
			}
		} else {
			_, _ = fmt.Fprintln(os.Stderr, "gitraft:", redact.String(err.Error()))
		}
		os.Exit(1)
	}
}
