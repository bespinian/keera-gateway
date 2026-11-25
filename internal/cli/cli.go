// Package cli is Keera Gateway's administration command line, over the
// control API.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/bespinian/keera-gateway/internal/version"
)

// Run returns an error rather than exiting, so the caller owns the exit code.
func Run(ctx context.Context, args []string) error {
	// --url and --color apply to every command, so they are taken off first.
	args, err := takeColorMode(args)
	if err != nil {
		return err
	}
	resolveStyle()
	if args, err = takeURL(args); err != nil {
		return err
	}

	if len(args) == 0 {
		fmt.Print(overview(style))
		return errors.New("no command given")
	}

	cmd, rest := args[0], args[1:]
	// helpCmd calls Run, so it cannot sit in runners without an
	// initialization cycle.
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		return helpCmd(ctx, rest)
	}
	if c, ok := find(cmd); ok {
		if run, ok := runners[c.name]; ok {
			return run(ctx, rest)
		}
	}
	if near := suggest(cmd); near != "" {
		return fmt.Errorf("no command %q; did you mean 'keera %s'?\n"+
			"Run 'keera' for the whole list", cmd, near)
	}
	return fmt.Errorf("no command %q; run 'keera' for the list", cmd)
}

// runners runs each command in help.go's commands, by name. It is a map of
// its own, as a field on command would make an initialization cycle.
var runners = map[string]func(context.Context, []string) error{
	"org":       orgCmd,
	"project":   projectCmd,
	"user":      userCmd,
	"key":       keyCmd,
	"model":     modelCmd,
	"filter":    filterCmd,
	"router":    routerCmd,
	"mcp":       mcpCmd,
	"guardrail": guardrailCmd,
	// Each sets one part of a guardrail, like 'guardrail set' with fewer flags.
	"limit":     func(ctx context.Context, args []string) error { return facetCmd(ctx, "limit", args) },
	"budget":    func(ctx context.Context, args []string) error { return facetCmd(ctx, "budget", args) },
	"usage":     usageCmd,
	"billing":   billingCmd,
	"failures":  failuresCmd,
	"audit":     auditCmd,
	"session":   sessionCmd,
	"connect":   connectCmd,
	"agent":     agentCmd,
	"task":      taskCmd,
	"schedule":  scheduleCmd,
	"workspace": workspaceCmd,
	"doctor":    doctorCmd,
	"login":     loginCmd,
	"logout":    logoutCmd,
	"whoami":    whoamiCmd,
	"version":   func(_ context.Context, args []string) error { return versionCmd(args) },
}

func versionCmd(args []string) error {
	if done, err := newCmdRun("version", args).parse(); done {
		return err
	}
	fmt.Println(version.String())
	return nil
}

// Fail prints a command's error.
func Fail(err error) {
	fmt.Fprintln(os.Stderr, styleErr.bad("keera:"), err)
}

func takeURL(args []string) ([]string, error) {
	return takeGlobal(args, "url", func(v string) error {
		if v == "" {
			return errors.New("--url needs an address, such as --url https://keera.example.ch")
		}
		urlFlag = v
		if !strings.Contains(v, "://") {
			// People type a bare host; deployments are served over https.
			urlFlag = "https://" + v
		}
		return nil
	})
}

// takeGlobal takes a global flag out of args and hands each value to set. It
// stops at "--", since the rest is positional.
func takeGlobal(args []string, name string, set func(string) error) ([]string, error) {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		value := ""
		switch {
		case arg == "--":
			return append(out, args[i:]...), nil
		case arg == "--"+name || arg == "-"+name:
			if i+1 < len(args) {
				i++
				value = args[i]
			}
		case strings.HasPrefix(arg, "--"+name+"=") || strings.HasPrefix(arg, "-"+name+"="):
			_, value, _ = strings.Cut(arg, "=")
		default:
			out = append(out, arg)
			continue
		}
		if err := set(value); err != nil {
			return nil, err
		}
	}
	return out, nil
}
