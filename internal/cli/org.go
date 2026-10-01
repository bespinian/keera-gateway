package cli

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// orgRun is one 'keera org' invocation: the client, the flags every verb
// shares, and the arguments after the verb.
type orgRun struct {
	c        *client
	fs       *flag.FlagSet
	args     []string
	name     string
	domain   string
	noDomain bool
	yes      bool
	asJSON   bool
}

func orgCmd(ctx context.Context, args []string) error {
	sub, rest := split(args)
	fs := flag.NewFlagSet("org "+sub, flag.ExitOnError)
	r := &orgRun{c: newClient(), fs: fs, args: rest}
	fs.StringVar(&r.name, "name", "", "the organisation's new name")
	fs.StringVar(&r.domain, "domain", "",
		"email domain whose sign-ins land in this organisation, such as example.ch")
	fs.BoolVar(&r.noDomain, "no-domain", false, "remove the organisation's email domain")
	fs.BoolVar(&r.yes, "yes", false, yesUsage)
	fs.BoolVar(&r.asJSON, "json", false, jsonUsage)

	fs.Usage = func() { _ = printHelp(fs, "org", sub) }
	if want, ok := wantsHelp(args); ok {
		return printHelp(fs, "org", want)
	}
	if err := parse(fs, rest); err != nil {
		return err
	}
	if err := verbFlags(fs, "org", sub); err != nil {
		return err
	}
	r.args = fs.Args()

	switch sub {
	case "create", "add", "new":
		return r.create(ctx)
	case "set", "edit", "update":
		return r.set(ctx)
	case "list", "ls", "":
		return r.list(ctx)
	case "delete", "rm", "remove":
		return r.delete(ctx)
	default:
		return unknownSub("org", sub)
	}
}

func (r *orgRun) create(ctx context.Context) error {
	if err := parseArgs(r.fs, r.args, 1, "usage: keera org create <name> [--domain <domain>]"); err != nil {
		return err
	}
	d, err := policy.CleanEmailDomain(r.domain)
	if err != nil {
		return err
	}
	var org store.Org
	if err := r.c.do(ctx, "POST", "/v1/orgs",
		map[string]string{"name": r.fs.Arg(0), "email_domain": d}, &org); err != nil {
		return err
	}
	if err := out(r.asJSON, org, func(w *table) {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", org.ID, org.Name, orNotSet(org.EmailDomain))
	}); err != nil {
		return err
	}
	warnAboutDomains(ctx, r.c)
	return nil
}

func (r *orgRun) set(ctx context.Context) error {
	if err := parse(r.fs, r.args); err != nil {
		return err
	}
	if r.fs.NArg() > 1 {
		return fmt.Errorf("usage: keera org set [<org-id>] [--name <name>] [--domain <domain> | --no-domain]")
	}
	r.name = strings.TrimSpace(r.name)
	if r.name == "" && r.domain == "" && !r.noDomain {
		return fmt.Errorf("nothing to change; pass --name <name>, --domain <domain> or --no-domain")
	}
	if r.domain != "" && r.noDomain {
		return fmt.Errorf("--domain and --no-domain say opposite things; pass one of them")
	}
	// The id may be left off while there is only one organisation, which is
	// exactly when the domain has to be set.
	orgID := r.fs.Arg(0)
	if orgID == "" {
		only, err := theOnlyOrg(ctx, r.c, "name the one to change: keera org set <org-id>")
		if err != nil {
			return err
		}
		orgID = only
	}
	change := map[string]string{}
	if r.name != "" {
		change["name"] = r.name
	}
	if r.domain != "" || r.noDomain {
		d, err := policy.CleanEmailDomain(r.domain)
		if err != nil {
			return err
		}
		change["email_domain"] = d
	}
	var org store.Org
	if err := r.c.do(ctx, "PATCH", "/v1/orgs/"+url.PathEscape(orgID), change, &org); err != nil {
		return err
	}
	return out(r.asJSON, org, func(w *table) {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", org.ID, org.Name, orNotSet(org.EmailDomain))
		if _, ok := change["email_domain"]; ok {
			// Only a first sign-in reads the domain, so nobody already here moves.
			_, _ = fmt.Fprintln(w, "\nThis decides where a first sign-in lands. "+
				"Everyone already here keeps the organisation they are in.")
		}
	})
}

func (r *orgRun) list(ctx context.Context) error {
	if err := parse(r.fs, r.args); err != nil {
		return err
	}
	orgs, err := list[store.Org](ctx, r.c, "/v1/orgs")
	if err != nil {
		return err
	}
	// The domain is a column because a missing one is what an operator must
	// notice: with two organisations or more, nobody new can sign in to it.
	return out(r.asJSON, orgs, func(w *table) {
		w.header("ID\tNAME\tEMAIL DOMAIN\tCREATED")
		for _, o := range orgs {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
				o.ID, o.Name, orNotSet(o.EmailDomain), o.CreatedAt.Format(time.DateOnly))
		}
	})
}

func (r *orgRun) delete(ctx context.Context) error {
	if err := parseArgs(r.fs, r.args, 1, "usage: keera org delete <org-id> [--yes]"); err != nil {
		return err
	}
	orgID := r.fs.Arg(0)
	if !r.yes {
		if err := confirmOrgDelete(ctx, r.c, orgID); err != nil {
			return err
		}
	}
	var gone deletedOrg
	if err := r.c.do(ctx, "DELETE", "/v1/orgs/"+url.PathEscape(orgID), nil, &gone); err != nil {
		return err
	}
	return out(r.asJSON, gone, func(w *table) {
		_, _ = fmt.Fprintf(w, "deleted %s (%s)\t%s, %s, %s\n", gone.ID, gone.Name,
			plural(gone.Teams, "team"), plural(gone.Users, "user"), plural(gone.Keys, "key"))
	})
}

// warnAboutDomains warns when the deployment now has several organisations
// and some have no email domain.
//
// With one organisation, every sign-in lands in it. From the second one on, a
// first sign-in is placed by domain or refused, so creating a second tenant
// can stop sign-ins to the first.
//
// It writes to stderr so --json stays clean, and fails nothing: the
// organisation already exists.
func warnAboutDomains(ctx context.Context, c *client) {
	orgs, err := list[store.Org](ctx, c, "/v1/orgs")
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nThe organisation was created. Reading the others back to "+
			"check their email domains failed: %v\n", err)
		return
	}
	if len(orgs) < 2 {
		return
	}
	var missing []store.Org
	for _, o := range orgs {
		if o.EmailDomain == "" {
			missing = append(missing, o)
		}
	}
	if len(missing) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "\nThis deployment now has %d organisations, so a first sign-in is "+
		"placed by\nemail domain - and an address matching none of them is refused rather "+
		"than\nput in one. These have no domain:\n", len(orgs))
	for _, o := range missing {
		fmt.Fprintf(os.Stderr, "  %s (%s)\n", o.Name, o.ID)
	}
	fmt.Fprintf(os.Stderr, "Set one with: keera org set %s --domain example.ch\n", missing[0].ID)
	fmt.Fprintln(os.Stderr, "Anyone who has signed in before keeps the organisation they are in.")
}

// orNotSet renders an empty email domain as something a table can show.
func orNotSet(domain string) string {
	if domain == "" {
		return "not set"
	}
	return domain
}

// deletedOrg is what DELETE /v1/orgs/{id} reports it removed.
type deletedOrg struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Teams int    `json:"teams"`
	Users int    `json:"users"`
	Keys  int    `json:"keys"`
}

// confirmOrgDelete prints what the deletion destroys and makes the operator
// type the organisation's name back. An id is easy to paste from the wrong
// row; a name is not.
func confirmOrgDelete(ctx context.Context, c *client, orgID string) error {
	orgs, err := list[store.Org](ctx, c, "/v1/orgs")
	if err != nil {
		return err
	}
	var org *store.Org
	for i := range orgs {
		if orgs[i].ID == orgID {
			org = &orgs[i]
			break
		}
	}
	if org == nil {
		return fmt.Errorf("no organisation %s (see: keera org list)", orgID)
	}

	// Three extra reads, only when asking, so the prompt can say how much goes.
	teams, err := list[store.Team](ctx, c, inOrg("/v1/teams", orgID))
	if err != nil {
		return err
	}
	users, err := list[store.User](ctx, c, inOrg("/v1/users", orgID))
	if err != nil {
		return err
	}
	keys, err := list[store.KeyInfo](ctx, c, inOrg("/v1/keys", orgID))
	if err != nil {
		return err
	}
	// The control plane refuses while sandboxes hold machines. Saying so now
	// spares typing the name for nothing. A failed read leaves it to the
	// control plane.
	if live, err := list[store.Sandbox](ctx, c, inOrg("/v1/sandboxes", orgID)); err == nil && len(live) > 0 {
		return fmt.Errorf("%s still has %s; terminate them first "+
			"(keera sandbox list --org %s)", org.Name, plural(len(live), "live sandbox", "live sandboxes"), orgID)
	}

	fmt.Fprintf(os.Stderr, "%s\n", styleErr.head(fmt.Sprintf("Deleting %s (%s) removes:", org.Name, org.ID)))
	fmt.Fprintf(os.Stderr, "  %s\n  %s, signed out everywhere\n  %s, which stop working at once\n",
		plural(len(teams), "team"), plural(len(users), "user"), plural(len(keys), "API key"))
	fmt.Fprintln(os.Stderr, "  its guardrails and its budget counters")
	fmt.Fprintln(os.Stderr, "  its models and their stored provider keys, MCP servers, filters, routers,")
	fmt.Fprintln(os.Stderr, "  sandbox classes and the record of its finished sandboxes")
	fmt.Fprintln(os.Stderr, "Usage history and the audit log are kept.")
	fmt.Fprintln(os.Stderr, styleErr.bad("This cannot be undone."))
	return confirmTyping("organisation's name", org.Name, "nothing was deleted")
}
