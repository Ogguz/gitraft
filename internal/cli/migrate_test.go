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
	_, err := pickProvider([]provider.Provider{gh}, u, "gihub") // typo
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

func TestBuildProviders_WarnsWhenTokenUnset(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	logger, buf := testLogger()
	ps := buildProviders(logger)
	if len(ps) != 1 {
		t.Errorf("expected 1 provider; got %d", len(ps))
	}
	if !strings.Contains(buf.String(), "GITHUB_TOKEN unset") {
		t.Errorf("expected warning; got %s", buf.String())
	}
}

func TestBuildProviders_QuietWhenTokenSet(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")
	logger, buf := testLogger()
	_ = buildProviders(logger)
	if strings.Contains(buf.String(), "GITHUB_TOKEN unset") {
		t.Errorf("expected no warning when token set; got %s", buf.String())
	}
}
