// Package config reads gitraft's optional YAML config file.
//
// Resolution order for any setting (highest to lowest priority):
//  1. CLI flag (e.g., --gitlab-url)
//  2. Environment variable (e.g., GITLAB_TOKEN)
//  3. Config file (this package)
//
// The config file is optional. The default path is
// $XDG_CONFIG_HOME/gitraft/config.yaml, falling back to
// $HOME/.config/gitraft/config.yaml when XDG_CONFIG_HOME is unset.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"

	"gopkg.in/yaml.v3"
)

// Config is the top-level config schema.
//
// The Version field is reserved for future schema migrations; current code
// accepts any value (including the zero value, meaning "unspecified"). When
// breaking schema changes are introduced, consumers will be able to gate
// behavior on Version explicitly.
type Config struct {
	Version   int       `yaml:"version,omitempty"`
	Providers Providers `yaml:"providers"`
}

// Providers groups per-provider settings.
type Providers struct {
	GitHub          GitHubProvider          `yaml:"github"`
	GitLab          GitLabProvider          `yaml:"gitlab"`
	Bitbucket       BitbucketProvider       `yaml:"bitbucket"`
	BitbucketServer BitbucketServerProvider `yaml:"bitbucket-server"`
	Gitea           GiteaProvider           `yaml:"gitea"`
}

// GitHubProvider is the GitHub (SaaS or GitHub Enterprise Server) provider
// section. URL empty means github.com (the SaaS default); a non-empty URL
// engages GHE mode (the provider's /api/v3 endpoint and host-specific
// URL routing).
type GitHubProvider struct {
	URL   string `yaml:"url"`
	Token string `yaml:"token"`
}

// GitLabProvider is the GitLab (SaaS or self-hosted) provider section.
type GitLabProvider struct {
	URL   string `yaml:"url"`
	Token string `yaml:"token"`
}

// BitbucketProvider is the Bitbucket Cloud (bitbucket.org) provider section.
type BitbucketProvider struct {
	Username    string `yaml:"username"`
	AppPassword string `yaml:"app_password"`
}

// BitbucketServerProvider is the Bitbucket Server / Data Center provider section.
type BitbucketServerProvider struct {
	URL      string `yaml:"url"`
	Username string `yaml:"username"`
	Token    string `yaml:"token"`
}

// GiteaProvider is the Gitea provider section.
type GiteaProvider struct {
	URL   string `yaml:"url"`
	Token string `yaml:"token"`
}

// DefaultPath returns the default config-file path:
//
//	$XDG_CONFIG_HOME/gitraft/config.yaml, or
//	$HOME/.config/gitraft/config.yaml as a fallback.
//
// Returns "" when neither XDG_CONFIG_HOME nor a home directory is available.
func DefaultPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "gitraft", "config.yaml")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "gitraft", "config.yaml")
}

// Load reads, parses, and validates the YAML file at path. Returns an error
// if the file is missing — used when the user explicitly asked for a
// particular config (via --config) and we don't want to silently ignore a
// typo.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	return parseAndValidate(data, path)
}

// LoadOrEmpty reads, parses, and validates the YAML file at path. Returns an
// empty Config (no error) when the file is missing — used for the default
// lookup so users who don't have a config file see no friction.
func LoadOrEmpty(path string) (*Config, error) {
	if path == "" {
		return &Config{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	return parseAndValidate(data, path)
}

// Validate checks the in-memory config for semantic errors. Currently it
// verifies that any URL fields parse as URLs with scheme and host so the
// failure surfaces at load time with file context, not at request time deep
// inside a provider call.
func (c *Config) Validate() error {
	var errs []error
	check := func(field, raw string) {
		if raw == "" {
			return
		}
		u, err := url.Parse(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: invalid URL %q: %w", field, raw, err))
			return
		}
		if u.Scheme == "" || u.Host == "" {
			errs = append(errs, fmt.Errorf("%s: URL %q must include scheme and host", field, raw))
		}
	}
	check("providers.github.url", c.Providers.GitHub.URL)
	check("providers.gitlab.url", c.Providers.GitLab.URL)
	check("providers.bitbucket-server.url", c.Providers.BitbucketServer.URL)
	check("providers.gitea.url", c.Providers.Gitea.URL)
	return errors.Join(errs...)
}

// WarnInsecurePermissions logs a warning when the config file is readable by
// group or other users on Unix-like systems. Tokens are stored plaintext, so
// world/group read access is the same exposure SSH and AWS CLI refuse on
// credential files. No-op on Windows where mode bits don't carry the same
// meaning.
func WarnInsecurePermissions(path string, logger *slog.Logger) {
	if logger == nil || path == "" || runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	mode := info.Mode().Perm()
	if mode&0o077 != 0 {
		logger.Warn("config file is readable by group/other; tokens may leak — run `chmod 600` to restrict",
			"path", path, "mode", fmt.Sprintf("%04o", mode))
	}
}

// parseAndValidate is the shared decode+validate path used by Load and
// LoadOrEmpty. It rejects unknown YAML fields so user typos surface at parse
// time instead of being silently dropped and producing confusing
// "TOKEN unset" warnings later.
func parseAndValidate(data []byte, path string) (*Config, error) {
	cfg, err := decode(data, path)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config %q: %w", path, err)
	}
	return cfg, nil
}

func decode(data []byte, path string) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		// Empty file decodes to io.EOF — treat as zero-value config.
		if errors.Is(err, io.EOF) {
			return &c, nil
		}
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	return &c, nil
}
