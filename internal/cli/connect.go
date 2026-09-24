package cli

import (
	"context"
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
	sub, rest := split(args)
	c := newClient()
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	model := fs.String("model", "",
		"the model or router to configure (default: the first enabled chat model)")
	// Only needed for routers, which belong to an organisation.
	org := fs.String("org", "", "organisation whose routers to offer (defaults to the only one)")
	asJSON := fs.Bool("json", false, jsonUsage)
	fs.Usage = func() { _ = printHelp(fs, "connect", "") }
	if want, ok := wantsHelp(args); ok {
		return printHelp(fs, "connect", want)
	}
	// Here split() has taken the client's name, not a verb. No client, or only
	// a flag, lands on the listing.
	if err := parse(fs, rest); err != nil {
		return err
	}

	var cat struct {
		Data       []connect.Client `json:"data"`
		GatewayURL string           `json:"gateway_url"`
	}
	if err := c.do(ctx, "GET", "/v1/connect", nil, &cat); err != nil {
		return err
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

	client, found := lookupClient(cat.Data, sub)
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
		return fmt.Errorf("this deployment does not say where a client reaches the " +
			"inference plane; set KEERA_PUBLIC_URL on the gateway")
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

// connectModels is what a client may be pointed at: the enabled chat models,
// and the organisation's routers, which a client names in the same field.
//
// Routers cost a second round trip, so they are read only for the listing or
// for a --model that is not a model. Failing to read them is not an error: an
// operator with several organisations still gets the models.
func connectModels(ctx context.Context, c *client, listing bool, model, org string,
) ([]policy.Model, error) {
	models, err := catalogue(ctx, c)
	if err != nil {
		return nil, err
	}
	chat := make([]policy.Model, 0, len(models))
	for _, m := range models {
		if m.Enabled && m.Kind == policy.KindChat {
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
		return policy.Model{}, fmt.Errorf("a coding agent needs a chat model and the catalogue has none " +
			"that is enabled; add one with: keera model add <alias> --backend <url> " +
			"--backend-model <name>")
	}
	if model == "" {
		return chat[0], nil
	}
	named, found := findModel(chat, model)
	if !found {
		return policy.Model{}, fmt.Errorf("no enabled chat model or router %s; this deployment "+
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

func lookupClient(clients []connect.Client, key string) (connect.Client, bool) {
	for _, c := range clients {
		if c.Key == key {
			return c, true
		}
	}
	return connect.Client{}, false
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
		fmt.Fprintf(os.Stderr, "\n   %s\n", styleErr.muted(wrap(c.Note, 76, "   ")))
	}

	fmt.Fprintf(os.Stderr, "\n%s %s\n", styleErr.head("3."), wrap(c.RunText(alias), 76, "   "))
}

// wrap breaks prose to width, indenting the lines it carries over. It counts
// runes, not bytes, because the prose holds em-dashes.
func wrap(text string, width int, indent string) string {
	var b strings.Builder
	column := 0
	for i, word := range strings.Fields(text) {
		n := len([]rune(word))
		switch {
		case i == 0:
			column = n
		case column+1+n > width:
			b.WriteString("\n" + indent)
			column = len([]rune(indent)) + n
		default:
			b.WriteString(" ")
			column += 1 + n
		}
		b.WriteString(word)
	}
	return b.String()
}

// connectRouters reads the organisation's routers as model entries with no
// backend and no context window, which is what a router is to a client. Which
// model answers is decided per request, so no context window is right.
func connectRouters(ctx context.Context, c *client, org string) ([]policy.Model, error) {
	orgID, err := resolveOrg(ctx, c, org)
	if err != nil {
		return nil, err
	}
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
