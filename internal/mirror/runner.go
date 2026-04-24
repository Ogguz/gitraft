package mirror

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
)

// CommandRunner executes shell commands. The default implementation
// runs them via os/exec; tests and dry-run use other implementations.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) error
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout

	// Tee stderr so the user sees progress live AND we can include a tail in
	// any returned error — otherwise a scrolled terminal loses the diagnostic.
	var captured bytes.Buffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &captured)

	err := cmd.Run()
	if err == nil {
		return nil
	}
	// Distinguish context cancellation from a real command failure so callers
	// can tell "user hit Ctrl-C" apart from "git rejected the push".
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if tail := lastLines(captured.String(), 5); tail != "" {
		return fmt.Errorf("%w: %s", err, tail)
	}
	return err
}

type dryRunRunner struct {
	logger *slog.Logger
}

func (r dryRunRunner) Run(_ context.Context, name string, args ...string) error {
	r.logger.Info("DRY-RUN", "cmd", strings.Join(append([]string{name}, args...), " "))
	return nil
}

func defaultRunner(dryRun bool, logger *slog.Logger) CommandRunner {
	if dryRun {
		return dryRunRunner{logger: logger}
	}
	return execRunner{}
}

// lastLines returns the last n non-empty lines of s joined by " | ".
// Used to attach a bounded stderr tail to error messages.
func lastLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
