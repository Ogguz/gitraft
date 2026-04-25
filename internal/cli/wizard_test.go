package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
)

// ---- validateRepoURL ----

func TestValidateRepoURL(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"", true},                                    // empty → required
		{"   ", true},                                 // whitespace-only → required
		{"https://github.com/owner/repo.git", false},
		{"git@github.com:owner/repo.git", false},
		{"ssh://git@host/owner/repo.git", false},
		{"not a url", true},
		{"http://", true}, // no host
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			err := validateRepoURL(tc.in)
			if tc.wantErr && err == nil {
				t.Errorf("validateRepoURL(%q) = nil; want error", tc.in)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateRepoURL(%q) = %v; want nil", tc.in, err)
			}
		})
	}
}

func TestValidateRepoURL_FriendlyWrap(t *testing.T) {
	// Non-empty but malformed inputs should surface the friendlier wrap so
	// the inline form error is more actionable than a raw url.Parse string.
	err := validateRepoURL("not a url")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "not a valid git URL") {
		t.Errorf("expected 'not a valid git URL' wrap; got %v", err)
	}
}

// ---- wrapWizardErr ----

func TestWrapWizardErr(t *testing.T) {
	cases := []struct {
		name    string
		in      error
		want    string
		wantNil bool
	}{
		{"nil", nil, "", true},
		{"huh.ErrUserAborted", huh.ErrUserAborted, "wizard cancelled", false},
		{"context.Canceled", context.Canceled, "wizard cancelled", false},
		{"context.DeadlineExceeded", context.DeadlineExceeded, "wizard cancelled", false},
		{"wrapped huh.ErrUserAborted", errors.Join(errors.New("outer"), huh.ErrUserAborted),
			"wizard cancelled", false},
		{"wrapped context.Canceled", errors.Join(errors.New("outer"), context.Canceled),
			"wizard cancelled", false},
		{"generic error", errors.New("disk full"), "wizard: disk full", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wrapWizardErr(tc.in)
			if tc.wantNil {
				if got != nil {
					t.Errorf("wrapWizardErr(nil) = %v; want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected non-nil error")
			}
			if got.Error() != tc.want {
				t.Errorf("wrapWizardErr(%v) = %q; want %q", tc.in, got.Error(), tc.want)
			}
		})
	}
}

// ---- resolveSourceDest ----

func TestResolveSourceDest_TwoArgsBypassesWizard(t *testing.T) {
	resetNonInteractive(t)
	resetEnv(t)
	resetRunWizardFn(t)
	wizardCalled := false
	runWizardFn = func(context.Context, *os.File, *os.File) (wizardResult, error) {
		wizardCalled = true
		return wizardResult{source: "wizard-src", destination: "wizard-dst"}, nil
	}

	logger, _ := testLogger()
	r, w := nonTTYFile(t)
	src, dst, err := resolveSourceDest(
		context.Background(),
		[]string{"https://github.com/x/y.git", "https://gitlab.com/a/b.git"},
		r, w, logger,
	)
	if err != nil {
		t.Fatal(err)
	}
	if src != "https://github.com/x/y.git" || dst != "https://gitlab.com/a/b.git" {
		t.Errorf("got (%q, %q); expected the args verbatim", src, dst)
	}
	if wizardCalled {
		t.Error("wizard must not run when 2 positional args are provided")
	}
}

func TestResolveSourceDest_ZeroArgsNonInteractiveErrors(t *testing.T) {
	resetNonInteractive(t)
	resetEnv(t)
	nonInteractive = true
	logger, _ := testLogger()
	r, w := nonTTYFile(t)

	_, _, err := resolveSourceDest(context.Background(), nil, r, w, logger)
	if err == nil {
		t.Fatal("expected error when 0 args and non-interactive")
	}
	if !strings.Contains(err.Error(), "two URL arguments required") {
		t.Errorf("error should explain the required form; got %v", err)
	}
}

func TestResolveSourceDest_ZeroArgsCIEnvErrors(t *testing.T) {
	resetNonInteractive(t)
	t.Setenv("CI", "true")
	t.Setenv("TERM", "")
	logger, _ := testLogger()
	r, w := nonTTYFile(t)

	_, _, err := resolveSourceDest(context.Background(), nil, r, w, logger)
	if err == nil {
		t.Fatal("expected error when 0 args under CI")
	}
	if !strings.Contains(err.Error(), "two URL arguments required") {
		t.Errorf("error should explain the required form; got %v", err)
	}
}

func TestResolveSourceDest_PipedStdinAndStdoutSymmetric(t *testing.T) {
	// Both streams non-TTY — the gate must fire regardless of which side is
	// piped. Sister to the TTY-detection unit tests; this exercises the
	// gate at the resolveSourceDest layer.
	resetNonInteractive(t)
	resetEnv(t)
	logger, _ := testLogger()
	stdin, _ := nonTTYFile(t)
	_, stdout := nonTTYFile(t)

	_, _, err := resolveSourceDest(context.Background(), nil, stdin, stdout, logger)
	if err == nil {
		t.Fatal("expected error when both streams are non-TTY")
	}
	if !strings.Contains(err.Error(), "two URL arguments required") {
		t.Errorf("error should explain the required form; got %v", err)
	}
}

func TestResolveSourceDest_WizardEmptyResultErrors(t *testing.T) {
	// Hook the wizard to return empty values with nil error — defensive
	// guard at resolveSourceDest must catch this so downstream provider.Parse
	// never sees "".
	resetNonInteractive(t)
	resetEnv(t)
	resetRunWizardFn(t)
	runWizardFn = func(context.Context, *os.File, *os.File) (wizardResult, error) {
		return wizardResult{}, nil
	}
	// Force interactive so the gate doesn't catch us first.
	stdin, stdout := makeFakeTTYPair(t)

	logger, _ := testLogger()
	_, _, err := resolveSourceDest(context.Background(), nil, stdin, stdout, logger)
	if err == nil {
		t.Fatal("expected error when wizard returns empty")
	}
	if !strings.Contains(err.Error(), "non-empty") {
		t.Errorf("error should explain the empty-after-resolve case; got %v", err)
	}
}

func TestResolveSourceDest_HookFiredOnInteractivePath(t *testing.T) {
	// When the streams are TTYs, the wizard hook fires and its return values
	// are returned verbatim. The companion test
	// TestResolveSourceDest_HookReceivesStreams checks the streams; this one
	// confirms the hook return reaches the caller untouched.
	resetNonInteractive(t)
	resetEnv(t)
	resetRunWizardFn(t)
	runWizardFn = func(context.Context, *os.File, *os.File) (wizardResult, error) {
		return wizardResult{source: "src-from-hook", destination: "dst-from-hook"}, nil
	}
	stdin, stdout := makeFakeTTYPair(t)

	logger, _ := testLogger()
	src, dst, err := resolveSourceDest(context.Background(), nil, stdin, stdout, logger)
	if err != nil {
		t.Fatal(err)
	}
	if src != "src-from-hook" || dst != "dst-from-hook" {
		t.Errorf("got (%q, %q); expected hook values", src, dst)
	}
}

func TestResolveSourceDest_HookReceivesStreams(t *testing.T) {
	// Verify the streams resolveSourceDest passes match what the hook
	// receives — guards against the asymmetry that earlier review flagged
	// (wizard reaching for os.Stdin/Stdout instead of the passed streams).
	resetNonInteractive(t)
	resetEnv(t)
	resetRunWizardFn(t)

	var seenStdin, seenStdout *os.File
	runWizardFn = func(_ context.Context, in, out *os.File) (wizardResult, error) {
		seenStdin = in
		seenStdout = out
		return wizardResult{source: "x", destination: "y"}, nil
	}

	stdin, stdout := makeFakeTTYPair(t)
	logger, _ := testLogger()
	if _, _, err := resolveSourceDest(context.Background(), nil, stdin, stdout, logger); err != nil {
		t.Fatal(err)
	}
	if seenStdin != stdin || seenStdout != stdout {
		t.Errorf("hook received different streams than were passed in")
	}
}

// ---- migrate command Args validation ----

func TestMigrateCmd_OneArgRejected(t *testing.T) {
	resetNonInteractive(t)
	resetEnv(t)
	root := NewRoot()
	root.SetArgs([]string{"migrate", "only-one-arg"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err == nil {
		t.Fatal("expected error for 1-arg invocation")
	}
}

func TestMigrateCmd_ThreeArgsRejected(t *testing.T) {
	resetNonInteractive(t)
	resetEnv(t)
	root := NewRoot()
	root.SetArgs([]string{"migrate", "a", "b", "c"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err == nil {
		t.Fatal("expected error for 3-arg invocation")
	}
}

func TestMigrateCmd_ZeroArgsNonInteractiveSurfacesHint(t *testing.T) {
	resetNonInteractive(t)
	resetEnv(t)
	root := NewRoot()
	root.SetArgs([]string{"--non-interactive", "migrate"})
	root.SetOut(io.Discard)
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error from migrate with 0 args + --non-interactive")
	}
	if !strings.Contains(err.Error(), "two URL arguments required") {
		t.Errorf("error should mention the URL-args requirement; got %v", err)
	}
}

func TestMigrateCmd_EmptyStringArgsRejected(t *testing.T) {
	// `gitraft migrate "" ""` — Cobra accepts the count, but the empty-after-
	// resolve guard in resolveSourceDest must catch this so the user gets a
	// clean error instead of a downstream parser-level message.
	resetNonInteractive(t)
	resetEnv(t)
	root := NewRoot()
	root.SetArgs([]string{"--non-interactive", "migrate", "", ""})
	root.SetOut(io.Discard)
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for empty-string args")
	}
	if !strings.Contains(err.Error(), "non-empty") {
		t.Errorf("error should mention non-empty requirement; got %v", err)
	}
}

func TestResolveSourceDest_WizardPathHonorsPipedStreams(t *testing.T) {
	// Replace os.Stdin AND os.Stdout with pipes so isTTY returns false; the
	// helper should bail out without invoking the wizard.
	resetNonInteractive(t)
	resetEnv(t)
	resetRunWizardFn(t)
	wizardCalled := false
	runWizardFn = func(context.Context, *os.File, *os.File) (wizardResult, error) {
		wizardCalled = true
		return wizardResult{}, nil
	}

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
	})

	logger, _ := testLogger()
	if _, _, err := resolveSourceDest(context.Background(), nil, stdinR, stdoutW, logger); err == nil {
		t.Error("expected error when both streams are pipes (non-TTY)")
	}
	if wizardCalled {
		t.Error("wizard must not be invoked when streams are non-TTY")
	}
}

// ---- runWizard ctx-cancel via wrapWizardErr ----

func TestRunWizard_CancelledContextErrorWraps(t *testing.T) {
	// The real runWizard would attempt to start a TUI and the test would block
	// on a non-TTY. Instead, use the wrapWizardErr helper to verify the
	// cancellation taxonomy that runWizard relies on.
	cancelled := wrapWizardErr(context.Canceled)
	if cancelled == nil || cancelled.Error() != "wizard cancelled" {
		t.Errorf("context.Canceled should map to 'wizard cancelled'; got %v", cancelled)
	}
}

// ---- helpers ----

// makeFakeTTYPair returns a stdin/stdout pair plus a t.Cleanup that
// overrides [isInteractiveFn] to claim the streams are interactive. This
// lets wizard-hook tests run in CI where stdin/stdout are pipes and the
// real isTTY would return false.
//
// The streams returned are pipe-backed so any data the production code
// writes to stdout doesn't leak to the test runner's terminal. The
// override is reverted by the test's t.Cleanup chain.
func makeFakeTTYPair(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	stdinR, _, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdinR.Close()
		_ = stdoutW.Close()
	})
	resetIsInteractiveFn(t)
	isInteractiveFn = func(*os.File, *os.File) bool { return true }
	return stdinR, stdoutW
}

// Compile-time verification that bubbletea-context wiring compiles — the
// runWizard implementation depends on tea.WithContext being callable with
// our context type. (This guards against a future tea major bump that
// changes the option signature.)
var _ tea.ProgramOption = tea.WithContext(context.Background())
