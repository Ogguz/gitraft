// Package redact strips sensitive content from text destined for logs and
// stderr — primarily HTTP(S) URLs that have userinfo embedded (where gitraft
// puts auth tokens before passing the URL to git) and attribute values
// keyed by names that look like secrets.
//
// The package is conservative: it over-redacts rather than under-redacts.
// SSH URLs with the conventional single-token `git@host` form pass through
// (the username isn't a credential), but SSH URLs that actually carry a
// password (`ssh://user:pass@host`) are still redacted.
package redact

import (
	"net/url"
	"regexp"
	"strings"
)

// URLUserSentinel is the literal that replaces userinfo in HTTP(S) URLs and
// SSH URLs with passwords. Exported so tests (and downstream consumers) can
// assert against the contract by reference rather than hardcoded string —
// renaming this value is a breaking change to any consumer that pattern-
// matches log output.
//
// Distinct from [AttrSentinel]: this one lives INSIDE a URL's userinfo
// position (`https://redacted@host/...`); [AttrSentinel] is the standalone
// value an entire sensitive attribute is replaced with.
const URLUserSentinel = "redacted"

// AttrSentinel is the literal value that replaces an entire sensitive
// attribute's value (see [Sensitive] for which keys trigger this). Exported
// for the same reason as [URLUserSentinel] — renaming is a breaking change.
const AttrSentinel = "[redacted]"

// urlWithCredsPattern matches HTTP(S) URLs that carry userinfo before the
// host: scheme://anything-without-slash@host. The (?i) flag makes the scheme
// match case-insensitive so `HTTPS://` and `Https://` aren't bypass paths.
//
// We deliberately do NOT match ssh:// schemes here — SSH URLs use a
// conventional `git@` username that's neither a credential nor sensitive.
// SSH URLs that DO carry a password are handled by the [URL] function via
// url.Parse, which can introspect userinfo for `:`.
var urlWithCredsPattern = regexp.MustCompile(`(?i)(https?://)[^/\s@]+@`)

// String walks an arbitrary string and replaces userinfo in any HTTP(S) URL
// it finds with the literal "redacted". Non-URL content passes through
// unchanged. Used for error messages and any free-form text destined for
// stderr or logs.
func String(s string) string {
	if s == "" {
		return s
	}
	return urlWithCredsPattern.ReplaceAllString(s, "${1}"+URLUserSentinel+"@")
}

// URL strips userinfo from a parseable URL. SSH-family URLs only pass
// through unchanged when userinfo is a single token (conventional `git@`);
// when userinfo carries a `:password`, it's redacted regardless of scheme.
//
// Returns the input unchanged when parsing fails. As a belt-and-braces
// safety net, falls back to [String]'s regex when url.Parse decided there
// was no userinfo but the raw text still contains a HTTP(S)-shaped
// userinfo run (e.g., malformed `https:///user@host`).
func URL(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		// Last-ditch: the raw string may still textually contain a userinfo
		// segment we'd want to scrub. Apply the regex.
		return String(raw)
	}
	if u.User == nil {
		// Same belt-and-braces — url.Parse occasionally yields no User on
		// malformed inputs that nonetheless textually carry "user@host".
		return String(raw)
	}
	if isSSHScheme(u.Scheme) {
		if _, hasPassword := u.User.Password(); !hasPassword {
			return raw
		}
	}
	// url.User(URLUserSentinel) avoids the percent-encoding that
	// url.UserPassword would apply to reserved characters in the sentinel.
	u.User = url.User(URLUserSentinel)
	return u.String()
}

func isSSHScheme(scheme string) bool {
	s := strings.ToLower(scheme)
	return s == "ssh" || s == "git+ssh"
}

// Sensitive reports whether an attribute key is sensitive enough to warrant
// fully redacting its value (vs the URL-aware string redaction). Matches
// substrings (lowercased) so variations like `apiToken`, `auth_token`, or
// `Password` all hit. Over-redaction (e.g., `tokenize`) is acceptable per
// the package's "be conservative" policy — losing telemetry granularity
// is preferable to leaking a credential.
func Sensitive(key string) bool {
	lower := strings.ToLower(key)
	for _, marker := range []string{
		"token",
		"password",
		"secret",
		"apikey",
		"api_key",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	// "Authorization" is the standard HTTP header name; redact verbatim.
	return lower == "authorization"
}
