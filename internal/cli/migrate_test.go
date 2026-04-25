package cli

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"github.com/Ogguz/gitraft/internal/provider"
)

// mockProvider implements provider.Provider for CLI tests.
type mockProvider struct {
	name        string
	hosts       []string
	exists      bool
	existsErr   error
	createErr   error
	createCalls []provider.CreateOptions
	authURLFn   func(*url.URL) (string, error)
}

func (m *mockProvider) Name() string { return m.name }

func (m *mockProvider) Matches(u *url.URL) bool {
	for _, h := range m.hosts {
		if u.Hostname() == h {
			return true
		}
	}
	return false
}

func (m *mockProvider) ParseRepo(u *url.URL) (string, string, error) {
	return provider.SplitPath(u)
}

func (m *mockProvider) RepoExists(_ context.Context, _, _ string) (bool, error) {
	return m.exists, m.existsErr
}

func (m *mockProvider) CreateRepo(_ context.Context, opts provider.CreateOptions) error {
	m.createCalls = append(m.createCalls, opts)
	return m.createErr
}

func (m *mockProvider) AuthURL(u *url.URL) (string, error) {
	if m.authURLFn != nil {
		return m.authURLFn(u)
	}
	return u.String(), nil
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := provider.Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q): %v", raw, err)
	}
	return u
}

func testLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return logger, &buf
}

func TestPickProvider_OverrideMatches(t *testing.T) {
	gh := &mockProvider{name: "github", hosts: []string{"github.com"}}
	u := mustParse(t, "https://github.com/a/b.git")
	p, err := pickProvider([]provider.Provider{gh}, u, "github")
	if err != nil {
		t.Fatal(err)
	}
	if p != gh {
		t.Errorf("expected github; got %v", p)
	}
}

func TestPickProvider_OverrideUnknownErrors(t *testing.T) {
	gh := &mockProvider{name: "github", hosts: []string{"github.com"}}
	u := mustParse(t, "https://github.com/a/b.git")
	_, err := pickProvider([]provider.Provider{gh}, u, "gihub")
	if err == nil {
		t.Fatal("expected error for typo'd provider name")
	}
	if !strings.Contains(err.Error(), "gihub") {
		t.Errorf("error should name the bad input: %v", err)
	}
	if !strings.Contains(err.Error(), "github") {
		t.Errorf("error should list available providers: %v", err)
	}
}

func TestPickProvider_AutoDetect(t *testing.T) {
	gh := &mockProvider{name: "github", hosts: []string{"github.com"}}
	gl := &mockProvider{name: "gitlab", hosts: []string{"gitlab.com"}}
	u := mustParse(t, "git@gitlab.com:a/b.git")
	p, err := pickProvider([]provider.Provider{gh, gl}, u, "")
	if err != nil {
		t.Fatal(err)
	}
	if p != gl {
		t.Errorf("expected gitlab; got %v", p)
	}
}

func TestPickProvider_NoMatchNoError(t *testing.T) {
	gh := &mockProvider{name: "github", hosts: []string{"github.com"}}
	u := mustParse(t, "https://bitbucket.org/a/b.git")
	p, err := pickProvider([]provider.Provider{gh}, u, "")
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		t.Errorf("expected no match; got %q", p.Name())
	}
}

func TestEnsureDestination_SkipsWhenExists(t *testing.T) {
	gh := &mockProvider{name: "github", hosts: []string{"github.com"}, exists: true}
	logger, _ := testLogger()
	u := mustParse(t, "https://github.com/a/b.git")
	if err := ensureDestination(context.Background(), gh, u, "", provider.VisibilityPrivate, logger); err != nil {
		t.Fatal(err)
	}
	if len(gh.createCalls) != 0 {
		t.Errorf("expected no CreateRepo calls; got %d", len(gh.createCalls))
	}
}

func TestEnsureDestination_CreatesWhenMissing(t *testing.T) {
	gh := &mockProvider{name: "github", hosts: []string{"github.com"}, exists: false}
	logger, _ := testLogger()
	u := mustParse(t, "https://github.com/a/b.git")
	if err := ensureDestination(context.Background(), gh, u, "hi", provider.VisibilityPrivate, logger); err != nil {
		t.Fatal(err)
	}
	if len(gh.createCalls) != 1 {
		t.Fatalf("expected 1 CreateRepo call; got %d", len(gh.createCalls))
	}
	call := gh.createCalls[0]
	if call.Owner != "a" || call.Name != "b" {
		t.Errorf("owner/name mismatch: %+v", call)
	}
	if call.Description != "hi" {
		t.Errorf("description mismatch: %q", call.Description)
	}
	if call.Visibility != provider.VisibilityPrivate {
		t.Errorf("visibility mismatch: %v", call.Visibility)
	}
}

func TestEnsureDestination_AlreadyExistsRace(t *testing.T) {
	gh := &mockProvider{
		name: "github", hosts: []string{"github.com"},
		exists:    false,
		createErr: provider.ErrRepoAlreadyExists,
	}
	logger, buf := testLogger()
	u := mustParse(t, "https://github.com/a/b.git")
	if err := ensureDestination(context.Background(), gh, u, "", provider.VisibilityPrivate, logger); err != nil {
		t.Fatalf("expected nil error on race; got %v", err)
	}
	if !strings.Contains(buf.String(), "appeared during migration") {
		t.Errorf("expected warning log; got %s", buf.String())
	}
}

func TestEnsureDestination_CreateError(t *testing.T) {
	gh := &mockProvider{
		name: "github", hosts: []string{"github.com"},
		exists:    false,
		createErr: errors.New("boom"),
	}
	logger, _ := testLogger()
	u := mustParse(t, "https://github.com/a/b.git")
	err := ensureDestination(context.Background(), gh, u, "", provider.VisibilityPrivate, logger)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("expected wrapped error; got %v", err)
	}
}

func TestAuthURL_NilProviderWarnsAndPassesThrough(t *testing.T) {
	logger, buf := testLogger()
	u := mustParse(t, "https://bitbucket.org/a/b.git")
	got, err := authURL(nil, u, "https://bitbucket.org/a/b.git", logger)
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://bitbucket.org/a/b.git" {
		t.Errorf("expected raw pass-through; got %q", got)
	}
	if !strings.Contains(buf.String(), "no provider matched") {
		t.Errorf("expected warning log; got %s", buf.String())
	}
}

func TestAuthURL_WithProviderUsesProvider(t *testing.T) {
	gh := &mockProvider{
		name:  "github",
		hosts: []string{"github.com"},
		authURLFn: func(u *url.URL) (string, error) {
			return "https://token@github.com/a/b.git", nil
		},
	}
	logger, _ := testLogger()
	u := mustParse(t, "https://github.com/a/b.git")
	got, err := authURL(gh, u, u.String(), logger)
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://token@github.com/a/b.git" {
		t.Errorf("expected provider rewrite; got %q", got)
	}
}

func TestBuildProviders_CreatesAllProviders(t *testing.T) {
	ps, err := buildProviders("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"github", "gitlab", "bitbucket", "bitbucket-server", "gitea"} {
		if provider.ByName(ps, name) == nil {
			t.Errorf("expected %q provider in registry", name)
		}
	}
}

func TestBuildProviders_GitLabSelfHostedMatches(t *testing.T) {
	ps, err := buildProviders("https://gitlab.example.com", "", "")
	if err != nil {
		t.Fatal(err)
	}
	gl := provider.ByName(ps, "gitlab")
	if gl == nil {
		t.Fatal("gitlab provider missing")
	}
	u := mustParse(t, "https://gitlab.example.com/a/b.git")
	if !gl.Matches(u) {
		t.Error("self-hosted gitlab should match configured host")
	}
	uDefault := mustParse(t, "https://gitlab.com/a/b.git")
	if gl.Matches(uDefault) {
		t.Error("self-hosted gitlab should not match gitlab.com")
	}
}

func TestBuildProviders_GitLabSelfHostedWithPortMatches(t *testing.T) {
	// Regression: previously gitlabHost returned Host (with port), but Matches
	// compared Hostname (without port) — so ":8080" URLs silently never matched.
	ps, err := buildProviders("https://gitlab.example.com:8080", "", "")
	if err != nil {
		t.Fatal(err)
	}
	gl := provider.ByName(ps, "gitlab")
	u := mustParse(t, "https://gitlab.example.com:8080/a/b.git")
	if !gl.Matches(u) {
		t.Error("self-hosted gitlab on :8080 should match")
	}
	uNoPort := mustParse(t, "https://gitlab.example.com/a/b.git")
	if !gl.Matches(uNoPort) {
		t.Error("self-hosted gitlab should match same host without port")
	}
}

func TestBuildProviders_InvalidGitLabURL(t *testing.T) {
	_, err := buildProviders("://broken", "", "")
	if err == nil {
		t.Fatal("expected error for malformed URL")
	}
	if !strings.Contains(err.Error(), "invalid --gitlab-url") {
		t.Errorf("expected 'invalid --gitlab-url' in error; got %v", err)
	}
}

func TestBuildProviders_InvalidBitbucketURL(t *testing.T) {
	_, err := buildProviders("", "://broken", "")
	if err == nil {
		t.Fatal("expected error for malformed URL")
	}
	if !strings.Contains(err.Error(), "invalid --bitbucket-url") {
		t.Errorf("expected 'invalid --bitbucket-url' in error; got %v", err)
	}
}

func TestBuildProviders_BitbucketServerUnconfiguredMatchesNothing(t *testing.T) {
	ps, err := buildProviders("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	bbs := provider.ByName(ps, "bitbucket-server")
	if bbs == nil {
		t.Fatal("bitbucket-server provider missing")
	}
	u := mustParse(t, "https://bitbucket.example.com/scm/proj/repo.git")
	if bbs.Matches(u) {
		t.Error("unconfigured bitbucket-server should not match any URL")
	}
}

func TestBuildProviders_BothInvalidURLsAreJoinedErrors(t *testing.T) {
	// Both flags malformed → caller sees both errors at once, not just the first.
	_, err := buildProviders("://gl-broken", "://bb-broken", "://gt-broken")
	if err == nil {
		t.Fatal("expected joined error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "--gitlab-url") {
		t.Errorf("expected gitlab error in joined output; got %v", msg)
	}
	if !strings.Contains(msg, "--bitbucket-url") {
		t.Errorf("expected bitbucket error in joined output; got %v", msg)
	}
	if !strings.Contains(msg, "--gitea-url") {
		t.Errorf("expected gitea error in joined output; got %v", msg)
	}
}

func TestBuildProviders_InvalidGiteaURL(t *testing.T) {
	_, err := buildProviders("", "", "://broken")
	if err == nil {
		t.Fatal("expected error for malformed URL")
	}
	if !strings.Contains(err.Error(), "invalid --gitea-url") {
		t.Errorf("expected 'invalid --gitea-url' in error; got %v", err)
	}
}

func TestBuildProviders_GiteaConfiguredMatches(t *testing.T) {
	ps, err := buildProviders("", "", "https://gitea.example.com")
	if err != nil {
		t.Fatal(err)
	}
	gt := provider.ByName(ps, "gitea")
	if gt == nil {
		t.Fatal("gitea provider missing")
	}
	if !gt.Matches(mustParse(t, "https://gitea.example.com/a/b.git")) {
		t.Error("configured gitea should match its host")
	}
	if gt.Matches(mustParse(t, "https://gitea.com/a/b.git")) {
		t.Error("configured gitea should not match a different host")
	}
}

func TestBuildProviders_GiteaUnconfiguredMatchesNothing(t *testing.T) {
	// Gitea is dominantly self-hosted — without --gitea-url, the provider
	// must match nothing (mirrors the Bitbucket Server pattern).
	ps, err := buildProviders("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	gt := provider.ByName(ps, "gitea")
	for _, raw := range []string{
		"https://gitea.com/a/b.git",
		"https://codeberg.org/a/b.git",
		"https://gitea.example.com/a/b.git",
	} {
		if gt.Matches(mustParse(t, raw)) {
			t.Errorf("unconfigured gitea must not match %s", raw)
		}
	}
}

func TestBuildProviders_AllSelfHostedFlagsCombined(t *testing.T) {
	ps, err := buildProviders("https://gl.example.com", "https://bb.example.com", "https://gt.example.com")
	if err != nil {
		t.Fatal(err)
	}
	gl := provider.ByName(ps, "gitlab")
	bbs := provider.ByName(ps, "bitbucket-server")
	gt := provider.ByName(ps, "gitea")
	if gl == nil || bbs == nil || gt == nil {
		t.Fatal("missing providers")
	}
	if !gl.Matches(mustParse(t, "https://gl.example.com/a/b.git")) {
		t.Error("gitlab should match its configured host")
	}
	if !bbs.Matches(mustParse(t, "https://bb.example.com/scm/proj/repo.git")) {
		t.Error("bitbucket-server should match its configured host")
	}
	if !gt.Matches(mustParse(t, "https://gt.example.com/owner/repo.git")) {
		t.Error("gitea should match its configured host")
	}
}

func TestBuildProviders_BitbucketServerConfiguredMatches(t *testing.T) {
	ps, err := buildProviders("", "https://bitbucket.example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	bbs := provider.ByName(ps, "bitbucket-server")
	u := mustParse(t, "https://bitbucket.example.com/scm/proj/repo.git")
	if !bbs.Matches(u) {
		t.Error("configured bitbucket-server should match its host")
	}
	uOther := mustParse(t, "https://bitbucket.org/team/repo.git")
	if bbs.Matches(uOther) {
		t.Error("bitbucket-server must not match bitbucket.org")
	}
}

func TestParseHostFromURL(t *testing.T) {
	tests := []struct {
		in       string
		flagName string
		want     string
		wantErr  bool
	}{
		{"", "--flag", "", false},
		{"https://host.example.com", "--flag", "host.example.com", false},
		{"https://host.example.com:8080/path", "--flag", "host.example.com", false},
		{"host.example.com", "--flag", "host.example.com", false},
		{"host.example.com:8080", "--flag", "host.example.com", false},
		{"://broken", "--flag", "", true},
		{"https://", "--flag", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseHostFromURL(tc.in, tc.flagName)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.in)
				}
				if !strings.Contains(err.Error(), tc.flagName) {
					t.Errorf("error should include flag name; got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("parseHostFromURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestAuthURL_BitbucketServerHintWhenUnconfigured(t *testing.T) {
	logger, buf := testLogger()
	u := mustParse(t, "https://bb.internal.corp/scm/PROJ/repo.git")
	got, err := authURL(nil, u, "https://bb.internal.corp/scm/PROJ/repo.git", logger)
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://bb.internal.corp/scm/PROJ/repo.git" {
		t.Errorf("expected pass-through; got %q", got)
	}
	if !strings.Contains(buf.String(), "--bitbucket-url") {
		t.Errorf("expected --bitbucket-url hint for BB-Server-shaped URL; got %s", buf.String())
	}
}

func TestAuthURL_NoHintForUnrelatedURL(t *testing.T) {
	logger, buf := testLogger()
	u := mustParse(t, "https://random.example.com/owner/repo.git")
	_, _ = authURL(nil, u, u.String(), logger)
	if strings.Contains(buf.String(), "--bitbucket-url") {
		t.Errorf("BB-Server hint must not fire for unrelated URLs; got %s", buf.String())
	}
}

func TestGitLabHost(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"https://gitlab.example.com", "gitlab.example.com", false},
		{"https://gitlab.example.com:8080/path", "gitlab.example.com", false}, // port stripped
		{"gitlab.example.com", "gitlab.example.com", false},
		{"gitlab.example.com:8080", "gitlab.example.com", false}, // port stripped
		{"://broken", "", true},
		{"https://", "", true}, // no host
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := gitlabHost(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("gitlabHost(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestWarnMissingTokens_BothUnset(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GITLAB_TOKEN", "")
	logger, buf := testLogger()
	warnMissingTokens(logger,
		&mockProvider{name: "github"},
		&mockProvider{name: "gitlab"},
	)
	if !strings.Contains(buf.String(), "GITHUB_TOKEN unset") {
		t.Errorf("expected GITHUB_TOKEN warning; got %s", buf.String())
	}
	if !strings.Contains(buf.String(), "GITLAB_TOKEN unset") {
		t.Errorf("expected GITLAB_TOKEN warning; got %s", buf.String())
	}
}

func TestWarnMissingTokens_OnlyWarnsForUsedProviders(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GITLAB_TOKEN", "")
	logger, buf := testLogger()
	// Only gitlab is used — GITHUB_TOKEN warning must NOT fire.
	warnMissingTokens(logger, &mockProvider{name: "gitlab"})
	if strings.Contains(buf.String(), "GITHUB_TOKEN") {
		t.Errorf("GITHUB_TOKEN warning must not fire when github isn't used; got %s", buf.String())
	}
	if !strings.Contains(buf.String(), "GITLAB_TOKEN") {
		t.Errorf("GITLAB_TOKEN warning expected; got %s", buf.String())
	}
}

func TestWarnMissingTokens_Dedup(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	logger, buf := testLogger()
	// Same provider passed twice — warn only once.
	warnMissingTokens(logger,
		&mockProvider{name: "github"},
		&mockProvider{name: "github"},
	)
	count := strings.Count(buf.String(), "GITHUB_TOKEN unset")
	if count != 1 {
		t.Errorf("expected exactly 1 GITHUB_TOKEN warning; got %d", count)
	}
}

func TestWarnMissingTokens_TokenSetNoWarn(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	t.Setenv("GITLAB_TOKEN", "y")
	logger, buf := testLogger()
	warnMissingTokens(logger,
		&mockProvider{name: "github"},
		&mockProvider{name: "gitlab"},
	)
	if strings.Contains(buf.String(), "unset") {
		t.Errorf("no warnings expected; got %s", buf.String())
	}
}

func TestWarnMissingTokens_NilProviderIgnored(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	logger, buf := testLogger()
	warnMissingTokens(logger, nil, &mockProvider{name: "github"})
	if !strings.Contains(buf.String(), "GITHUB_TOKEN") {
		t.Errorf("github warning expected despite nil entry; got %s", buf.String())
	}
}

func TestWarnMissingTokens_GiteaTokenUnsetWarns(t *testing.T) {
	cases := []struct {
		name  string
		token string
		warn  bool
	}{
		{"unset", "", true},
		{"set", "tok", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GITEA_TOKEN", tc.token)
			logger, buf := testLogger()
			warnMissingTokens(logger, &mockProvider{name: "gitea"})
			warned := strings.Contains(buf.String(), "GITEA_TOKEN unset")
			if warned != tc.warn {
				t.Errorf("warned = %v; want %v (buf=%s)", warned, tc.warn, buf.String())
			}
		})
	}
}

func TestEnsureDestination_WarnsOnInternalCollapse(t *testing.T) {
	cases := []struct {
		providerName string
		wantWarn     bool
	}{
		{"github", true},           // GitHub has no native internal — warn
		{"gitlab", false},          // GitLab supports internal — no warn
		{"bitbucket", true},        // Cloud — warn
		{"bitbucket-server", true}, // Server — warn
		{"gitea", true},            // Gitea — warn
	}
	for _, tc := range cases {
		t.Run(tc.providerName, func(t *testing.T) {
			mp := &mockProvider{name: tc.providerName, hosts: []string{"example.com"}, exists: false}
			logger, buf := testLogger()
			u := mustParse(t, "https://example.com/owner/repo.git")
			if err := ensureDestination(context.Background(), mp, u, "", provider.VisibilityInternal, logger); err != nil {
				t.Fatal(err)
			}
			warned := strings.Contains(buf.String(), "does not support 'internal' visibility")
			if warned != tc.wantWarn {
				t.Errorf("warned = %v; want %v (buf=%s)", warned, tc.wantWarn, buf.String())
			}
		})
	}
}

func TestWarnMissingTokens_BitbucketServerEitherEnvUnsetWarns(t *testing.T) {
	cases := []struct {
		name     string
		username string
		token    string
		warn     bool
	}{
		{"both unset", "", "", true},
		{"username unset", "", "secret", true},
		{"token unset", "alice", "", true},
		{"both set", "alice", "secret", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BITBUCKET_SERVER_USERNAME", tc.username)
			t.Setenv("BITBUCKET_SERVER_TOKEN", tc.token)
			logger, buf := testLogger()
			warnMissingTokens(logger, &mockProvider{name: "bitbucket-server"})
			warned := strings.Contains(buf.String(), "BITBUCKET_SERVER_USERNAME or BITBUCKET_SERVER_TOKEN unset")
			if warned != tc.warn {
				t.Errorf("warned = %v; want %v (buf=%s)", warned, tc.warn, buf.String())
			}
		})
	}
}

func TestWarnMissingTokens_BitbucketEitherEnvUnsetWarns(t *testing.T) {
	cases := []struct {
		name     string
		username string
		password string
		warn     bool
	}{
		{"both unset", "", "", true},
		{"username unset", "", "secret", true},
		{"password unset", "alice", "", true},
		{"both set", "alice", "secret", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BITBUCKET_USERNAME", tc.username)
			t.Setenv("BITBUCKET_APP_PASSWORD", tc.password)
			logger, buf := testLogger()
			warnMissingTokens(logger, &mockProvider{name: "bitbucket"})
			warned := strings.Contains(buf.String(), "BITBUCKET_USERNAME or BITBUCKET_APP_PASSWORD unset")
			if warned != tc.warn {
				t.Errorf("warned = %v; want %v (buf=%s)", warned, tc.warn, buf.String())
			}
		})
	}
}
