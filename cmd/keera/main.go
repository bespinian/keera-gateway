// Command keera administers a Keera Gateway: organisations, teams, people, keys,
// the model catalogue, guardrails and usage. It only uses the control API, never
// the database, so it works the same against any gateway.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/bespinian/keera-gateway/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := cli.Run(ctx, os.Args[1:]); err != nil {
		cli.Fail(err)
		os.Exit(1)
	}
}
