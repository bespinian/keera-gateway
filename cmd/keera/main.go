// Command keera administers a Keera Gateway. It only uses the control API,
// never the database, so it works the same against any gateway.
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

// run is the command, apart from os.Exit, so that its deferred calls run.
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
