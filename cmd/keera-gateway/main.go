// Command keera-gateway is the Keera Gateway server: the inference data plane
// and the control plane in one binary.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/bespinian/keera-gateway/internal/server"
)

func main() {
	// SIGTERM is how an orchestrator stops a pod.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := server.Run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "keera-gateway:", err)
		os.Exit(1)
	}
}
