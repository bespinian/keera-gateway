package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/bespinian/keera-gateway/internal/connect"
	"github.com/bespinian/keera-gateway/internal/policy"
)

// connectCmd is the panel's "Connect client" dialog, in a terminal.
//
// The configuration blocks come from the control plane rather than from this
// binary, so an older CLI cannot hand out a configuration the deployment has
// moved on from.
func connectCmd(ctx context.Context, args []string) error {
	c := newClient()
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	key := fs.String("key", "",
		"the API key whose models to configure, by name or id (default: your first active key)")
	model := fs.String("model", "", "with --subscription, the model Claude Code starts with "+
		"(default: the first subscription model)")
	org := fs.String("org", "", orgUsage)
	asJSON := fs.Bool("json", false, jsonUsage)
	subscription := fs.Bool("subscription", false,
		"for Claude Code signed in to a Claude plan: give this machine its own key and "+
			"write it into ~/.claude/settings.json")
	project := fs.String("project", "", "with --subscription, the project the new key belongs to "+
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
		if *key != "" {
			return errors.New("--key is not for --subscription, which issues its own key")
		}
		if cat.GatewayURL == "" {
			return errNoGatewayURL
		}
		return connectSubscription(ctx, c, subscriptionSetup{
			org: *org, project: *project, model: *model, gatewayURL: cat.GatewayURL, asJSON: *asJSON,
		})
	}
	if *project != "" {
		return errors.New("--project is for --subscription, which issues a key")
	}
	if *model != "" {
		return errors.New("--model is for --subscription; a configuration lists every model " +
			"its key may use, so choose the key with --key instead")
	}
	orgID, err := resolveOrg(ctx, c, *org)
	if err != nil {
		return err
	}
	chat, err := connectModels(ctx, c, orgID)
	if err != nil {
		return err
	}

	// No client named: list the choices rather than pick one.
	if sub == "list" || sub == "" {
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
	if len(chat) == 0 {
		return errNoChatModel
	}
	own, err := connectKey(ctx, c, orgID, *key)
	if err != nil {
		return err
	}
	models := chat
	if own != nil {
		models = keyModels(chat, own.AllowedModels)
		if len(models) == 0 {
			return fmt.Errorf("the key %s may use no chat model; an administrator can allow "+
				"one in its guardrails", own.Name)
		}
	}

	// The deployment says where clients reach it. A client pointed anywhere
	// else would get a 401 that looks like a bad key.
	gatewayURL := cat.GatewayURL
	if gatewayURL == "" {
		return errNoGatewayURL
	}

	if *asJSON {
		res := map[string]any{
			"client": client.Key, "models": aliasesOf(models), "url": gatewayURL,
			"path":   client.Path,
			"config": client.Render(gatewayURL, connectList(models)),
			"run":    client.RunText(connectList(models)),
			"note":   client.Note,
		}
		if own != nil {
			res["key"] = own.Name
		}
		return out(true, res, nil)
	}
	printConnect(client, own, models, gatewayURL)
	return nil
}

// errNoGatewayURL is a deployment that does not say where clients reach it.
var errNoGatewayURL = errors.New("this deployment does not say where a client reaches the " +
	"inference plane; set KEERA_PUBLIC_URL on the gateway")

var errNoChatModel = errors.New("a coding agent needs a chat model and this organisation has " +
	"no enabled one; add one with: keera model add <alias> --backend <url> " +
	"--backend-model <name>")

// connectModels is what a client may be pointed at: the enabled chat models,
// and the organisation's routers, which a client names in the same field.
// Subscription models are left out: only a subscription key reaches them, and
// --subscription sets those up.
//
// Failing to read the routers is not an error: the models are still worth
// offering.
func connectModels(ctx context.Context, c *client, org string) ([]policy.Model, error) {
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
	if routers, err := connectRouters(ctx, c, org); err == nil {
		chat = append(chat, routers...)
	}
	return chat, nil
}

// ownKey is one of the caller's keys, as /v1/access describes it.
type ownKey struct {
	ID     string         `json:"id"`
	OrgID  string         `json:"org_id"`
	Name   string         `json:"name"`
	Prefix string         `json:"prefix"`
	Kind   policy.KeyKind `json:"kind"`
	State  string         `json:"state"`
	// AllowedModels is every model the key may call, already resolved.
	AllowedModels []string `json:"allowed_models"`
}

// connectKey picks the caller's key whose models go into the configuration:
// the one named, or else their first active one. Somebody with no key of their
// own, such as the operator key, gets none, and every model is configured.
//
// Only the caller's own keys are offered: the configuration is for their
// machine, and a subscription key works only through --subscription.
func connectKey(ctx context.Context, c *client, org, named string) (*ownKey, error) {
	var access struct {
		Keys []ownKey `json:"keys"`
	}
	if err := c.do(ctx, "GET", "/v1/access", nil, &access); err != nil {
		return nil, err
	}
	var usable []ownKey
	for _, k := range access.Keys {
		if k.State == "active" && k.Kind != policy.KeySubscription && k.OrgID == org {
			usable = append(usable, k)
		}
	}
	if named == "" {
		if len(usable) == 0 {
			return nil, nil
		}
		return &usable[0], nil
	}
	names := make([]string, 0, len(usable))
	for i, k := range usable {
		if k.ID == named || k.Name == named {
			return &usable[i], nil
		}
		names = append(names, k.Name)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("you have no active key %s, and no other either; "+
			"ask an administrator to issue one in your name", named)
	}
	return nil, fmt.Errorf("you have no active key %s; yours are: %s",
		named, strings.Join(names, ", "))
}

// keyModels is the chat models and routers a key's allow-list names, in the
// organisation's order.
func keyModels(chat []policy.Model, allowed []string) []policy.Model {
	out := make([]policy.Model, 0, len(chat))
	for _, m := range chat {
		if slices.Contains(allowed, m.Alias) {
			out = append(out, m)
		}
	}
	return out
}

// connectList is what the catalogue renders a configuration from.
func connectList(models []policy.Model) []connect.Model {
	out := make([]connect.Model, 0, len(models))
	for _, m := range models {
		out = append(out, connect.Model{Alias: m.Alias, MaxContext: m.MaxContext})
	}
	return out
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
		_, _ = fmt.Fprintf(w, "\nThe configuration for one of them, with every model your "+
			"key may use:\n  keera connect %s\n", clients[0].Key)
	}
}

// printConnect writes the three steps in order. Only the configuration block
// goes to stdout, so it can be redirected into its file.
func printConnect(c connect.Client, own *ownKey, models []policy.Model, base string) {
	which := "every model"
	if own != nil {
		which = "key " + own.Name
	}
	fmt.Fprintf(os.Stderr, "%s · %s · %s\n\n", styleErr.head(c.Label), which, base)

	fmt.Fprintln(os.Stderr, styleErr.head("1.")+" The key, which your client reads from the "+
		"environment:")
	if own != nil {
		fmt.Fprintln(os.Stderr, "     "+styleErr.cmd("export KEERA_API_KEY="+own.Prefix+"…"))
		fmt.Fprintf(os.Stderr, "   Use the key %s. It was shown once, when it was issued.\n",
			own.Name)
	} else {
		fmt.Fprintln(os.Stderr, "     "+styleErr.cmd("export KEERA_API_KEY=keera_sk_…"))
		fmt.Fprintln(os.Stderr, "   Issue one with 'keera key create --user <email> --name <what for>',")
		fmt.Fprintln(os.Stderr, "   or ask whoever administers this deployment for one.")
	}

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
	fmt.Println(c.Render(base, connectList(models)))
	if c.Note != "" {
		fmt.Fprintf(os.Stderr, "\n   %s\n", styleErr.muted(wrapAt(c.NoteText(), 0, 3, 76)))
	}

	fmt.Fprintf(os.Stderr, "\n%s %s\n", styleErr.head("3."),
		wrapAt(c.RunText(connectList(models)), 0, 3, 76))
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
