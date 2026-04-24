package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Ogguz/gitraft/internal/cli"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := cli.NewRoot()
	root.Version = fmt.Sprintf("%s (commit %s, built %s)", version, commit, date)
	root.SilenceUsage = true  // usage spam on runtime errors is noise
	root.SilenceErrors = true // we own the error printing below

	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "gitraft:", err)
		os.Exit(1)
	}
}
