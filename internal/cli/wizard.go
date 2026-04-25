package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"

	"github.com/Ogguz/gitraft/internal/provider"
)

// wizardResult collects the answers gathered from the interactive prompts.
// Currently just the bare minimum (source + destination); visibility,
// description, --dry-run and provider overrides stay flag-driven so users
// can compose them with the wizard or skip the wizard entirely with two URL
// arguments.
type wizardResult struct {
	source      string
	destination string
}

// runWizardFn is a package-level seam so tests can swap the wizard
// implementation without driving a real TUI. Production wiring points at
// [runWizard]. Tests using [resetRunWizardFn] save+restore around mutations.
var runWizardFn = runWizard

// resetRunWizardFn saves+restores the package-global runWizardFn across a
// test so tests don't leak state to each other.
func resetRunWizardFn(t interface{ Cleanup(func()) }) {
	prev := runWizardFn
	t.Cleanup(func() { runWizardFn = prev })
}

// runWizard prompts the user for source/destination URLs via charmbracelet/huh.
// Returns the populated wizardResult or an error if the user cancelled
// (Ctrl+C / Esc / parent ctx cancel) or the form failed to render.
//
// Validation is per-field so the user sees errors inline as they type
// (huh runs Validate on each blur and again on submit). Both fields go
// through [validateRepoURL], which delegates to [provider.Parse] — keeping
// the wizard's accepted shapes a strict subset of what the rest of the CLI
// accepts. If those two ever drift, the wizard could let through a URL
// that errors at parse time downstream; the [validateRepoURL] doc comment
// pins this contract.
//
// stdin/stdout are passed explicitly (rather than huh defaulting to
// os.Stdin/os.Stdout) so callers and tests can substitute pipe-backed files
// — symmetric with how [isInteractive] takes streams as parameters.
func runWizard(ctx context.Context, stdin, stdout *os.File) (wizardResult, error) {
	var src, dst string
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Key("src").
				Title("Source repository URL").
				Description("Where to migrate from (e.g., https://github.com/owner/repo.git or git@host:owner/repo.git)").
				Value(&src).
				Validate(validateRepoURL),
			huh.NewInput().
				Key("dst").
				Title("Destination repository URL").
				Description("Where to migrate to").
				Value(&dst).
				Validate(validateRepoURL),
		),
	).WithProgramOptions(
		tea.WithContext(ctx),
		tea.WithInput(stdin),
		tea.WithOutput(stdout),
	)

	if err := form.RunWithContext(ctx); err != nil {
		return wizardResult{}, wrapWizardErr(err)
	}

	src = strings.TrimSpace(src)
	dst = strings.TrimSpace(dst)
	// Defensive guard: huh's Validate gates submit, so empty values shouldn't
	// reach here — but guard anyway so a future huh bug or validator
	// misconfiguration surfaces with wizard context, not as a confusing
	// "parse \"\": empty url" three call frames downstream.
	if src == "" || dst == "" {
		return wizardResult{}, errors.New("wizard: returned empty source or destination")
	}
	return wizardResult{source: src, destination: dst}, nil
}

// wrapWizardErr maps an error from huh.Form.RunWithContext into the
// caller-visible wizard error. User-cancellation (Ctrl+C, Esc, SIGINT
// propagated via context) collapses to a single concise message; everything
// else is wrapped with "wizard: " for traceability.
//
// Factored out for direct unit testing — exercising the real wizard
// requires a pty harness, but this helper covers the error-mapping logic
// independently.
func wrapWizardErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, huh.ErrUserAborted) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return errors.New("wizard cancelled")
	}
	return fmt.Errorf("wizard: %w", err)
}

// validateRepoURL is the per-field validator used by the wizard's inputs.
// Empty input returns "required" so the form treats the field as
// must-fill; non-empty input must parse via [provider.Parse] (the same
// parser the rest of the CLI uses), so a user who pastes a malformed URL
// sees the error inline before submitting the form.
//
// CONTRACT: this function MUST stay a strict subset of provider.Parse's
// acceptance — i.e., never accept what provider.Parse would reject. If
// they drift, the wizard could submit a URL that errors at parse time
// downstream. Adding a new acceptance pattern requires updating both.
func validateRepoURL(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("required")
	}
	if _, err := provider.Parse(s); err != nil {
		return fmt.Errorf("not a valid git URL: %w", err)
	}
	return nil
}
