package cli

import "os"

// isTTY reports whether f refers to a terminal device. Uses stdlib only —
// `info.Mode() & os.ModeCharDevice` is the canonical Unix+Windows check
// without pulling in a third-party isatty package.
//
// Returns false for any error (e.g., closed file, missing fd) so callers
// can treat "uncertain" as "not interactive" — the safer default for a
// migration tool that would otherwise block waiting for input.
func isTTY(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// isInteractive reports whether gitraft can safely prompt the user.
//
// Returns false when ANY of the following is true:
//   - the --non-interactive flag is set,
//   - the environment signals a non-interactive context: CI is set
//     (any non-empty value — convention used by GitHub Actions, GitLab,
//     CircleCI, Travis) or TERM=dumb (older Jenkins, raw shells, some
//     Docker exec sessions), OR
//   - either stdin or stdout is not a TTY.
//
// stdin must be a TTY because we'd be reading from it; stdout because we'd
// be writing the prompt. A piped stdin (CI, scripts) or redirected stdout
// (logging to file) silently disables interactivity — the user wouldn't see
// the prompt anyway.
//
// The CI/TERM checks live here (the TTY layer) rather than inside the
// future wizard: the wizard shouldn't have to re-derive "is this
// environment usable" — every interactive feature in gitraft can rely on
// this single source of truth.
//
// The helper takes the streams as parameters rather than referencing
// os.Stdin/os.Stdout directly so tests can substitute pipe-backed files.
func isInteractive(stdin, stdout *os.File) bool {
	if nonInteractive {
		return false
	}
	if envDisablesInteractive() {
		return false
	}
	return isTTY(stdin) && isTTY(stdout)
}

// isInteractiveFn is a package-level seam so tests can swap interactivity
// detection without spinning up a real pty. Production wiring points at
// [isInteractive]. Tests using [resetIsInteractiveFn] save+restore around
// mutations.
var isInteractiveFn = isInteractive

// resetIsInteractiveFn saves+restores the package-global isInteractiveFn
// across a test so tests don't leak state to each other.
func resetIsInteractiveFn(t interface{ Cleanup(func()) }) {
	prev := isInteractiveFn
	t.Cleanup(func() { isInteractiveFn = prev })
}

// envDisablesInteractive checks the environment for signals that the
// process is running unattended even when stdin/stdout happen to be TTYs.
// Factored out so the env policy can be tested without spinning up a pty.
//
// Both signals follow widely-used conventions:
//   - CI: any non-empty value disables interactivity. The presence of the
//     variable (not its content) is the signal — `CI=false` still counts.
//   - TERM=dumb: the terminfo signal that ANSI control sequences won't
//     render. Without it, prompt UI would emit unreadable escape codes.
func envDisablesInteractive() bool {
	if os.Getenv("CI") != "" {
		return true
	}
	if os.Getenv("TERM") == "dumb" {
		return true
	}
	return false
}
