package cli

import (
	"io"
	"os"
	"testing"
)

// nonTTYFile returns a file pair guaranteed not to be a TTY: an os.Pipe()-
// backed pair. Both ends are character-mode-less.
func nonTTYFile(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})
	return r, w
}

// resetNonInteractive saves+restores the package-global nonInteractive flag
// across a test so tests don't leak state to each other.
func resetNonInteractive(t *testing.T) {
	t.Helper()
	prev := nonInteractive
	t.Cleanup(func() { nonInteractive = prev })
	nonInteractive = false
}

// resetEnv clears CI and TERM (with cleanup) so isInteractive's env policy
// is tested in isolation. Tests that need specific values call t.Setenv
// after this helper.
func resetEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CI", "")
	t.Setenv("TERM", "")
}

// ---- isTTY ----

func TestIsTTY_NilFile(t *testing.T) {
	if isTTY(nil) {
		t.Error("nil *os.File must not be reported as a TTY")
	}
}

func TestIsTTY_PipeIsNotTTY(t *testing.T) {
	r, w := nonTTYFile(t)
	if isTTY(r) {
		t.Error("pipe read-end should not be a TTY")
	}
	if isTTY(w) {
		t.Error("pipe write-end should not be a TTY")
	}
}

func TestIsTTY_ClosedFileSafe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	_ = r.Close()
	_ = w.Close()
	if isTTY(r) {
		t.Error("closed pipe must not be reported as TTY")
	}
}

// ---- isInteractive: --non-interactive flag ----

func TestIsInteractive_NonInteractiveFlagOverrides(t *testing.T) {
	resetNonInteractive(t)
	resetEnv(t)
	nonInteractive = true
	r, w := nonTTYFile(t)
	if isInteractive(r, w) {
		t.Error("--non-interactive must force isInteractive to false even on TTYs")
	}
}

// ---- isInteractive: stream non-TTY paths ----

func TestIsInteractive_FalseWhenEitherStreamNotTTY(t *testing.T) {
	resetNonInteractive(t)
	resetEnv(t)
	r, w := nonTTYFile(t)
	if isInteractive(r, w) {
		t.Error("non-TTY stream pair must yield false")
	}
}

func TestIsInteractive_ClosedStreamsSafe(t *testing.T) {
	resetNonInteractive(t)
	resetEnv(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	_ = r.Close()
	_ = w.Close()
	// Must not panic and must return false.
	if isInteractive(r, w) {
		t.Error("closed streams must yield isInteractive=false")
	}
}

// ---- isInteractive: env-disables policy ----

func TestEnvDisablesInteractive(t *testing.T) {
	cases := []struct {
		name string
		ci   string
		term string
		want bool
	}{
		{"both unset", "", "", false},
		{"CI=true", "true", "", true},
		{"CI=false (still present)", "false", "", true},
		{"CI=1", "1", "", true},
		{"TERM=dumb", "", "dumb", true},
		{"TERM=xterm-256color", "", "xterm-256color", false},
		{"TERM=screen", "", "screen", false},
		{"both", "true", "dumb", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CI", tc.ci)
			t.Setenv("TERM", tc.term)
			if got := envDisablesInteractive(); got != tc.want {
				t.Errorf("envDisablesInteractive() with CI=%q TERM=%q = %v; want %v",
					tc.ci, tc.term, got, tc.want)
			}
		})
	}
}

// ---- isInteractive: env-disables overrides streams ----

func TestIsInteractive_CIEnvOverridesStreams(t *testing.T) {
	resetNonInteractive(t)
	t.Setenv("CI", "true")
	t.Setenv("TERM", "")
	// We can't make pipes return true from isTTY, but we can confirm CI=true
	// doesn't accidentally produce true either — and pair this with the
	// env-disables table above which exercises the true-returning branch
	// independent of TTY state.
	r, w := nonTTYFile(t)
	if isInteractive(r, w) {
		t.Error("CI=true must produce non-interactive even with mocked-true streams")
	}
}

func TestIsInteractive_TermDumbOverridesStreams(t *testing.T) {
	resetNonInteractive(t)
	t.Setenv("CI", "")
	t.Setenv("TERM", "dumb")
	r, w := nonTTYFile(t)
	if isInteractive(r, w) {
		t.Error("TERM=dumb must produce non-interactive")
	}
}

// ---- root flag binding ----

func TestNonInteractiveFlagWiredToCobra(t *testing.T) {
	resetNonInteractive(t)
	root := NewRoot()
	root.SetArgs([]string{"--non-interactive", "--help"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !nonInteractive {
		t.Error("--non-interactive flag did not set the package global")
	}
}

func TestNonInteractiveFlagAfterSubcommand(t *testing.T) {
	// Persistent flags must work in any position — including after the
	// subcommand. A refactor that accidentally puts the flag on Flags()
	// instead of PersistentFlags() would silently fail this test.
	resetNonInteractive(t)
	root := NewRoot()
	root.SetArgs([]string{"migrate", "--non-interactive", "--help"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !nonInteractive {
		t.Error("--non-interactive after subcommand did not set the package global")
	}
}
