package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestProgressIndicator_NonInteractiveIsSilent verifies that without a TTY
// (the test environment), Start/Stop/Update are no-ops — nothing reaches
// the writer, so CI logs stay clean.
func TestProgressIndicator_NonInteractiveIsSilent(t *testing.T) {
	resetNonInteractive(t)
	resetEnv(t)
	stdin, stdout := nonTTYFile(t)

	var buf bytes.Buffer
	p := newProgressIndicator(&buf, stdin, stdout, "Migrating...")
	p.Start()
	p.Update("phase 2")
	p.Stop()

	if buf.Len() != 0 {
		t.Errorf("non-interactive indicator must not write to its sink; got %q", buf.String())
	}
}

// TestProgressIndicator_NonInteractiveFlagSilencesEvenWithFakeTTY proves the
// --non-interactive flag short-circuits before the TTY check, so users who
// explicitly opt out of interactivity never see the spinner even when
// running attached to a real terminal.
func TestProgressIndicator_NonInteractiveFlagSilencesEvenWithFakeTTY(t *testing.T) {
	resetNonInteractive(t)
	resetEnv(t)
	nonInteractive = true
	stdin, stdout := makeFakeTTYPair(t) // both look like TTYs to isTTY

	var buf bytes.Buffer
	p := newProgressIndicator(&buf, stdin, stdout, "Migrating...")
	p.Start()
	p.Stop()

	if buf.Len() != 0 {
		t.Errorf("--non-interactive must silence the spinner; got %q", buf.String())
	}
}

// TestProgressIndicator_CIEnvSilencesSpinner ensures CI=true disables the
// spinner so CI logs don't fill with rotating-glyph control sequences.
func TestProgressIndicator_CIEnvSilencesSpinner(t *testing.T) {
	resetNonInteractive(t)
	t.Setenv("CI", "true")
	t.Setenv("TERM", "")
	stdin, stdout := makeFakeTTYPair(t)

	var buf bytes.Buffer
	p := newProgressIndicator(&buf, stdin, stdout, "Migrating...")
	p.Start()
	p.Stop()

	if buf.Len() != 0 {
		t.Errorf("CI=true must silence the spinner; got %q", buf.String())
	}
}

// TestProgressIndicator_StopWithoutStartSafe — defensive: calling Stop on
// an indicator that was never Start()ed must not panic. briandowns/spinner
// handles this internally; we lock in the contract via the wrapper.
func TestProgressIndicator_StopWithoutStartSafe(t *testing.T) {
	resetNonInteractive(t)
	resetEnv(t)
	stdin, stdout := nonTTYFile(t)
	p := newProgressIndicator(&bytes.Buffer{}, stdin, stdout, "x")
	// Must not panic.
	p.Stop()
	p.Stop() // and re-stopping must also be safe
}

// TestProgressIndicator_UpdateOnSilentIsNoop confirms Update is safe on a
// silent indicator (where p.spinner is nil) — guards against a future
// refactor that drops the nil check.
func TestProgressIndicator_UpdateOnSilentIsNoop(t *testing.T) {
	resetNonInteractive(t)
	resetEnv(t)
	stdin, stdout := nonTTYFile(t)
	p := newProgressIndicator(&bytes.Buffer{}, stdin, stdout, "x")
	p.Update("new phase") // must not panic
}

// TestProgressSuffix_Redaction locks in the contract that the spinner
// suffix is built via [redact.URL] (not [redact.String]). The bug being
// guarded against: redact.String only matches HTTP(S)-shaped userinfo via
// regex, so an `ssh://user:pass@host/...` URL would leak its password to
// the visible terminal line. We exercise both shapes here.
func TestProgressSuffix_Redaction(t *testing.T) {
	cases := []struct {
		name     string
		src, dst string
		mustHave []string // substrings that MUST appear (e.g., the host)
		mustNot  []string // substrings that MUST NOT appear (the secrets)
	}{
		{
			name:     "https_with_token",
			src:      "https://x-access-token:secret-https@github.com/a/b.git",
			dst:      "https://oauth2:tk-dst@gitlab.com/x/y.git",
			mustHave: []string{"github.com/a/b.git", "gitlab.com/x/y.git", "Migrating", "→"},
			mustNot:  []string{"secret-https", "tk-dst", "x-access-token", "oauth2"},
		},
		{
			// The old redact.String regex would NOT touch this — its anchor
			// is `(?i)https?://`. redact.URL parses the URL and scrubs
			// userinfo regardless of scheme.
			name:     "ssh_with_password_redact_url_only",
			src:      "ssh://alice:hunter2@bitbucket.example.com/team/repo.git",
			dst:      "https://x-access-token:dsttok@github.com/x/y.git",
			mustHave: []string{"bitbucket.example.com/team/repo.git", "github.com/x/y.git"},
			mustNot:  []string{"hunter2", "alice:hunter2", "dsttok"},
		},
		{
			// Conventional `git@host:path` is NOT a URL; redact.URL leaves
			// it unchanged because the bare `git@` userinfo carries no
			// password.
			name:     "ssh_short_form_passes_through",
			src:      "git@github.com:owner/repo.git",
			dst:      "git@gitlab.com:owner/repo.git",
			mustHave: []string{"git@github.com:owner/repo.git", "git@gitlab.com:owner/repo.git"},
			mustNot:  []string{"redacted"}, // shouldn't over-scrub a safe form
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := progressSuffix(tc.src, tc.dst)
			for _, want := range tc.mustHave {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in suffix: %q", want, got)
				}
			}
			for _, banned := range tc.mustNot {
				if strings.Contains(got, banned) {
					t.Errorf("leaked %q in suffix: %q", banned, got)
				}
			}
		})
	}
}

// TestProgressIndicator_ActivePathConstructsSpinner covers the active
// branch of [newProgressIndicator]: when [isInteractiveFn] says the
// streams are interactive AND the writer is not a non-TTY *os.File, the
// constructor MUST build a real briandowns/spinner. Without this test,
// only the silent fallback path was covered.
//
// We assert on the wrapper's internal state (p.spinner != nil) rather
// than on bytes appearing in the writer because briandowns/spinner
// v1.23.2 has its own internal TTY check on s.WriterFile (defaults to
// os.Stdout) that suppresses output under `go test` regardless of what
// io.Writer we pass. Our wrapper's job is to construct the spinner; the
// underlying library decides whether to actually draw — that boundary is
// where the seam ends.
func TestProgressIndicator_ActivePathConstructsSpinner(t *testing.T) {
	resetNonInteractive(t)
	resetEnv(t)
	stdin, stdout := makeFakeTTYPair(t) // flips isInteractiveFn to true

	p := newProgressIndicator(&bytes.Buffer{}, stdin, stdout, "Migrating...")
	if p.spinner == nil {
		t.Fatal("spinner must be constructed when isInteractiveFn=true and writer is non-file")
	}
	if got, want := p.spinner.Suffix, " Migrating..."; got != want {
		t.Errorf("initial suffix = %q, want %q", got, want)
	}

	// Defensive: Start/Stop must not panic on the active path even
	// though briandowns won't actually write under captured stdout.
	p.Start()
	p.Stop()
}

// TestProgressIndicator_WriterTTYGate proves the writer-TTY check: when
// stdin/stdout are TTYs but the writer is a non-TTY *os.File (the
// `gitraft ... 2>migration.log` shape), the indicator falls back to
// silent. The fallback prevents ANSI control sequences from being
// scattered across log files.
func TestProgressIndicator_WriterTTYGate(t *testing.T) {
	resetNonInteractive(t)
	resetEnv(t)
	stdin, stdout := makeFakeTTYPair(t)

	// A pipe-backed file is a real *os.File but not a TTY — exactly the
	// shape of a redirected stderr.
	r, _ := nonTTYFile(t)

	p := newProgressIndicator(r, stdin, stdout, "x")
	if p.spinner != nil {
		t.Error("spinner must be nil when writer is a non-TTY *os.File (e.g. 2>file)")
	}
}

// TestProgressIndicator_UpdateOnActivePathChangesSuffix confirms Update
// on an active wrapper mutates the underlying spinner's Suffix. Pairs
// with [TestProgressIndicator_UpdateOnSilentIsNoop] (no-panic on nil) to
// cover both branches of Update.
func TestProgressIndicator_UpdateOnActivePathChangesSuffix(t *testing.T) {
	resetNonInteractive(t)
	resetEnv(t)
	stdin, stdout := makeFakeTTYPair(t)

	p := newProgressIndicator(&bytes.Buffer{}, stdin, stdout, "phase one")
	if p.spinner == nil {
		t.Fatal("spinner must be active for this test")
	}

	p.Update("phase two — pushing")

	if got, want := p.spinner.Suffix, " phase two — pushing"; got != want {
		t.Errorf("Update did not change suffix; got %q want %q", got, want)
	}
}
