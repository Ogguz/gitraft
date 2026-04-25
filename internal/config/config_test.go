package config_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Ogguz/gitraft/internal/config"
)

func TestLoadOrEmpty_MissingFileReturnsEmpty(t *testing.T) {
	cfg, err := config.LoadOrEmpty(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("expected nil error for missing file; got %v", err)
	}
	if cfg == nil {
		t.Fatal("expected empty config; got nil")
	}
	if cfg.Providers.GitHub.Token != "" {
		t.Errorf("expected zero-value config; got %+v", cfg)
	}
}

func TestLoadOrEmpty_EmptyPathReturnsEmpty(t *testing.T) {
	cfg, err := config.LoadOrEmpty("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers.GitHub.Token != "" {
		t.Error("expected zero-value config")
	}
}

func TestLoadOrEmpty_ZeroByteFileReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.yaml")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadOrEmpty(path)
	if err != nil {
		t.Fatalf("zero-byte file should be treated as empty config; got %v", err)
	}
	if cfg.Providers.GitHub.Token != "" {
		t.Errorf("expected zero-value config; got %+v", cfg)
	}
}

func TestLoad_MissingFileErrors(t *testing.T) {
	_, err := config.Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("expected error for missing explicit-path file")
	}
}

func TestLoadOrEmpty_ValidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	yaml := `version: 1
providers:
  github:
    token: ghp_test
  gitlab:
    url: https://gitlab.example.com
    token: glpat_test
  bitbucket:
    username: alice
    app_password: bb_pw
  bitbucket-server:
    url: https://bb.example.com
    username: alice
    token: bb_pat
  gitea:
    url: https://gitea.example.com
    token: gt_pat
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadOrEmpty(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Version != 1 {
		t.Errorf("version = %d", cfg.Version)
	}
	if cfg.Providers.GitHub.Token != "ghp_test" {
		t.Errorf("github.token = %q", cfg.Providers.GitHub.Token)
	}
	if cfg.Providers.GitLab.URL != "https://gitlab.example.com" {
		t.Errorf("gitlab.url = %q", cfg.Providers.GitLab.URL)
	}
	if cfg.Providers.GitLab.Token != "glpat_test" {
		t.Errorf("gitlab.token = %q", cfg.Providers.GitLab.Token)
	}
	if cfg.Providers.Bitbucket.Username != "alice" || cfg.Providers.Bitbucket.AppPassword != "bb_pw" {
		t.Errorf("bitbucket = %+v", cfg.Providers.Bitbucket)
	}
	if cfg.Providers.BitbucketServer.URL != "https://bb.example.com" {
		t.Errorf("bitbucket-server.url = %q", cfg.Providers.BitbucketServer.URL)
	}
	if cfg.Providers.BitbucketServer.Token != "bb_pat" {
		t.Errorf("bitbucket-server.token = %q", cfg.Providers.BitbucketServer.Token)
	}
	if cfg.Providers.Gitea.URL != "https://gitea.example.com" || cfg.Providers.Gitea.Token != "gt_pat" {
		t.Errorf("gitea = %+v", cfg.Providers.Gitea)
	}
}

func TestLoadOrEmpty_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte("this: is\n: bad yaml\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := config.LoadOrEmpty(path)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "parse config") {
		t.Errorf("expected 'parse config' in error; got %v", err)
	}
}

func TestLoadOrEmpty_PartialConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "partial.yaml")
	yaml := `providers:
  github:
    token: ghp_only
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadOrEmpty(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers.GitHub.Token != "ghp_only" {
		t.Errorf("github.token = %q", cfg.Providers.GitHub.Token)
	}
	if cfg.Providers.GitLab.Token != "" {
		t.Errorf("gitlab.token should be zero; got %q", cfg.Providers.GitLab.Token)
	}
}

func TestLoadOrEmpty_UnknownTopLevelKeyErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "typo.yaml")
	yaml := `provdiers:
  github:
    token: ghp_x
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := config.LoadOrEmpty(path)
	if err == nil {
		t.Fatal("expected error for unknown top-level key (typo)")
	}
	if !strings.Contains(err.Error(), "field provdiers not found") {
		t.Errorf("expected unknown-field error mentioning 'provdiers'; got %v", err)
	}
}

func TestLoadOrEmpty_UnknownProviderKeyErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "typo.yaml")
	yaml := `providers:
  gitub:
    token: ghp_x
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := config.LoadOrEmpty(path)
	if err == nil {
		t.Fatal("expected error for unknown provider key (typo)")
	}
	if !strings.Contains(err.Error(), "gitub") {
		t.Errorf("expected error mentioning the typo'd key 'gitub'; got %v", err)
	}
}

func TestLoadOrEmpty_TypeMismatchErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wrongtype.yaml")
	yaml := `providers:
  github:
    token: 42
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadOrEmpty(path)
	// yaml.v3 actually coerces scalars liberally; accept either parse-error or
	// stringified value, but make sure we don't silently end up with empty.
	if err == nil && cfg.Providers.GitHub.Token == "" {
		t.Error("type mismatch silently dropped the value")
	}
}

func TestLoadOrEmpty_ArrayInsteadOfMapErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wrongshape.yaml")
	yaml := `providers:
  - github
  - gitlab
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := config.LoadOrEmpty(path)
	if err == nil {
		t.Fatal("expected error for sequence in place of mapping")
	}
}

func TestValidate_GoodURLs(t *testing.T) {
	cfg := &config.Config{Providers: config.Providers{
		GitLab:          config.GitLabProvider{URL: "https://gitlab.example.com"},
		BitbucketServer: config.BitbucketServerProvider{URL: "https://bb.example.com"},
		Gitea:           config.GiteaProvider{URL: "https://gitea.example.com"},
	}}
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected no error; got %v", err)
	}
}

func TestValidate_EmptyURLsOK(t *testing.T) {
	cfg := &config.Config{}
	if err := cfg.Validate(); err != nil {
		t.Errorf("empty config must validate cleanly; got %v", err)
	}
}

func TestValidate_MissingScheme(t *testing.T) {
	cfg := &config.Config{Providers: config.Providers{
		GitLab: config.GitLabProvider{URL: "gitlab.example.com"},
	}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for URL without scheme")
	}
	if !strings.Contains(err.Error(), "providers.gitlab.url") {
		t.Errorf("error should name the offending field; got %v", err)
	}
}

func TestValidate_MultipleErrorsJoined(t *testing.T) {
	cfg := &config.Config{Providers: config.Providers{
		GitLab:          config.GitLabProvider{URL: "no-scheme.com"},
		BitbucketServer: config.BitbucketServerProvider{URL: "also-bad"},
		Gitea:           config.GiteaProvider{URL: "still-bad"},
	}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected joined errors")
	}
	for _, want := range []string{"providers.gitlab.url", "providers.bitbucket-server.url", "providers.gitea.url"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected joined error to mention %q; got %v", want, err)
		}
	}
}

func TestLoadOrEmpty_ValidationErrorWraps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad-url.yaml")
	yaml := `providers:
  gitlab:
    url: bad-no-scheme
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := config.LoadOrEmpty(path)
	if err == nil {
		t.Fatal("expected error for invalid URL in config")
	}
	if !strings.Contains(err.Error(), "validate config") {
		t.Errorf("expected 'validate config' in wrapping; got %v", err)
	}
	// The production wrap uses %q to quote the path, which double-escapes
	// backslashes on Windows (`C:\foo` → `"C:\\foo"`). Assert on the
	// basename only — it has no separators on any OS, so the substring
	// check works portably without forking the test on runtime.GOOS.
	base := filepath.Base(path)
	if !strings.Contains(err.Error(), base) {
		t.Errorf("expected file basename %q in error; got %v", base, err)
	}
}

func TestDefaultPath_XDGConfigHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(string(filepath.Separator), "custom", "xdg"))
	got := config.DefaultPath()
	// Expected value is built with filepath.Join so the separator matches
	// the host OS (forward slashes on Unix, backslashes on Windows).
	// DefaultPath itself uses filepath.Join internally; mirroring the
	// constructor here keeps the test portable instead of hard-coding
	// "/custom/xdg/...".
	want := filepath.Join(string(filepath.Separator), "custom", "xdg", "gitraft", "config.yaml")
	if got != want {
		t.Errorf("DefaultPath() = %q; want %q", got, want)
	}
}

func TestDefaultPath_HomeFallback(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	got := config.DefaultPath()
	if got == "" {
		t.Fatal("expected non-empty default path")
	}
	if !strings.HasSuffix(got, filepath.Join(".config", "gitraft", "config.yaml")) {
		t.Errorf("DefaultPath() = %q; expected suffix .config/gitraft/config.yaml", got)
	}
}

func TestWarnInsecurePermissions_TooOpen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permissions warning is no-op on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "open.yaml")
	if err := os.WriteFile(path, []byte("providers:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	config.WarnInsecurePermissions(path, logger)
	if !strings.Contains(buf.String(), "readable by group/other") {
		t.Errorf("expected permission warning; got %s", buf.String())
	}
}

func TestWarnInsecurePermissions_RestrictiveSilent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permissions warning is no-op on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "secure.yaml")
	if err := os.WriteFile(path, []byte("providers:\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	config.WarnInsecurePermissions(path, logger)
	if buf.Len() != 0 {
		t.Errorf("expected no warning for 0600; got %s", buf.String())
	}
}

func TestWarnInsecurePermissions_NilLoggerSafe(t *testing.T) {
	// Must not panic on a nil logger.
	config.WarnInsecurePermissions("/anything", nil)
}

func TestWarnInsecurePermissions_MissingFileSilent(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	config.WarnInsecurePermissions(filepath.Join(t.TempDir(), "nope"), logger)
	if buf.Len() != 0 {
		t.Errorf("expected no warning for missing file; got %s", buf.String())
	}
}
