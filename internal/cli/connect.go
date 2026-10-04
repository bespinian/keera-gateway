package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/bespinian/keera-gateway/internal/connect"
	"github.com/bespinian/keera-gateway/internal/policy"
)

// connectCmd is the panel's "Connect a client" screen, in a terminal.
//
// The configuration blocks come from the control plane rather than from this
// binary, so an older CLI cannot hand out a configuration the deployment has
// moved on from.
func connectCmd(ctx context.Context, args []string) error {
	c := newClient()
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	model := fs.String("model", "",
		"the model or router to configure (default: the first enabled chat model)")
	org := fs.String("org", "", orgUsage)
	asJSON := fs.Bool("json", false, jsonUsage)
	subscription := fs.Bool("subscription", false,
		"for Claude Code signed in to a Claude plan: issue this machine its own key and "+
			"write it into ~/.claude/settings.json")
	team := fs.String("team", "", "with --subscription, the team the new key belongs to "+
		"(administrators only)")
	fs.Usage = func() { _ = printHelp(fs, "connect", "") }
	if want, ok := wantsHelp(args); ok {
		return printHelp(fs, "connect", want)
	}
	if err := parseCmd(fs, "connect", args); err != nil {
		return err
	}
	// No client, or only a flag, lands on the listing.
	sub := fs.Arg(0)

	var cat struct {
		Data       []connect.Client `json:"data"`
		GatewayURL string           `json:"gateway_url"`
	}
	if err := c.do(ctx, "GET", "/v1/connect", nil, &cat); err != nil {
		return err
	}
	if *subscription {
		if sub != "claude-code" {
			return errors.New("--subscription is for Claude Code: keera connect claude-code --subscription")
		}
		if cat.GatewayURL == "" {
			return errNoGatewayURL
		}
		return connectSubscription(ctx, c, subscriptionSetup{
			org: *org, team: *team, model: *model, gatewayURL: cat.GatewayURL, asJSON: *asJSON,
		})
	}
	if *team != "" {
		return errors.New("--team is for --subscription, which issues a key")
	}
	listing := sub == "list" || sub == ""
	chat, err := connectModels(ctx, c, listing, *model, *org)
	if err != nil {
		return err
	}

	// No client named: list the choices rather than pick one.
	if listing {
		return out(*asJSON, cat, func(w *table) {
			listConnect(w, cat.Data, chat, cat.GatewayURL)
		})
	}

	client, found := connect.Find(cat.Data, sub)
	if !found {
		keys := make([]string, 0, len(cat.Data))
		for _, cl := range cat.Data {
			keys = append(keys, cl.Key)
		}
		return fmt.Errorf("no client %q; this deployment can configure: %s",
			sub, strings.Join(keys, ", "))
	}
	chosen, err := chooseConnectModel(chat, *model)
	if err != nil {
		return err
	}
	alias := chosen.Alias

	// The deployment says where clients reach it. A client pointed anywhere
	// else would get a 401 that looks like a bad key.
	gatewayURL := cat.GatewayURL
	if gatewayURL == "" {
		return errNoGatewayURL
	}

	if *asJSON {
		return out(true, map[string]any{
			"client": client.Key, "model": alias, "url": gatewayURL, "path": client.Path,
			"config": client.Render(gatewayURL, alias, chosen.MaxContext),
			"run":    client.RunText(alias),
			"note":   client.Note,
		}, nil)
	}
	printConnect(client, chosen, gatewayURL)
	return nil
}

// errNoGatewayURL is a deployment that does not say where clients reach it.
var errNoGatewayURL = errors.New("this deployment does not say where a client reaches the " +
	"inference plane; set KEERA_PUBLIC_URL on the gateway")

// connectModels is what a client may be pointed at: the enabled chat models,
// and the organisation's routers, which a client names in the same field.
// Subscription models are left out: only a subscription key reaches them, and
// --subscription sets those up.
//
// Routers cost a second round trip, so they are read only for the listing or
// for a --model that is not a model. Failing to read them is not an error:
// the models are still worth offering.
func connectModels(ctx context.Context, c *client, listing bool, model, org string,
) ([]policy.Model, error) {
	org, err := resolveOrg(ctx, c, org)
	if err != nil {
		return nil, err
	}
	models, err := catalogue(ctx, c, org)
	if err != nil {
		return nil, err
	}
	chat := make([]policy.Model, 0, len(models))
	for _, m := range models {
		if m.Enabled && m.Kind == policy.KindChat && !m.Subscription {
			chat = append(chat, m)
		}
	}
	wantRouters := listing
	if model != "" {
		if _, found := findModel(chat, model); !found {
			wantRouters = true
		}
	}
	if wantRouters {
		if routers, err := connectRouters(ctx, c, org); err == nil {
			chat = append(chat, routers...)
		}
	}
	return chat, nil
}

// chooseConnectModel picks the named model, or the first one. The whole model
// is returned because its context window goes into the configuration.
func chooseConnectModel(chat []policy.Model, model string) (policy.Model, error) {
	if len(chat) == 0 {
		return policy.Model{}, fmt.Errorf("a coding agent needs a chat model and this organisation has no " +
			"enabled one; add one with: keera model add <alias> --backend <url> " +
			"--backend-model <name>")
	}
	if model == "" {
		return chat[0], nil
	}
	named, found := findModel(chat, model)
	if !found {
		return policy.Model{}, fmt.Errorf("no enabled chat model or router %s; this organisation "+
			"serves: %s", model, strings.Join(aliasesOf(chat), ", "))
	}
	return named, nil
}

func aliasesOf(models []policy.Model) []string {
	names := make([]string, 0, len(models))
	for _, m := range models {
		names = append(names, m.Alias)
	}
	return names
}

func listConnect(w *table, clients []connect.Client,
	chat []policy.Model, gateway string,
) {
	w.header("CLIENT\tGOES IN")
	for _, c := range clients {
		where := c.Path
		if where == "" {
			where = "(environment variables)"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\n", c.Key, where)
	}
	if gateway != "" {
		_, _ = fmt.Fprintf(w, "\nThis gateway is at %s.\n", gateway)
	}
	if len(chat) == 0 {
		_, _ = fmt.Fprintln(w, "\nNo enabled chat model exists yet, so there is nothing "+
			"to point a coding agent at.")
	} else {
		_, _ = fmt.Fprintf(w, "Chat models and routers: %s\n", strings.Join(aliasesOf(chat), ", "))
	}
	if len(clients) > 0 {
		_, _ = fmt.Fprintf(w, "\nThe configuration for one of them:\n  keera connect %s",
			clients[0].Key)
		if len(chat) > 0 {
			_, _ = fmt.Fprintf(w, " --model %s", chat[0].Alias)
		}
		_, _ = fmt.Fprintln(w)
	}
}

// printConnect writes the three steps in order. Only the configuration block
// goes to stdout, so it can be redirected into its file.
func printConnect(c connect.Client, m policy.Model, base string) {
	alias := m.Alias
	fmt.Fprintf(os.Stderr, "%s · model %s · %s\n\n", styleErr.head(c.Label), alias, base)

	fmt.Fprintln(os.Stderr, styleErr.head("1.")+" The key, which your client reads from the "+
		"environment:")
	fmt.Fprintln(os.Stderr, "     "+styleErr.cmd("export KEERA_API_KEY=keera_sk_…"))
	fmt.Fprintln(os.Stderr, "   Issue one with 'keera key create --user <email> --alias <what for>',")
	fmt.Fprintln(os.Stderr, "   or ask whoever administers this deployment for one.")

	if c.Path != "" {
		fmt.Fprintf(os.Stderr, "\n%s This goes in %s. If that file exists already, merge\n",
			styleErr.head("2."), c.Path)
		fmt.Fprintln(os.Stderr, "   these keys into it rather than replacing it:")
	} else {
		fmt.Fprintln(os.Stderr, "\n"+styleErr.head("2.")+" These go in your shell profile, so "+
			"every client on the machine")
		fmt.Fprintln(os.Stderr, "   picks them up:")
	}
	fmt.Fprintln(os.Stderr)
	fmt.Println(c.Render(base, alias, m.MaxContext))
	if c.Note != "" {
		fmt.Fprintf(os.Stderr, "\n   %s\n", styleErr.muted(wrapAt(c.NoteText(), 0, 3, 76)))
	}

	fmt.Fprintf(os.Stderr, "\n%s %s\n", styleErr.head("3."), wrapAt(c.RunText(alias), 0, 3, 76))
}

// connectRouters reads the organisation's routers as model entries with no
// backend and no context window, which is what a router is to a client. Which
// model answers is decided per request, so no context window is right.
func connectRouters(ctx context.Context, c *client, orgID string) ([]policy.Model, error) {
	routers, err := list[policy.Router](ctx, c, inOrg("/v1/routers", orgID))
	if err != nil {
		return nil, err
	}
	out := make([]policy.Model, 0, len(routers))
	for _, rt := range routers {
		out = append(out, policy.Model{
			Alias: rt.Alias, Kind: policy.KindChat, Enabled: true,
			Description: rt.Description,
		})
	}
	return out, nil
}
