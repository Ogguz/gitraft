package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"github.com/Ogguz/gitraft/internal/mirror"
	"github.com/Ogguz/gitraft/internal/provider"
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
}

func newMigrateCmd() *cobra.Command {
	var f migrateFlags
	cmd := &cobra.Command{
		Use:   "migrate SOURCE DESTINATION",
		Short: "Mirror a git repository from SOURCE to DESTINATION",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMigrate(cmd.Context(), args[0], args[1], f, newLogger(verbose))
		},
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
	cmd.Flags().StringVar(&f.GiteaURL, "gitea-url", "", "self-hosted Gitea base URL (default: https://gitea.com)")
	return cmd
}

func runMigrate(ctx context.Context, srcRaw, dstRaw string, f migrateFlags, logger *slog.Logger) error {
	providers, err := buildProviders(f.GitLabURL, f.BitbucketURL, f.GiteaURL)
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
	warnMissingTokens(logger, srcProv, dstProv)

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

// buildProviders constructs the set of available providers, configured from
// env and flags. Does NOT emit token warnings — those are deferred to
// warnMissingTokens so we only warn for providers the user actually reached.
//
// All host-bearing flags are validated and errors are joined so a user with
// multiple bad flags fixes everything in one round-trip.
func buildProviders(gitlabURL, bitbucketServerURL, giteaURL string) ([]provider.Provider, error) {
	gitlabHostname, gErr := gitlabHost(gitlabURL)
	bbServerHostname, bbErr := bitbucketServerHost(bitbucketServerURL)
	giteaHostname, gtErr := giteaHost(giteaURL)
	if gErr != nil || bbErr != nil || gtErr != nil {
		return nil, errors.Join(gErr, bbErr, gtErr)
	}
	return []provider.Provider{
		github.New(github.Options{Token: os.Getenv("GITHUB_TOKEN")}),
		gitlab.New(gitlab.Options{Token: os.Getenv("GITLAB_TOKEN"), Host: gitlabHostname}),
		bitbucket.New(bitbucket.Options{
			Username:    os.Getenv("BITBUCKET_USERNAME"),
			AppPassword: os.Getenv("BITBUCKET_APP_PASSWORD"),
		}),
		bitbucketserver.New(bitbucketserver.Options{
			Host:     bbServerHostname,
			Username: os.Getenv("BITBUCKET_SERVER_USERNAME"),
			Token:    os.Getenv("BITBUCKET_SERVER_TOKEN"),
		}),
		gitea.New(gitea.Options{
			Host:  giteaHostname,
			Token: os.Getenv("GITEA_TOKEN"),
		}),
	}, nil
}

// providerAuthVars maps provider name to the env vars whose presence enables
// authenticated API calls. Multiple vars means all are required.
var providerAuthVars = map[string][]string{
	"github":           {"GITHUB_TOKEN"},
	"gitlab":           {"GITLAB_TOKEN"},
	"bitbucket":        {"BITBUCKET_USERNAME", "BITBUCKET_APP_PASSWORD"},
	"bitbucket-server": {"BITBUCKET_SERVER_USERNAME", "BITBUCKET_SERVER_TOKEN"},
	"gitea":            {"GITEA_TOKEN"},
}

// providerWarnings is the message emitted when any required env var is unset
// for that provider. Single message per provider keeps the log compact.
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

// warnMissingTokens emits one warning per provider in `used` whose required
// auth env vars are unset. Duplicates collapse so a source+dest on the same
// provider yields a single warning.
func warnMissingTokens(logger *slog.Logger, used ...provider.Provider) {
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
		envVars, ok := providerAuthVars[name]
		if !ok {
			continue
		}
		for _, env := range envVars {
			if os.Getenv(env) == "" {
				logger.Warn(providerWarnings[name])
				break
			}
		}
	}
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
	level := slog.LevelWarn
	switch {
	case v == 1:
		level = slog.LevelInfo
	case v >= 2:
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}
