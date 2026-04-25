package cli

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ogguz/gitraft/internal/config"
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

// ---- helpers for buildProviders test inputs ----

func settingsWithGitLabURL(u string) providerSettings {
	return providerSettings{GitLab: gitlabSettings{URL: u}}
}

func settingsWithBitbucketURL(u string) providerSettings {
	return providerSettings{BitbucketServer: bitbucketServerSettings{URL: u}}
}

func settingsWithGiteaURL(u string) providerSettings {
	return providerSettings{Gitea: giteaSettings{URL: u}}
}

// ---- pickProvider ----

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

// ---- ensureDestination ----

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

func TestEnsureDestination_WarnsOnInternalCollapse(t *testing.T) {
	cases := []struct {
		providerName string
		wantWarn     bool
	}{
		{"github", true},
		{"gitlab", false},
		{"bitbucket", true},
		{"bitbucket-server", true},
		{"gitea", true},
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

// ---- authURL ----

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

// ---- buildProviders ----

func TestBuildProviders_CreatesAllProviders(t *testing.T) {
	ps, err := buildProviders(providerSettings{})
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
	ps, err := buildProviders(settingsWithGitLabURL("https://gitlab.example.com"))
	if err != nil {
		t.Fatal(err)
	}
	gl := provider.ByName(ps, "gitlab")
	if !gl.Matches(mustParse(t, "https://gitlab.example.com/a/b.git")) {
		t.Error("self-hosted gitlab should match configured host")
	}
	if gl.Matches(mustParse(t, "https://gitlab.com/a/b.git")) {
		t.Error("self-hosted gitlab should not match gitlab.com")
	}
}

func TestBuildProviders_GitLabSelfHostedWithPortMatches(t *testing.T) {
	ps, err := buildProviders(settingsWithGitLabURL("https://gitlab.example.com:8080"))
	if err != nil {
		t.Fatal(err)
	}
	gl := provider.ByName(ps, "gitlab")
	if !gl.Matches(mustParse(t, "https://gitlab.example.com:8080/a/b.git")) {
		t.Error("self-hosted gitlab on :8080 should match")
	}
	if !gl.Matches(mustParse(t, "https://gitlab.example.com/a/b.git")) {
		t.Error("self-hosted gitlab should match same host without port")
	}
}

func TestBuildProviders_InvalidGitLabURL(t *testing.T) {
	_, err := buildProviders(settingsWithGitLabURL("://broken"))
	if err == nil {
		t.Fatal("expected error for malformed URL")
	}
	if !strings.Contains(err.Error(), "invalid --gitlab-url") {
		t.Errorf("expected 'invalid --gitlab-url' in error; got %v", err)
	}
}

func TestBuildProviders_InvalidBitbucketURL(t *testing.T) {
	_, err := buildProviders(settingsWithBitbucketURL("://broken"))
	if err == nil {
		t.Fatal("expected error for malformed URL")
	}
	if !strings.Contains(err.Error(), "invalid --bitbucket-url") {
		t.Errorf("expected 'invalid --bitbucket-url' in error; got %v", err)
	}
}

func TestBuildProviders_InvalidGiteaURL(t *testing.T) {
	_, err := buildProviders(settingsWithGiteaURL("://broken"))
	if err == nil {
		t.Fatal("expected error for malformed URL")
	}
	if !strings.Contains(err.Error(), "invalid --gitea-url") {
		t.Errorf("expected 'invalid --gitea-url' in error; got %v", err)
	}
}

func TestBuildProviders_AllInvalidURLsAreJoinedErrors(t *testing.T) {
	_, err := buildProviders(providerSettings{
		GitLab:          gitlabSettings{URL: "://gl-broken"},
		BitbucketServer: bitbucketServerSettings{URL: "://bb-broken"},
		Gitea:           giteaSettings{URL: "://gt-broken"},
	})
	if err == nil {
		t.Fatal("expected joined error")
	}
	for _, want := range []string{"--gitlab-url", "--bitbucket-url", "--gitea-url"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected %q in joined output; got %v", want, err.Error())
		}
	}
}

func TestBuildProviders_BitbucketServerUnconfiguredMatchesNothing(t *testing.T) {
	ps, _ := buildProviders(providerSettings{})
	bbs := provider.ByName(ps, "bitbucket-server")
	if bbs.Matches(mustParse(t, "https://bitbucket.example.com/scm/proj/repo.git")) {
		t.Error("unconfigured bitbucket-server should not match any URL")
	}
}

func TestBuildProviders_BitbucketServerConfiguredMatches(t *testing.T) {
	ps, _ := buildProviders(settingsWithBitbucketURL("https://bitbucket.example.com"))
	bbs := provider.ByName(ps, "bitbucket-server")
	if !bbs.Matches(mustParse(t, "https://bitbucket.example.com/scm/proj/repo.git")) {
		t.Error("configured bitbucket-server should match its host")
	}
	if bbs.Matches(mustParse(t, "https://bitbucket.org/team/repo.git")) {
		t.Error("bitbucket-server must not match bitbucket.org")
	}
}

func TestBuildProviders_GiteaConfiguredMatches(t *testing.T) {
	ps, _ := buildProviders(settingsWithGiteaURL("https://gitea.example.com"))
	gt := provider.ByName(ps, "gitea")
	if !gt.Matches(mustParse(t, "https://gitea.example.com/a/b.git")) {
		t.Error("configured gitea should match its host")
	}
	if gt.Matches(mustParse(t, "https://gitea.com/a/b.git")) {
		t.Error("configured gitea should not match a different host")
	}
}

func TestBuildProviders_GiteaUnconfiguredMatchesNothing(t *testing.T) {
	ps, _ := buildProviders(providerSettings{})
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
	ps, err := buildProviders(providerSettings{
		GitLab:          gitlabSettings{URL: "https://gl.example.com"},
		BitbucketServer: bitbucketServerSettings{URL: "https://bb.example.com"},
		Gitea:           giteaSettings{URL: "https://gt.example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !provider.ByName(ps, "gitlab").Matches(mustParse(t, "https://gl.example.com/a/b.git")) {
		t.Error("gitlab should match its configured host")
	}
	if !provider.ByName(ps, "bitbucket-server").Matches(mustParse(t, "https://bb.example.com/scm/proj/repo.git")) {
		t.Error("bitbucket-server should match its configured host")
	}
	if !provider.ByName(ps, "gitea").Matches(mustParse(t, "https://gt.example.com/owner/repo.git")) {
		t.Error("gitea should match its configured host")
	}
}

// ---- parseHostFromURL / gitlabHost ----

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

func TestGitLabHost(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"https://gitlab.example.com", "gitlab.example.com", false},
		{"https://gitlab.example.com:8080/path", "gitlab.example.com", false},
		{"gitlab.example.com", "gitlab.example.com", false},
		{"gitlab.example.com:8080", "gitlab.example.com", false},
		{"://broken", "", true},
		{"https://", "", true},
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

// ---- warnMissingTokens ----

func TestWarnMissingTokens_BothUnset(t *testing.T) {
	logger, buf := testLogger()
	warnMissingTokens(logger, providerSettings{},
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
	logger, buf := testLogger()
	warnMissingTokens(logger, providerSettings{}, &mockProvider{name: "gitlab"})
	if strings.Contains(buf.String(), "GITHUB_TOKEN") {
		t.Errorf("GITHUB_TOKEN warning must not fire when github isn't used; got %s", buf.String())
	}
	if !strings.Contains(buf.String(), "GITLAB_TOKEN") {
		t.Errorf("GITLAB_TOKEN warning expected; got %s", buf.String())
	}
}

func TestWarnMissingTokens_Dedup(t *testing.T) {
	logger, buf := testLogger()
	warnMissingTokens(logger, providerSettings{},
		&mockProvider{name: "github"},
		&mockProvider{name: "github"},
	)
	count := strings.Count(buf.String(), "GITHUB_TOKEN unset")
	if count != 1 {
		t.Errorf("expected exactly 1 GITHUB_TOKEN warning; got %d", count)
	}
}

func TestWarnMissingTokens_TokenSetNoWarn(t *testing.T) {
	s := providerSettings{
		GitHub: githubSettings{Token: "x"},
		GitLab: gitlabSettings{Token: "y"},
	}
	logger, buf := testLogger()
	warnMissingTokens(logger, s,
		&mockProvider{name: "github"},
		&mockProvider{name: "gitlab"},
	)
	if strings.Contains(buf.String(), "unset") {
		t.Errorf("no warnings expected; got %s", buf.String())
	}
}

func TestWarnMissingTokens_NilProviderIgnored(t *testing.T) {
	logger, buf := testLogger()
	warnMissingTokens(logger, providerSettings{}, nil, &mockProvider{name: "github"})
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
			logger, buf := testLogger()
			s := providerSettings{Gitea: giteaSettings{Token: tc.token}}
			warnMissingTokens(logger, s, &mockProvider{name: "gitea"})
			warned := strings.Contains(buf.String(), "GITEA_TOKEN unset")
			if warned != tc.warn {
				t.Errorf("warned = %v; want %v (buf=%s)", warned, tc.warn, buf.String())
			}
		})
	}
}

func TestWarnMissingTokens_BitbucketServerEitherUnsetWarns(t *testing.T) {
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
			s := providerSettings{BitbucketServer: bitbucketServerSettings{
				Username: tc.username,
				Token:    tc.token,
			}}
			logger, buf := testLogger()
			warnMissingTokens(logger, s, &mockProvider{name: "bitbucket-server"})
			warned := strings.Contains(buf.String(), "BITBUCKET_SERVER_USERNAME or BITBUCKET_SERVER_TOKEN unset")
			if warned != tc.warn {
				t.Errorf("warned = %v; want %v (buf=%s)", warned, tc.warn, buf.String())
			}
		})
	}
}

func TestWarnMissingTokens_BitbucketEitherUnsetWarns(t *testing.T) {
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
			s := providerSettings{Bitbucket: bitbucketSettings{
				Username:    tc.username,
				AppPassword: tc.password,
			}}
			logger, buf := testLogger()
			warnMissingTokens(logger, s, &mockProvider{name: "bitbucket"})
			warned := strings.Contains(buf.String(), "BITBUCKET_USERNAME or BITBUCKET_APP_PASSWORD unset")
			if warned != tc.warn {
				t.Errorf("warned = %v; want %v (buf=%s)", warned, tc.warn, buf.String())
			}
		})
	}
}

// TestProviderAuthSatisfied_AllProviderWarningsHaveBranches asserts that
// every entry in providerWarnings has a matching providerAuthSatisfied case
// and that an empty settings struct yields false (so the warning fires).
// Catches regressions where a future provider is added to the warnings map
// but the auth-satisfied switch isn't updated (or vice versa).
func TestProviderAuthSatisfied_AllProviderWarningsHaveBranches(t *testing.T) {
	for name := range providerWarnings {
		t.Run(name, func(t *testing.T) {
			if providerAuthSatisfied(name, providerSettings{}) {
				t.Errorf("providerAuthSatisfied(%q, empty) = true; expected false (no auth set)", name)
			}
		})
	}
}

// ---- resolveSettings ----

func TestResolveSettings_EnvWinsOverConfig(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "from-env")
	cfg := &config.Config{Providers: config.Providers{GitHub: config.GitHubProvider{Token: "from-config"}}}
	s := resolveSettings(migrateFlags{}, cfg)
	if s.GitHub.Token != "from-env" {
		t.Errorf("env should win; got %q", s.GitHub.Token)
	}
}

func TestResolveSettings_FlagWinsOverConfig(t *testing.T) {
	cfg := &config.Config{Providers: config.Providers{GitLab: config.GitLabProvider{URL: "https://from-config"}}}
	s := resolveSettings(migrateFlags{GitLabURL: "https://from-flag"}, cfg)
	if s.GitLab.URL != "https://from-flag" {
		t.Errorf("flag should win; got %q", s.GitLab.URL)
	}
}

func TestResolveSettings_FlagWinsOverEnvAndConfig(t *testing.T) {
	// URLs don't currently have env-var sources, but lock in the contract that
	// a future env-var would still lose to a flag.
	t.Setenv("GITLAB_URL", "https://from-env") // ignored today, but defensive
	cfg := &config.Config{Providers: config.Providers{GitLab: config.GitLabProvider{URL: "https://from-config"}}}
	s := resolveSettings(migrateFlags{GitLabURL: "https://from-flag"}, cfg)
	if s.GitLab.URL != "https://from-flag" {
		t.Errorf("flag should win over env+config; got %q", s.GitLab.URL)
	}
}

func TestResolveSettings_ConfigUsedWhenOthersUnset(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	cfg := &config.Config{Providers: config.Providers{GitHub: config.GitHubProvider{Token: "from-config"}}}
	s := resolveSettings(migrateFlags{}, cfg)
	if s.GitHub.Token != "from-config" {
		t.Errorf("config should win when env empty; got %q", s.GitHub.Token)
	}
}

func TestResolveSettings_AllEmpty(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("BITBUCKET_USERNAME", "")
	t.Setenv("BITBUCKET_APP_PASSWORD", "")
	t.Setenv("BITBUCKET_SERVER_USERNAME", "")
	t.Setenv("BITBUCKET_SERVER_TOKEN", "")
	t.Setenv("GITEA_TOKEN", "")
	s := resolveSettings(migrateFlags{}, &config.Config{})
	if s != (providerSettings{}) {
		t.Errorf("expected zero-value settings; got %+v", s)
	}
}

func TestResolveSettings_NilConfigSafe(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	s := resolveSettings(migrateFlags{}, nil)
	if s.GitHub.Token != "x" {
		t.Errorf("expected env-only resolution; got %q", s.GitHub.Token)
	}
}

func TestResolveSettings_MixedSources(t *testing.T) {
	// One field per source: GitHub from config, GitLab URL from flag,
	// Bitbucket username from env. All three must land correctly.
	t.Setenv("BITBUCKET_USERNAME", "alice-from-env")
	cfg := &config.Config{Providers: config.Providers{
		GitHub: config.GitHubProvider{Token: "ghp-from-config"},
	}}
	s := resolveSettings(migrateFlags{GitLabURL: "https://gl-from-flag"}, cfg)
	if s.GitHub.Token != "ghp-from-config" {
		t.Errorf("github from config; got %q", s.GitHub.Token)
	}
	if s.GitLab.URL != "https://gl-from-flag" {
		t.Errorf("gitlab url from flag; got %q", s.GitLab.URL)
	}
	if s.Bitbucket.Username != "alice-from-env" {
		t.Errorf("bitbucket username from env; got %q", s.Bitbucket.Username)
	}
}

// ---- loadConfig ----

func TestLoadConfig_DefaultPathMissingIsOK(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	logger, _ := testLogger()
	cfg, err := loadConfig("", logger)
	if err != nil {
		t.Fatalf("expected no error for missing default config; got %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
}

func TestLoadConfig_ExplicitPathMissingErrors(t *testing.T) {
	logger, _ := testLogger()
	_, err := loadConfig(filepath.Join(t.TempDir(), "nope.yaml"), logger)
	if err == nil {
		t.Fatal("expected error for explicit missing path")
	}
}

func TestLoadConfig_ExplicitPathReadsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	yaml := "providers:\n  github:\n    token: from-file\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	logger, _ := testLogger()
	cfg, err := loadConfig(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers.GitHub.Token != "from-file" {
		t.Errorf("expected token from file; got %q", cfg.Providers.GitHub.Token)
	}
}

func TestLoadConfig_NoHomeDetectedDebugLogs(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "") // Windows
	logger, buf := testLogger()
	cfg, err := loadConfig("", logger)
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	// The debug log only appears when no home is detected. UserHomeDir may
	// still find a path on some systems; only assert when truly empty.
	if config.DefaultPath() == "" && !strings.Contains(buf.String(), "no config home detected") {
		t.Errorf("expected debug log when no config home detected; got %s", buf.String())
	}
}

// ---- end-to-end resolution + provider construction ----

func TestResolveSettings_ConfigFileEndToEnd(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("BITBUCKET_USERNAME", "")
	t.Setenv("BITBUCKET_APP_PASSWORD", "")
	t.Setenv("BITBUCKET_SERVER_USERNAME", "")
	t.Setenv("BITBUCKET_SERVER_TOKEN", "")
	t.Setenv("GITEA_TOKEN", "")

	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	yaml := `providers:
  github:
    token: ghp_x
  gitlab:
    url: https://gl.x.com
    token: gl_x
  bitbucket:
    username: alice
    app_password: bb_x
  bitbucket-server:
    url: https://bb.x.com
    username: alice
    token: bbs_x
  gitea:
    url: https://gt.x.com
    token: gt_x
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	logger, _ := testLogger()
	cfg, err := loadConfig(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	s := resolveSettings(migrateFlags{}, cfg)

	if s.GitHub.Token != "ghp_x" {
		t.Errorf("github token = %q", s.GitHub.Token)
	}
	if s.GitLab.URL != "https://gl.x.com" || s.GitLab.Token != "gl_x" {
		t.Errorf("gitlab = %+v", s.GitLab)
	}
	if s.Bitbucket.Username != "alice" || s.Bitbucket.AppPassword != "bb_x" {
		t.Errorf("bitbucket = %+v", s.Bitbucket)
	}
	if s.BitbucketServer.URL != "https://bb.x.com" || s.BitbucketServer.Token != "bbs_x" {
		t.Errorf("bitbucket-server = %+v", s.BitbucketServer)
	}
	if s.Gitea.URL != "https://gt.x.com" || s.Gitea.Token != "gt_x" {
		t.Errorf("gitea = %+v", s.Gitea)
	}
}

// TestBuildProviders_PassesSettingsToProviders verifies the chain from
// providerSettings through buildProviders into the provider's behavior.
// We can't introspect provider internals, but we can verify that AuthURL
// reflects the token we set, which exercises the full settings → provider
// → operation path.
func TestBuildProviders_PassesSettingsToProviders(t *testing.T) {
	ps, err := buildProviders(providerSettings{
		GitHub: githubSettings{Token: "passed-through"},
	})
	if err != nil {
		t.Fatal(err)
	}
	gh := provider.ByName(ps, "github")
	u := mustParse(t, "https://github.com/owner/repo.git")
	authed, err := gh.AuthURL(u)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(authed, "passed-through") {
		t.Errorf("token should have made it into the auth URL; got %q", authed)
	}
}

// ---- resolve helper ----

// TestNewLogger_OutputIsRedacted locks in the wiring contract that
// newLoggerTo (and therefore newLogger) actually applies redact.New around
// the underlying text handler. The redact package's own tests cover the
// redaction policy; this test catches a refactor that drops the wrap.
func TestNewLogger_OutputIsRedacted(t *testing.T) {
	var buf bytes.Buffer
	logger := newLoggerTo(&buf, 1) // -v: Debug level
	logger.Info("pushing", "dst", "https://x-access-token:secret-token@github.com/x/y.git")

	out := buf.String()
	if strings.Contains(out, "secret-token") {
		t.Errorf("token leaked through CLI logger; got %s", out)
	}
	if !strings.Contains(out, "https://redacted@github.com") {
		t.Errorf("expected URL userinfo redacted; got %s", out)
	}
}

// TestNewLoggerTo_VerbosityLadder pins the verbosity contract so a future
// refactor can't silently regress the default level.
//
// The default rung is intentionally Info (not Warn): users who run
// gitraft without `-v` should still see phase markers like "cloning
// source" / "pushing destination". `-v` and `-vv` both currently
// resolve to Debug; the count is preserved for forward compatibility
// but must not _drop_ levels below what the previous step emitted.
func TestNewLoggerTo_VerbosityLadder(t *testing.T) {
	cases := []struct {
		name   string
		v      int
		level  slog.Level
		emits  bool
		needle string
	}{
		// Default (no -v) MUST surface Info — this is the regression we want
		// to catch if anyone "fixes" the ladder back to Warn-by-default.
		{"v0_info_emits", 0, slog.LevelInfo, true, "phase-info"},
		{"v0_debug_silent", 0, slog.LevelDebug, false, "phase-debug"},
		// -v unlocks Debug; Info still flows.
		{"v1_info_emits", 1, slog.LevelInfo, true, "phase-info"},
		{"v1_debug_emits", 1, slog.LevelDebug, true, "phase-debug"},
		// -vv is currently a no-op above -v; Debug still flows, nothing
		// quieter than Debug exists in slog.
		{"v2_debug_emits", 2, slog.LevelDebug, true, "phase-debug"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := newLoggerTo(&buf, tc.v)
			logger.Log(context.Background(), tc.level, tc.needle)
			got := strings.Contains(buf.String(), tc.needle)
			if got != tc.emits {
				t.Errorf("v=%d level=%v: emit=%v, want %v\noutput: %q",
					tc.v, tc.level, got, tc.emits, buf.String())
			}
		})
	}
}

func TestResolve_PriorityOrder(t *testing.T) {
	cases := []struct {
		name string
		flag string
		env  string
		cfg  string
		want string
	}{
		{"all empty", "", "", "", ""},
		{"flag wins", "F", "E", "C", "F"},
		{"env wins when no flag", "", "E", "C", "E"},
		{"config wins when others empty", "", "", "C", "C"},
		{"flag wins even when env empty", "F", "", "C", "F"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolve(tc.flag, tc.env, tc.cfg); got != tc.want {
				t.Errorf("resolve(%q,%q,%q) = %q; want %q", tc.flag, tc.env, tc.cfg, got, tc.want)
			}
		})
	}
}
