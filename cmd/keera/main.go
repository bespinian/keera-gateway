// Command keera administers a Keera Gateway through the control API only.
package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/bespinian/keera-gateway/internal/cli"
)

func main() { os.Exit(run()) }

// run is kept apart from os.Exit so its deferred calls run.
func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := cli.Run(ctx, os.Args[1:])
	if err == nil {
		return 0
	}
	if code, ok := errors.AsType[cli.ExitCode](err); ok {
		return int(code)
	}
	cli.Fail(err)
	return 1
}
