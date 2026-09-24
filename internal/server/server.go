// Package server is the Keera Gateway server: it wires the inference data plane
// and the control plane together and serves them from one listener, told apart
// by path.
package server

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/bespinian/keera-gateway/internal/version"
)

const usageText = `keera-gateway - Keera Gateway: the LLM gateway and control plane

Usage:
  keera-gateway serve     run the gateway: panel, inference API and control API
  keera-gateway migrate   apply the schema and the model catalogue, then exit
  keera-gateway version   print the build version

One listener serves all three, told apart by path rather than by port:

  /            the control panel
  /api/v1/...  the inference API (OpenAI- and Anthropic-shaped)
  /control/... the control API

Administration - organisations, teams, keys, guardrails, usage - is the 'keera'
command, which talks to the control API over HTTP.

The server reads its configuration from the environment:
  KEERA_DATABASE_URL   Postgres connection string                    (required)
  KEERA_OPERATOR_KEY      credential for the control API                (required)
  KEERA_ADDR           the listener                                  (default :8080)
  KEERA_MODELS_FILE    model catalogue applied on start, and read-only elsewhere
  KEERA_SECRET_KEY     encrypts API keys set in the panel; without it they
                      cannot be set there    (openssl rand -hex 32)
  KEERA_LOG_LEVEL      debug, info, warn or error                    (default info)
  KEERA_LOG_FORMAT     text or json                                  (default text)

A hosted model's API key is either set on the model - in the control panel, or
with 'keera model set <alias> --api-key @-', which reads it from stdin rather
than a shell history - and stored encrypted with KEERA_SECRET_KEY, or it is named
as an environment variable by the model, which for the anthropic and openai
providers defaults to ANTHROPIC_API_KEY and OPENAI_API_KEY. See
docs/providers.md.
`

// Run dispatches one invocation. It returns an error rather than exiting, so
// the entry point owns the exit code.
func Run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Print(usageText)
		return errors.New("no command given")
	}

	switch cmd := args[0]; cmd {
	case "serve":
		return serve(ctx)
	case "migrate":
		return migrate(ctx)
	case "version":
		fmt.Println(version.String())
		return nil
	case "help", "-h", "--help":
		fmt.Print(usageText)
		return nil
	default:
		fmt.Fprint(os.Stderr, usageText)
		return fmt.Errorf("unknown command: %s", cmd)
	}
}
