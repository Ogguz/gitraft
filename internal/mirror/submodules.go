package mirror

import (
	"bufio"
	"context"
	"net/url"
	"os/exec"
	"strings"
)

// submodule describes one entry parsed from a .gitmodules file. Unexported —
// no external consumer; the type's only role is to carry path/URL pairs to
// the warning-logging loop in Run.
type submodule struct {
	Path string
	URL  string
}

// listSubmodules reads .gitmodules from HEAD of the given mirror clone and
// returns every submodule entry. The boolean indicates whether `.gitmodules`
// was found at all. Returns an error only when ref enumeration fails for a
// non-trivial reason (corrupt repo, ctx cancellation) — the common case
// "no .gitmodules on HEAD" yields (nil, false, nil).
func listSubmodules(ctx context.Context, repoDir string) ([]submodule, bool, error) {
	if !looksLikeMirrorClone(repoDir) {
		return nil, false, nil
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "show", "HEAD:.gitmodules")
	out, err := cmd.Output()
	if err != nil {
		// Distinguish ctx cancellation (real failure) from path-not-found
		// (the common case — most repos don't have .gitmodules).
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, false, nil
	}
	return parseGitmodules(string(out)), true, nil
}

// parseGitmodules parses the INI-like .gitmodules content. Tolerates comments
// (# and ;), blank lines, partial entries (without all fields), and unquoted
// values. Submodule entries with no `path` value are skipped — the path is
// the minimum identification needed to surface a useful warning.
//
// Duplicate keys within a single [submodule] section follow last-write-wins.
// Section names with embedded spaces (e.g., `[submodule "lib with space"]`)
// parse correctly because the [ prefix is the only thing that matters.
func parseGitmodules(content string) []submodule {
	var (
		result []submodule
		cur    submodule
		active bool
	)
	flush := func() {
		if active && cur.Path != "" {
			result = append(result, cur)
		}
		cur = submodule{}
		active = false
	}

	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[submodule") {
			flush()
			active = true
			continue
		}
		if !active {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			k = strings.TrimSpace(k)
			v = strings.Trim(strings.TrimSpace(v), `"`)
			switch k {
			case "path":
				cur.Path = v
			case "url":
				cur.URL = v
			}
		}
	}
	flush()
	return result
}

// redactURL strips userinfo (user:pass) from a URL for safe logging. If the
// URL doesn't parse, it's returned unchanged — caller may still want to log
// the raw value so the user can investigate.
func redactURL(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	// Use a single-segment user (no colon) so the URL encoder doesn't have
	// to percent-encode any reserved chars in the redaction sentinel.
	u.User = url.User("redacted")
	return u.String()
}
