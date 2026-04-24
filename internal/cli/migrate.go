package cli

import (
	"log/slog"
	"os"

	"github.com/Ogguz/gitraft/internal/mirror"
	"github.com/spf13/cobra"
)

func newMigrateCmd() *cobra.Command {
	var opts mirror.Options
	cmd := &cobra.Command{
		Use:   "migrate SOURCE DESTINATION",
		Short: "Mirror a git repository from SOURCE to DESTINATION",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Source = args[0]
			opts.Destination = args[1]
			opts.Logger = newLogger(verbose)
			return mirror.Run(cmd.Context(), opts)
		},
	}
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "print commands without executing them")
	cmd.Flags().BoolVar(&opts.Cleanup, "cleanup", false, "delete the temporary clone after pushing")
	return cmd
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
