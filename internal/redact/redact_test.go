package redact_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/Ogguz/gitraft/internal/redact"
)

func TestString(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain text", "hello world", "hello world"},
		{"https with token", "https://token@github.com/x/y", "https://redacted@github.com/x/y"},
		{"https with user:pass", "https://alice:secret@host/x", "https://redacted@host/x"},
		{"http with token", "http://api-key@example.com/", "http://redacted@example.com/"},
		{"https without creds", "https://github.com/x/y", "https://github.com/x/y"},
		{"ssh URL not redacted", "ssh://git@host/x", "ssh://git@host/x"},
		{"scp-like SSH not redacted", "git@github.com:org/repo.git", "git@github.com:org/repo.git"},
		{"sentence with embedded URL", "clone of https://t0ken@github.com/org/repo failed: timeout",
			"clone of https://redacted@github.com/org/repo failed: timeout"},
		{"multiple URLs", "from https://a@host1/ to https://b:c@host2/",
			"from https://redacted@host1/ to https://redacted@host2/"},
		{"non-URL @ symbol", "alice@example.com sent it", "alice@example.com sent it"},
		// Case-insensitivity: locked in via (?i) on the regex.
		{"uppercase scheme", "HTTPS://token@github.com/x/y", "HTTPS://redacted@github.com/x/y"},
		{"mixed-case scheme", "Https://user:p@host/", "Https://redacted@host/"},
		// SSH-with-creds via String() — the regex doesn't match SSH at all,
		// so this passes through. The test pins that contract so a future
		// regex widening that unintentionally redacted SSH would also need to
		// retain SSH-with-pass redaction in [URL].
		{"ssh URL with creds (regex skip)", "ssh://user:pass@host/x", "ssh://user:pass@host/x"},
		// URL embedded in newline-separated text (multi-line errors).
		{"multi-line text", "first https://a@host1\nsecond https://b@host2",
			"first https://redacted@host1\nsecond https://redacted@host2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redact.String(tc.in)
			if got != tc.want {
				t.Errorf("String(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"https://github.com/x/y", "https://github.com/x/y"},
		{"https://token@github.com/x/y", "https://redacted@github.com/x/y"},
		{"https://alice:pass@host/y", "https://redacted@host/y"},
		{"http://creds@host/y", "http://redacted@host/y"},
		{"ssh://git@host/x", "ssh://git@host/x"}, // SSH single-token: skipped
		{"git+ssh://git@host/x", "git+ssh://git@host/x"},
		// SSH carrying actual password (not just user) — must be redacted.
		{"ssh://user:pass@host/x", "ssh://redacted@host/x"},
		{"git+ssh://user:pass@host/x", "git+ssh://redacted@host/x"},
		// Query string and fragment must survive redaction.
		{"https://user@host/x?q=1#frag", "https://redacted@host/x?q=1#frag"},
		// Port retained.
		{"https://user@host:8443/x", "https://redacted@host:8443/x"},
		// scp-like with no scheme — passes through (not a parseable URL with userinfo).
		{"git@host:org/repo.git", "git@host:org/repo.git"},
		{"not-a-url", "not-a-url"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := redact.URL(tc.in)
			if got != tc.want {
				t.Errorf("URL(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSensitive(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"token", true},
		{"GITHUB_TOKEN", true},
		{"apiToken", true},
		{"api_key", true},
		{"apikey", true},
		{"password", true},
		{"PASSWORD", true},
		{"app_password", true},
		{"secret", true},
		{"client_secret", true},
		{"Authorization", true},
		{"authorization", true},
		// Substring matching is intentionally broad — these cases lock in the
		// over-redaction trade-off so a future "tighten matching" change
		// surfaces here for explicit re-evaluation.
		{"tokenize", true},
		{"tokenizer", true},
		{"x-auth-token", true},
		{"src", false},
		{"dst", false},
		{"host", false},
		{"path", false},
		{"name", false},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			if got := redact.Sensitive(tc.key); got != tc.want {
				t.Errorf("Sensitive(%q) = %v; want %v", tc.key, got, tc.want)
			}
		})
	}
}

// ---- handler tests ----

func newCapturingHandler() (slog.Handler, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}), &buf
}

func TestHandler_RedactsURLInMessage(t *testing.T) {
	inner, buf := newCapturingHandler()
	logger := slog.New(redact.New(inner))
	logger.Info("clone of https://token@github.com/x/y failed")

	if !strings.Contains(buf.String(), "https://redacted@github.com/x/y") {
		t.Errorf("expected URL userinfo redacted in message; got %s", buf.String())
	}
	if strings.Contains(buf.String(), "token@github.com") {
		t.Errorf("token leaked into message; got %s", buf.String())
	}
}

func TestHandler_RedactsURLInStringAttr(t *testing.T) {
	inner, buf := newCapturingHandler()
	logger := slog.New(redact.New(inner))
	logger.Info("pushing", "dst", "https://x-access-token:abc@github.com/x/y.git")

	if strings.Contains(buf.String(), "abc@github.com") {
		t.Errorf("token leaked through dst attr; got %s", buf.String())
	}
	if !strings.Contains(buf.String(), "https://redacted@github.com") {
		t.Errorf("expected URL redaction in attr; got %s", buf.String())
	}
}

func TestHandler_RedactsSensitiveKeyValue(t *testing.T) {
	inner, buf := newCapturingHandler()
	logger := slog.New(redact.New(inner))
	logger.Info("auth", "token", "ghp_supersecret")

	if strings.Contains(buf.String(), "ghp_supersecret") {
		t.Errorf("token value leaked; got %s", buf.String())
	}
	if !strings.Contains(buf.String(), "[redacted]") {
		t.Errorf("expected [redacted] sentinel; got %s", buf.String())
	}
}

func TestHandler_NonStringAttrPassthrough(t *testing.T) {
	inner, buf := newCapturingHandler()
	logger := slog.New(redact.New(inner))
	logger.Info("counts", "items", 42)

	if !strings.Contains(buf.String(), "items=42") {
		t.Errorf("expected items=42 in output; got %s", buf.String())
	}
}

func TestHandler_PreservesLevelFiltering(t *testing.T) {
	inner, buf := newCapturingHandler()
	logger := slog.New(redact.New(inner))
	logger.Debug("hidden if level were Info")
	if !strings.Contains(buf.String(), "hidden") {
		t.Errorf("inner handler is debug; expected message; got %s", buf.String())
	}
}

func TestHandler_WithAttrsRedacts(t *testing.T) {
	inner, buf := newCapturingHandler()
	base := slog.New(redact.New(inner))
	withAttrs := base.With("token", "ghp_x", "src", "https://t@host/x")

	withAttrs.Info("hi")
	out := buf.String()
	if strings.Contains(out, "ghp_x") {
		t.Errorf("token leaked through With(); got %s", out)
	}
	if strings.Contains(out, "t@host") {
		t.Errorf("URL userinfo leaked through With(); got %s", out)
	}
}

func TestHandler_WithGroupForwardsName(t *testing.T) {
	inner, buf := newCapturingHandler()
	base := slog.New(redact.New(inner))
	grouped := base.WithGroup("auth")
	grouped.Info("ok", "step", "verify")
	if !strings.Contains(buf.String(), "auth.step=verify") {
		t.Errorf("expected grouped attr in output; got %s", buf.String())
	}
}

func TestHandler_WithGroupAndAttrsChained(t *testing.T) {
	inner, buf := newCapturingHandler()
	base := slog.New(redact.New(inner))
	chained := base.WithGroup("auth").With("token", "ghp_x", "url", "https://tok@host/x")
	chained.Info("done")
	out := buf.String()
	if strings.Contains(out, "ghp_x") || strings.Contains(out, "tok@host") {
		t.Errorf("creds leaked through chained With/WithGroup; got %s", out)
	}
}

func TestHandler_EnabledForwards(t *testing.T) {
	innerOpts := &slog.HandlerOptions{Level: slog.LevelWarn}
	inner := slog.NewTextHandler(&bytes.Buffer{}, innerOpts)
	wrapped := redact.New(inner)

	if wrapped.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("wrapper should respect inner level filter (Warn)")
	}
	if !wrapped.Enabled(context.Background(), slog.LevelWarn) {
		t.Error("wrapper should forward Warn enabled")
	}
}

// TestHandler_AnyErrorRedacted closes the bypass where slog.Any("err", err)
// passes a non-string Kind through the handler unredacted. err.Error() is now
// pulled out and run through String() before formatting.
func TestHandler_AnyErrorRedacted(t *testing.T) {
	inner, buf := newCapturingHandler()
	logger := slog.New(redact.New(inner))
	leaky := errors.New("clone failed: https://token@github.com/x/y returned 401")
	logger.Info("oops", "err", leaky)

	out := buf.String()
	if strings.Contains(out, "token@github.com") {
		t.Errorf("error attr leaked credential; got %s", out)
	}
	if !strings.Contains(out, "https://redacted@github.com") {
		t.Errorf("expected URL redaction inside error; got %s", out)
	}
}

// TestHandler_AnyStringerRedacted exercises the fmt.Stringer path on KindAny.
func TestHandler_AnyStringerRedacted(t *testing.T) {
	inner, buf := newCapturingHandler()
	logger := slog.New(redact.New(inner))
	logger.Info("oops", "value", stringerWithCreds("https://token@host/x"))

	out := buf.String()
	if strings.Contains(out, "token@host") {
		t.Errorf("Stringer leaked credentials; got %s", out)
	}
}

type stringerWithCreds string

func (s stringerWithCreds) String() string { return string(s) }

// TestHandler_GroupAttrsRecursivelyRedacted ensures slog.Group()-style
// nested attribute groups also have their members redacted.
func TestHandler_GroupAttrsRecursivelyRedacted(t *testing.T) {
	inner, buf := newCapturingHandler()
	logger := slog.New(redact.New(inner))
	logger.Info("nested",
		slog.Group("auth",
			slog.String("token", "ghp_x"),
			slog.String("src", "https://tok@host/x"),
		),
	)
	out := buf.String()
	if strings.Contains(out, "ghp_x") || strings.Contains(out, "tok@host") {
		t.Errorf("group members leaked; got %s", out)
	}
}

// TestHandler_NilInnerPanics locks in the contract that New refuses a nil
// inner handler — the redacting wrapper would panic on first use otherwise.
func TestHandler_NilInnerPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on nil inner handler")
		}
	}()
	_ = redact.New(nil)
}
