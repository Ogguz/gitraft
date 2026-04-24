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
	"github.com/Ogguz/gitraft/internal/provider/github"
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
	return cmd
}

func runMigrate(ctx context.Context, srcRaw, dstRaw string, f migrateFlags, logger *slog.Logger) error {
	providers := buildProviders(logger)

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

// buildProviders returns the set of available providers, configured from env.
// Emits a warning when GITHUB_TOKEN is unset so users don't chase confusing
// 401/404s later.
func buildProviders(logger *slog.Logger) []provider.Provider {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		logger.Warn("GITHUB_TOKEN unset; GitHub API calls will be unauthenticated (public repos only)")
	}
	return []provider.Provider{
		github.New(github.Options{Token: token}),
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
// knows auth wasn't applied and can expect opaque git-level failures.
func authURL(p provider.Provider, u *url.URL, raw string, logger *slog.Logger) (string, error) {
	if p == nil {
		logger.Warn("no provider matched; pushing without embedded auth — set up SSH keys or a credential helper if needed", "host", u.Host)
		return raw, nil
	}
	return p.AuthURL(u)
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
