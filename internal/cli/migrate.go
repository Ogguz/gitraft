package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"github.com/Ogguz/gitraft/internal/config"
	"github.com/Ogguz/gitraft/internal/mirror"
	"github.com/Ogguz/gitraft/internal/provider"
	"github.com/Ogguz/gitraft/internal/redact"
	"github.com/Ogguz/gitraft/internal/provider/bitbucket"
	"github.com/Ogguz/gitraft/internal/provider/bitbucketserver"
	"github.com/Ogguz/gitraft/internal/provider/gitea"
	"github.com/Ogguz/gitraft/internal/provider/github"
	"github.com/Ogguz/gitraft/internal/provider/gitlab"
	"github.com/spf13/cobra"
)

type migrateFlags struct {
	DryRun         bool
	Cleanup        bool
	SourceProvider string
	DestProvider   string
	Visibility     string
	Description    string
	SkipCreate     bool
	GitLabURL      string
	BitbucketURL   string
	GiteaURL       string
	Config         string
}

func newMigrateCmd() *cobra.Command {
	var f migrateFlags
	cmd := &cobra.Command{
		Use:   "migrate [SOURCE DESTINATION]",
		Short: "Mirror a git repository from SOURCE to DESTINATION",
		Long: "Mirror a git repository from SOURCE to DESTINATION.\n\n" +
			"Run with two URL arguments for a one-shot migration, or with no\n" +
			"arguments from a TTY to launch the interactive wizard.",
		// Accept 0 or 2 positional args. 0 → wizard (when interactive), error
		// (otherwise). 1 is rejected so users don't accidentally pass only
		// half the URLs and get a confusing wizard prompt for the other half.
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 && len(args) != 2 {
				return fmt.Errorf("expected 0 or 2 arguments (SOURCE DESTINATION), got %d", len(args))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := newLogger(verbose)
			src, dst, err := resolveSourceDest(cmd.Context(), args, os.Stdin, os.Stdout, logger)
			if err != nil {
				return err
			}
			return runMigrate(cmd.Context(), src, dst, f, logger)
		},
		// Cobra prints usage on RunE errors by default; suppress when our
		// errors are domain failures rather than CLI-syntax problems.
		SilenceUsage: true,
	}
	cmd.Flags().BoolVar(&f.DryRun, "dry-run", false, "print commands without executing them")
	cmd.Flags().BoolVar(&f.Cleanup, "cleanup", false, "delete the temporary clone after pushing")
	cmd.Flags().StringVar(&f.SourceProvider, "source-provider", "", "override source provider detection (e.g. github)")
	cmd.Flags().StringVar(&f.DestProvider, "dest-provider", "", "override destination provider detection")
	cmd.Flags().StringVar(&f.Visibility, "visibility", "private", "destination visibility: private|public|internal (default private — safer for migrations)")
	cmd.Flags().StringVar(&f.Description, "description", "", "description for an auto-created destination")
	cmd.Flags().BoolVar(&f.SkipCreate, "skip-create", false, "do not auto-create the destination if it is missing")
	cmd.Flags().StringVar(&f.GitLabURL, "gitlab-url", "", "self-hosted GitLab base URL (default: https://gitlab.com)")
	cmd.Flags().StringVar(&f.BitbucketURL, "bitbucket-url", "", "self-hosted Bitbucket Server / Data Center URL (no default; required to engage that provider)")
	cmd.Flags().StringVar(&f.GiteaURL, "gitea-url", "", "self-hosted Gitea base URL (no default; required to engage that provider)")
	cmd.Flags().StringVar(&f.Config, "config", "", "path to YAML config file (default: $XDG_CONFIG_HOME/gitraft/config.yaml)")
	return cmd
}

// resolveSourceDest returns the (source, destination) URL pair the migrate
// command should operate on. With two positional args, those are used
// directly. With zero args, the interactive wizard runs (in TTY+non-CI
// environments) or the call errors with a helpful hint when interactivity
// isn't available — we'd rather fail loudly than block a CI job waiting
// for stdin.
//
// stdin/stdout are passed in (rather than the helper reaching for the
// os globals directly) so they round-trip into [runWizard] consistently
// and tests can substitute pipe-backed files. After resolution, an
// empty string in either slot is treated as a hard error here so the
// downstream provider.Parse never sees "" — the message is more
// actionable than a parser-level "empty url".
func resolveSourceDest(ctx context.Context, args []string, stdin, stdout *os.File, logger *slog.Logger) (string, string, error) {
	src, dst, err := resolveSourceDestImpl(ctx, args, stdin, stdout, logger)
	if err != nil {
		return "", "", err
	}
	if src == "" || dst == "" {
		return "", "", errors.New(
			"source and destination URLs must both be non-empty (got empty after resolving args/wizard)",
		)
	}
	return src, dst, nil
}

func resolveSourceDestImpl(ctx context.Context, args []string, stdin, stdout *os.File, logger *slog.Logger) (string, string, error) {
	if len(args) == 2 {
		return args[0], args[1], nil
	}
	if !isInteractiveFn(stdin, stdout) {
		return "", "", errors.New(
			"two URL arguments required (source destination); run from a TTY (and not in CI) to use the interactive wizard, or pass --non-interactive=false explicitly",
		)
	}
	// Print directly to stdout so the user gets a visible heads-up before the
	// TUI takes over — slog's default level (Warn) would suppress an Info
	// log here.
	_, _ = fmt.Fprintln(stdout, "Launching interactive wizard...")
	logger.Info("no arguments — launching interactive wizard")
	res, err := runWizardFn(ctx, stdin, stdout)
	if err != nil {
		return "", "", err
	}
	return res.source, res.destination, nil
}

func runMigrate(ctx context.Context, srcRaw, dstRaw string, f migrateFlags, logger *slog.Logger) error {
	cfg, err := loadConfig(f.Config, logger)
	if err != nil {
		return err
	}
	settings := resolveSettings(f, cfg)
	providers, err := buildProviders(settings)
	if err != nil {
		return err
	}

	srcURL, err := provider.Parse(srcRaw)
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	dstURL, err := provider.Parse(dstRaw)
	if err != nil {
		return fmt.Errorf("destination: %w", err)
	}

	srcProv, err := pickProvider(providers, srcURL, f.SourceProvider)
	if err != nil {
		return fmt.Errorf("source provider: %w", err)
	}
	dstProv, err := pickProvider(providers, dstURL, f.DestProvider)
	if err != nil {
		return fmt.Errorf("destination provider: %w", err)
	}

	// Warn about missing tokens only for providers we actually resolved to —
	// avoids noise when e.g. the user is migrating gitlab→gitlab.
	warnMissingTokens(logger, settings, srcProv, dstProv)

	if dstProv != nil && !f.SkipCreate {
		vis, err := provider.ParseVisibility(f.Visibility)
		if err != nil {
			return err
		}
		if err := ensureDestination(ctx, dstProv, dstURL, f.Description, vis, logger); err != nil {
			return err
		}
	}

	srcAuthed, err := authURL(srcProv, srcURL, srcRaw, logger)
	if err != nil {
		return fmt.Errorf("source auth: %w", err)
	}
	dstAuthed, err := authURL(dstProv, dstURL, dstRaw, logger)
	if err != nil {
		return fmt.Errorf("destination auth: %w", err)
	}

	return mirror.Run(ctx, mirror.Options{
		Source:      srcAuthed,
		Destination: dstAuthed,
		DryRun:      f.DryRun,
		Cleanup:     f.Cleanup,
		Logger:      logger,
	})
}

// providerSettings holds per-provider configuration after resolving values
// from CLI flags, environment variables, and the config file. Resolution
// priority is flag > env > config (highest to lowest), encoded by the call
// order to resolve(). Per-provider sub-structs co-locate the auth-completeness
// rule with the data via Authenticated().
type providerSettings struct {
	GitHub          githubSettings
	GitLab          gitlabSettings
	Bitbucket       bitbucketSettings
	BitbucketServer bitbucketServerSettings
	Gitea           giteaSettings
}

type githubSettings struct {
	Token string
}

func (s githubSettings) Authenticated() bool { return s.Token != "" }

type gitlabSettings struct {
	URL, Token string
}

func (s gitlabSettings) Authenticated() bool { return s.Token != "" }

type bitbucketSettings struct {
	Username, AppPassword string
}

func (s bitbucketSettings) Authenticated() bool {
	return s.Username != "" && s.AppPassword != ""
}

type bitbucketServerSettings struct {
	URL, Username, Token string
}

func (s bitbucketServerSettings) Authenticated() bool {
	return s.Username != "" && s.Token != ""
}

type giteaSettings struct {
	URL, Token string
}

func (s giteaSettings) Authenticated() bool { return s.Token != "" }

// loadConfig reads the config file. With an empty path argument the default
// location is used and a missing file is treated as no-config (no error).
// With an explicit path, a missing file is an error so typos surface loudly.
// Always emits a permissions warning when the resolved file is too readable.
func loadConfig(explicitPath string, logger *slog.Logger) (*config.Config, error) {
	if explicitPath != "" {
		cfg, err := config.Load(explicitPath)
		if err != nil {
			return nil, err
		}
		config.WarnInsecurePermissions(explicitPath, logger)
		return cfg, nil
	}
	defaultPath := config.DefaultPath()
	if defaultPath == "" {
		logger.Debug("no config home detected; skipping config-file lookup")
		return &config.Config{}, nil
	}
	cfg, err := config.LoadOrEmpty(defaultPath)
	if err != nil {
		return nil, err
	}
	config.WarnInsecurePermissions(defaultPath, logger)
	return cfg, nil
}

// resolveSettings merges values from flags, env vars, and the config file.
// Each call to resolve(...) makes the priority explicit at the call site.
func resolveSettings(f migrateFlags, cfg *config.Config) providerSettings {
	if cfg == nil {
		cfg = &config.Config{}
	}
	return providerSettings{
		GitHub: githubSettings{
			Token: resolve("", os.Getenv("GITHUB_TOKEN"), cfg.Providers.GitHub.Token),
		},
		GitLab: gitlabSettings{
			URL:   resolve(f.GitLabURL, "", cfg.Providers.GitLab.URL),
			Token: resolve("", os.Getenv("GITLAB_TOKEN"), cfg.Providers.GitLab.Token),
		},
		Bitbucket: bitbucketSettings{
			Username:    resolve("", os.Getenv("BITBUCKET_USERNAME"), cfg.Providers.Bitbucket.Username),
			AppPassword: resolve("", os.Getenv("BITBUCKET_APP_PASSWORD"), cfg.Providers.Bitbucket.AppPassword),
		},
		BitbucketServer: bitbucketServerSettings{
			URL:      resolve(f.BitbucketURL, "", cfg.Providers.BitbucketServer.URL),
			Username: resolve("", os.Getenv("BITBUCKET_SERVER_USERNAME"), cfg.Providers.BitbucketServer.Username),
			Token:    resolve("", os.Getenv("BITBUCKET_SERVER_TOKEN"), cfg.Providers.BitbucketServer.Token),
		},
		Gitea: giteaSettings{
			URL:   resolve(f.GiteaURL, "", cfg.Providers.Gitea.URL),
			Token: resolve("", os.Getenv("GITEA_TOKEN"), cfg.Providers.Gitea.Token),
		},
	}
}

// resolve picks the first non-empty value in priority order: flag > env > config.
// Empty arguments mean "no source for this slot" — handy when a setting has
// no env var (URLs) or no flag (tokens).
func resolve(flag, env, cfg string) string {
	for _, v := range [...]string{flag, env, cfg} {
		if v != "" {
			return v
		}
	}
	return ""
}

// buildProviders constructs the set of available providers from the resolved
// settings. Does NOT emit token warnings — those are deferred to
// warnMissingTokens so we only warn for providers the user actually reached.
//
// All host-bearing settings are validated and errors are joined so a user
// with multiple bad URLs fixes everything in one round-trip.
func buildProviders(s providerSettings) ([]provider.Provider, error) {
	gitlabHostname, gErr := gitlabHost(s.GitLab.URL)
	bbServerHostname, bbErr := bitbucketServerHost(s.BitbucketServer.URL)
	giteaHostname, gtErr := giteaHost(s.Gitea.URL)
	if gErr != nil || bbErr != nil || gtErr != nil {
		return nil, errors.Join(gErr, bbErr, gtErr)
	}
	return []provider.Provider{
		github.New(github.Options{Token: s.GitHub.Token}),
		gitlab.New(gitlab.Options{Token: s.GitLab.Token, Host: gitlabHostname}),
		bitbucket.New(bitbucket.Options{
			Username:    s.Bitbucket.Username,
			AppPassword: s.Bitbucket.AppPassword,
		}),
		bitbucketserver.New(bitbucketserver.Options{
			Host:     bbServerHostname,
			Username: s.BitbucketServer.Username,
			Token:    s.BitbucketServer.Token,
		}),
		gitea.New(gitea.Options{
			Host:  giteaHostname,
			Token: s.Gitea.Token,
		}),
	}, nil
}

// providerWarnings is the message emitted when any required auth value is
// unset for that provider (after resolving env and config). Single message
// per provider keeps the log compact.
var providerWarnings = map[string]string{
	"github":           "GITHUB_TOKEN unset; GitHub API calls will be unauthenticated (public repos only)",
	"gitlab":           "GITLAB_TOKEN unset; GitLab API calls will be unauthenticated (public projects only)",
	"bitbucket":        "BITBUCKET_USERNAME or BITBUCKET_APP_PASSWORD unset; Bitbucket API calls will be unauthenticated (public repos only)",
	"bitbucket-server": "BITBUCKET_SERVER_USERNAME or BITBUCKET_SERVER_TOKEN unset; Bitbucket Server API calls will be unauthenticated",
	"gitea":            "GITEA_TOKEN unset; Gitea API calls will be unauthenticated (public repos only)",
}

// providersWithInternalVisibility names providers that support a per-repo
// "internal" tier natively. When a destination provider is NOT in this set
// and the user asks for VisibilityInternal, ensureDestination warns about
// the silent collapse to "private".
var providersWithInternalVisibility = map[string]bool{
	"gitlab": true,
}

// parseHostFromURL extracts a hostname (no port) from either a full URL
// ("https://host:8080/path") or a bare hostname ("host" or "host:port").
// Empty input returns empty output (caller decides what that means).
// Invalid input returns an error tagged with the originating flag name.
func parseHostFromURL(raw, flagName string) (string, error) {
	if raw == "" {
		return "", nil
	}
	candidate := raw
	if !strings.Contains(raw, "://") {
		candidate = "https://" + raw
	}
	u, err := url.Parse(candidate)
	if err != nil {
		return "", fmt.Errorf("invalid %s %q: %w", flagName, raw, err)
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("invalid %s %q: no host", flagName, raw)
	}
	return host, nil
}

// gitlabHost reads --gitlab-url; empty means "use the gitlab.com default".
func gitlabHost(raw string) (string, error) {
	return parseHostFromURL(raw, "--gitlab-url")
}

// bitbucketServerHost reads --bitbucket-url; empty means "not configured" —
// the Bitbucket Server provider's Matches will return false for every URL.
func bitbucketServerHost(raw string) (string, error) {
	return parseHostFromURL(raw, "--bitbucket-url")
}

// giteaHost reads --gitea-url; empty means "use the gitea.com default".
func giteaHost(raw string) (string, error) {
	return parseHostFromURL(raw, "--gitea-url")
}

// warnMissingTokens emits one warning per provider in `used` whose effective
// auth values (after resolving env + config) are unset. Duplicates collapse
// so a source+dest on the same provider yields a single warning.
func warnMissingTokens(logger *slog.Logger, s providerSettings, used ...provider.Provider) {
	seen := map[string]bool{}
	for _, p := range used {
		if p == nil {
			continue
		}
		name := p.Name()
		if seen[name] {
			continue
		}
		seen[name] = true
		if !providerAuthSatisfied(name, s) {
			if msg, ok := providerWarnings[name]; ok {
				logger.Warn(msg)
			}
		}
	}
}

// providerAuthSatisfied reports whether the resolved settings have all the
// values needed for that provider to authenticate API calls. Unknown
// provider names default to FALSE (i.e., warn) so a future provider added
// without updating this dispatch fails loudly via the "TOKEN unset" warning
// rather than silently skipping it.
func providerAuthSatisfied(name string, s providerSettings) bool {
	switch name {
	case "github":
		return s.GitHub.Authenticated()
	case "gitlab":
		return s.GitLab.Authenticated()
	case "bitbucket":
		return s.Bitbucket.Authenticated()
	case "bitbucket-server":
		return s.BitbucketServer.Authenticated()
	case "gitea":
		return s.Gitea.Authenticated()
	}
	return false
}

// pickProvider resolves override-by-name first, falling back to URL auto-detection.
// Returns an error for unknown overrides so typos surface loudly at flag time
// rather than silently disabling auth.
func pickProvider(ps []provider.Provider, u *url.URL, override string) (provider.Provider, error) {
	if override == "" {
		return provider.Detect(ps, u), nil
	}
	if p := provider.ByName(ps, override); p != nil {
		return p, nil
	}
	return nil, fmt.Errorf("unknown provider %q; available: %s", override, strings.Join(providerNames(ps), ", "))
}

func providerNames(ps []provider.Provider) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name())
	}
	return out
}

// ensureDestination checks if the destination repo exists; if not, creates it.
// A 409/422 "already exists" from CreateRepo is treated as a benign race.
//
// Warns when VisibilityInternal would silently collapse to "private" because
// the destination provider doesn't have a native internal tier.
func ensureDestination(ctx context.Context, p provider.Provider, u *url.URL, description string, vis provider.Visibility, logger *slog.Logger) error {
	owner, name, err := p.ParseRepo(u)
	if err != nil {
		return fmt.Errorf("parse destination: %w", err)
	}
	exists, err := p.RepoExists(ctx, owner, name)
	if err != nil {
		return fmt.Errorf("check destination: %w", err)
	}
	if exists {
		logger.Info("destination exists; skipping create", "provider", p.Name(), "owner", owner, "name", name)
		return nil
	}
	if vis == provider.VisibilityInternal && !providersWithInternalVisibility[p.Name()] {
		logger.Warn("provider does not support 'internal' visibility; will use 'private' instead", "provider", p.Name())
	}
	logger.Info("creating destination", "provider", p.Name(), "owner", owner, "name", name, "visibility", vis.String())
	err = p.CreateRepo(ctx, provider.CreateOptions{
		Owner:       owner,
		Name:        name,
		Description: description,
		Visibility:  vis,
	})
	if errors.Is(err, provider.ErrRepoAlreadyExists) {
		logger.Warn("destination appeared during migration; proceeding", "owner", owner, "name", name)
		return nil
	}
	if err != nil {
		return fmt.Errorf("create destination: %w", err)
	}
	return nil
}

// authURL embeds auth into the URL via the provider, or returns the raw input
// if the provider is nil (unknown host). Warns in the nil case so the user
// knows auth wasn't applied and can expect opaque git-level failures. When
// the URL shape suggests Bitbucket Server, surface a hint about --bitbucket-url.
func authURL(p provider.Provider, u *url.URL, raw string, logger *slog.Logger) (string, error) {
	if p == nil {
		msg := "no provider matched; pushing without embedded auth — set up SSH keys or a credential helper if needed"
		if looksLikeBitbucketServerURL(u) {
			msg += fmt.Sprintf(" — URL looks like Bitbucket Server, try --bitbucket-url=https://%s", u.Host)
		}
		logger.Warn(msg, "host", u.Host)
		return raw, nil
	}
	return p.AuthURL(u)
}

// looksLikeBitbucketServerURL returns true when the URL path matches one of
// the Bitbucket Server URL shapes (clone or browser). Used to give the user
// an actionable hint when no provider matched.
func looksLikeBitbucketServerURL(u *url.URL) bool {
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) >= 3 && parts[0] == "scm" {
		return true
	}
	if len(parts) >= 4 && parts[0] == "projects" && parts[2] == "repos" {
		return true
	}
	return false
}

func newLogger(v int) *slog.Logger {
	return newLoggerTo(os.Stderr, v)
}

// newLoggerTo is the testable seam: builds the same handler chain as
// newLogger but writes to an arbitrary writer. The redact wrap is the whole
// point of the chain — applied here so a CLI test can assert it actually
// fires (not just that the redact package works in isolation).
func newLoggerTo(w io.Writer, v int) *slog.Logger {
	level := slog.LevelWarn
	switch {
	case v == 1:
		level = slog.LevelInfo
	case v >= 2:
		level = slog.LevelDebug
	}
	return slog.New(redact.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})))
}
