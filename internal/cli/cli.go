// Package cli is Keera Gateway's administration command line. It talks to a
// gateway's control API over HTTP, so it works the same against any
// deployment, local or remote.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/bespinian/keera-gateway/internal/version"
)

// Run dispatches one invocation. It returns an error rather than exiting, so
// the entry point owns the exit code.
func Run(ctx context.Context, args []string) error {
	// --url and --color mean the same on every command, so they are taken off
	// first and no command declares them.
	args, err := takeColorMode(args)
	if err != nil {
		return err
	}
	resolveStyle()
	if args, err = takeURL(args); err != nil {
		return err
	}

	if len(args) == 0 {
		fmt.Print(overview())
		return errors.New("no command given")
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "org", "orgs":
		return orgCmd(ctx, rest)
	case "team", "teams":
		return teamCmd(ctx, rest)
	case "user", "users":
		return userCmd(ctx, rest)
	case "key", "keys":
		return keyCmd(ctx, rest)
	case "model", "models":
		return modelCmd(ctx, rest)
	case "filter", "filters":
		return filterCmd(ctx, rest)
	case "router", "routers":
		return routerCmd(ctx, rest)
	case "mcp":
		return mcpCmd(ctx, rest)
	case "sandbox", "sandboxes", "sbx":
		return sandboxCmd(ctx, rest)
	case "guardrail", "guardrails":
		return guardrailCmd(ctx, rest)
	// One part of a guardrail each: the same call as 'guardrail set', with
	// fewer flags to read.
	case "limit", "limits":
		return facetCmd(ctx, "limit", rest)
	case "budget", "budgets":
		return facetCmd(ctx, "budget", rest)
	case "usage":
		return usageCmd(ctx, rest)
	case "failure", "failures":
		return failuresCmd(ctx, rest)
	case "sessions":
		return sessionsCmd(ctx, rest)
	case "session":
		return sessionCmd(ctx, rest)
	case "connect":
		return connectCmd(ctx, rest)
	case "doctor":
		return doctorCmd(ctx, rest)
	case "login":
		return loginCmd(ctx, rest)
	case "logout":
		return logoutCmd(ctx, rest)
	case "whoami":
		return whoamiCmd(ctx, rest)
	case "version":
		fmt.Println(version.String())
		return nil
	case "help", "-h", "--help":
		return helpCmd(ctx, rest)
	default:
		// Suggest the nearest command, or point at the list.
		if near := suggest(cmd); near != "" {
			return fmt.Errorf("no command %q; did you mean 'keera %s'?\n"+
				"Run 'keera' for the whole list", cmd, near)
		}
		return fmt.Errorf("no command %q; run 'keera' for the list", cmd)
	}
}

// Fail prints a command's error, painting the prefix only when stderr is a
// terminal.
func Fail(err error) {
	fmt.Fprintln(os.Stderr, styleErr.bad("keera:"), err)
}

// takeURL moves the global --url from the arguments into urlFlag. Taking it
// here means no command can forget to declare it.
func takeURL(args []string) ([]string, error) {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--url" || arg == "-url":
			if i+1 >= len(args) {
				return nil, errors.New("--url needs an address, such as " +
					"--url https://keera.example.ch")
			}
			i++
			urlFlag = args[i]
		case strings.HasPrefix(arg, "--url=") || strings.HasPrefix(arg, "-url="):
			urlFlag = arg[strings.Index(arg, "=")+1:]
		case arg == "--":
			// What follows belongs to the command being run, as in
			// `keera sandbox ssh box -- git status`.
			return append(out, args[i:]...), nil
		default:
			out = append(out, arg)
			continue
		}
		if urlFlag != "" && !strings.Contains(urlFlag, "://") {
			// People type a bare host, and deployments are served over https.
			urlFlag = "https://" + urlFlag
		}
	}
	return out, nil
}
