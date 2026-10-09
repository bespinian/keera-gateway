// Package server is the Keera Gateway server: it wires the inference data plane
// and the control plane together and serves them from one listener, told apart
// by path.
package server

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/bespinian/keera-gateway/internal/config"
	"github.com/bespinian/keera-gateway/internal/version"
)

const usageText = `keera-gateway - Keera Gateway: the LLM gateway and control plane

Usage:
  keera-gateway serve     run the gateway: panel, inference API and control API
  keera-gateway health    exit 0 if the gateway on KEERA_ADDR is ready (/readyz)
  keera-gateway version   print the build version

One listener serves all of it, told apart by path rather than by port:

  /             the control panel
  /api/v1/...   the inference API (OpenAI- and Anthropic-shaped)
  /api/mcp/...  the MCP servers the gateway stands in front of
  /control/...  the control API
  /sandbox/...  attaching to a sandbox

Administration - organisations, projects, keys, guardrails, usage - is the 'keera'
command, which talks to the control API over HTTP.

The server reads its configuration from the environment. Three settings are
required:
  KEERA_DATABASE_URL    Postgres connection string
  KEERA_OPERATOR_KEY    credential for the control API
  KEERA_SECRET_KEY      encrypts API keys set in the panel (openssl rand -hex 32)

Every other setting is in docs/install.md.

A hosted model's API key is set on the model - in the control panel, or with
'keera model set <alias> --api-key @-', which reads it from stdin rather than a
shell history - and stored encrypted with KEERA_SECRET_KEY. See
docs/providers.md. KEERA_PROVIDER_<NAME>_API_KEY sets the deployment's own key
for a provider instead, and bills its use. See docs/billing.md.
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
	case "health":
		return health(ctx, config.Addr())
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
