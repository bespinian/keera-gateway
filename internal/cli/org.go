package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// orgRun is one 'keera org' invocation.
type orgRun struct {
	*cmdRun
	name      string
	domain    string
	noDomain  bool
	liftLimit bool
	all       bool
}

func orgCmd(ctx context.Context, args []string) error {
	r := &orgRun{cmdRun: newCmdRun("org", args)}
	fs := r.fs
	fs.StringVar(&r.name, "name", "", "the organisation's new name")
	fs.StringVar(&r.domain, "domain", "",
		"email domain whose sign-ins land in this organisation, such as example.ch")
	fs.BoolVar(&r.noDomain, "no-domain", false, "remove the organisation's email domain")
	fs.BoolVar(&r.liftLimit, "lift-limit", false,
		"open the models and MCP servers inside this deployment's network, and sandboxes, "+
			"to an organisation that signed up; without card payments, also the models on "+
			"the deployment's own provider keys")
	fs.BoolVar(&r.all, "all", false,
		"act on every organisation again, rather than on one (operators)")
	if done, err := r.parse(); done {
		return err
	}
	switch r.verb {
	case "create":
		return r.create(ctx)
	case "set":
		return r.set(ctx)
	case "delete":
		return r.delete(ctx)
	case "switch":
		return r.switchTo(ctx)
	default:
		return r.list(ctx)
	}
}

func (r *orgRun) create(ctx context.Context) error {
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
		orgRow(w, org)
	}); err != nil {
		return err
	}
	warnAboutDomains(ctx, r.c)
	return nil
}

func (r *orgRun) set(ctx context.Context) error {
	if !changesSomething(r.fs) {
		return nothingToChange("org set")
	}
	r.name = strings.TrimSpace(r.name)
	if given(r.fs, "name") && r.name == "" {
		return errors.New("--name is empty; an organisation needs a name")
	}
	if r.domain != "" && r.noDomain {
		return opposites("domain", "no-domain")
	}
	// The organisation may be left off while there is only one, which is
	// exactly when the domain has to be set.
	var orgID string
	var err error
	if r.fs.Arg(0) == "" {
		orgID, err = theOnlyOrg(ctx, r.c, "name the one to change: keera org set <org>")
	} else {
		orgID, err = resolveOrg(ctx, r.c, r.fs.Arg(0))
	}
	if err != nil {
		return err
	}
	change := map[string]any{}
	if r.liftLimit {
		change["limited"] = false
	}
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
	path := "/v1/orgs/" + url.PathEscape(orgID)
	return doOut(ctx, r.c, "PATCH", path, change, r.asJSON, func(w *table, org store.Org) {
		orgRow(w, org)
		if _, ok := change["email_domain"]; ok {
			// Only a first sign-in reads the domain, so nobody already here moves.
			_, _ = fmt.Fprintln(w, "\nThis decides where a first sign-in lands. "+
				"Everyone already here keeps the organisation they are in.")
		}
		if r.liftLimit {
			_, _ = fmt.Fprintln(w, "\nThe models and MCP servers inside this deployment's "+
				"network, and sandboxes, are open to it now.")
		}
	})
}

func (r *orgRun) list(ctx context.Context) error {
	me, err := whoami(ctx, r.c)
	if err != nil {
		return err
	}
	if !me.Unrestricted {
		return r.listMine(me)
	}
	orgs, err := list[store.Org](ctx, r.c, "/v1/orgs")
	if err != nil {
		return err
	}
	// The domain is a column because a missing one is what an operator must
	// notice: with two organisations or more, nobody new can sign in to it.
	return out(r.asJSON, orgs, func(w *table) {
		if len(orgs) == 0 {
			printNone(w, "organisations", "keera org create <name>")
			return
		}
		w.header("ID\tNAME\tEMAIL DOMAIN\tCREATED")
		for _, o := range orgs {
			name := o.Name
			if o.Limited {
				name += " " + style.warn("(limited)")
			}
			if o.ID == r.c.org {
				name += " " + style.ok("(active)")
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
				o.ID, name, orNotSet(o.EmailDomain), o.CreatedAt.Format(time.DateOnly))
		}
	})
}

// listMine lists the organisations a person is in, and the ones that
// invited them.
func (r *orgRun) listMine(me identity) error {
	orgs := myOrgs(me)
	return out(r.asJSON, orgs, func(w *table) {
		w.header("ID\tNAME\tROLE")
		for _, o := range orgs {
			name, role := o.Name, o.Role
			if o.ID == me.OrgID {
				name += " " + style.ok("(active)")
			}
			if o.Invited {
				role += " " + style.muted("(invitation)")
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", o.ID, name, role)
		}
	})
}

// myOrgs is every organisation a person can switch to. /v1/me lists them only
// when there are several.
func myOrgs(me identity) []membership {
	if len(me.Orgs) > 0 {
		return me.Orgs
	}
	if me.OrgID == "" {
		return nil
	}
	return []membership{{ID: me.OrgID, Name: me.OrgName, Role: me.Role}}
}

// switchTo chooses the organisation later commands act on. A person in
// several moves their sign-in there, because their role differs in each. An
// operator sees every one, so the choice is only remembered here.
func (r *orgRun) switchTo(ctx context.Context) error {
	given := r.fs.Arg(0)
	if given == "" && !r.all {
		return errors.New("usage: keera org switch <org>; 'keera org list' shows them")
	}
	me, err := whoami(ctx, r.c)
	if err != nil {
		return err
	}
	if r.all {
		if !me.Unrestricted {
			return errors.New("only an operator acts on every organisation at once")
		}
		if err := saveOrg(r.c.base, ""); err != nil {
			return err
		}
		fmt.Println("Commands now act on every organisation, and ask for one where they need it.")
		r.warnAboutEnv()
		return nil
	}

	if me.Unrestricted {
		orgs, err := list[store.Org](ctx, r.c, "/v1/orgs")
		if err != nil {
			return err
		}
		org, ok := findIn(orgs, given, func(o store.Org) (string, string) { return o.ID, o.Name })
		if !ok {
			return notFound("organisation", given, "", "org")
		}
		if err := saveOrg(r.c.base, org.ID); err != nil {
			return err
		}
		fmt.Printf("Commands now act on %s.\n", org.Name)
		r.warnAboutEnv()
		return nil
	}

	mine := myOrgs(me)
	m, ok := findIn(mine, given, func(o membership) (string, string) { return o.ID, o.Name })
	if !ok {
		return notFound("organisation", given, "", "org")
	}
	var res struct {
		OrgID   string `json:"org_id"`
		OrgName string `json:"org_name"`
		Role    string `json:"role"`
	}
	if err := r.c.do(ctx, "POST", "/v1/me/org", map[string]string{"org_id": m.ID}, &res); err != nil {
		return err
	}
	if err := saveOrg(r.c.base, res.OrgID); err != nil {
		return err
	}
	if m.Invited {
		fmt.Printf("Joined %s as %s.\n", res.OrgName, res.Role)
	} else {
		fmt.Printf("Now in %s as %s.\n", res.OrgName, res.Role)
	}
	r.warnAboutEnv()
	return nil
}

// warnAboutEnv says when KEERA_ORG overrides the choice just made.
func (r *orgRun) warnAboutEnv() {
	if env("KEERA_ORG") != "" {
		fmt.Fprintln(os.Stderr, styleErr.warn("\nNote: KEERA_ORG is set in this shell, and it "+
			"wins. Unset it to use this choice."))
	}
}

func (r *orgRun) delete(ctx context.Context) error {
	orgID, err := resolveOrg(ctx, r.c, r.fs.Arg(0))
	if err != nil {
		return err
	}
	if !r.yes {
		if err := confirmOrgDelete(ctx, r.c, orgID); err != nil {
			return err
		}
	}
	var gone deletedOrg
	raw, err := keepRaw(ctx, r.c, "DELETE", "/v1/orgs/"+url.PathEscape(orgID), nil, &gone)
	if err != nil {
		return err
	}
	if r.c.org == gone.ID && r.c.orgFrom == "keera org switch" {
		if err := saveOrg(r.c.base, ""); err != nil {
			return err
		}
	}
	return out(r.asJSON, raw, func(w *table) {
		_, _ = fmt.Fprintf(w, "deleted %s (%s)\t%s, %s, %s\n", gone.ID, gone.Name,
			plural(gone.Projects, "project"), plural(gone.Users, "user"), plural(gone.Keys, "key"))
	})
}

// warnAboutDomains warns when there are several organisations and some have
// no email domain: from the second one on, a first sign-in is placed by domain
// or refused, so a new tenant can stop sign-ins to the first. It writes to
// stderr so --json stays clean, and fails nothing: the organisation exists.
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

// orgRow writes the row that create and set answer with.
func orgRow(w *table, o store.Org) {
	_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", o.ID, o.Name, orNotSet(o.EmailDomain))
}

// orNotSet renders an empty email domain as something a table can show.
func orNotSet(domain string) string { return cmp.Or(domain, "not set") }

// deletedOrg is what DELETE /v1/orgs/{id} reports it removed, as far as the
// table shows it.
type deletedOrg struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Projects int    `json:"projects"`
	Users    int    `json:"users"`
	Keys     int    `json:"keys"`
}

// confirmOrgDelete prints what the deletion destroys and makes the operator
// type the organisation's name back. An id is easy to paste from the wrong
// row; a name is not.
func confirmOrgDelete(ctx context.Context, c *client, orgID string) error {
	orgs, err := list[store.Org](ctx, c, "/v1/orgs")
	if err != nil {
		return err
	}
	org, ok := findIn(orgs, orgID, func(o store.Org) (string, string) { return o.ID, o.Name })
	if !ok {
		return notFound("organisation", orgID, "", "org")
	}

	// Extra reads, only when asking, so the prompt can say how much goes.
	projects, err := list[store.Project](ctx, c, inOrg("/v1/projects", orgID))
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
			"(keera org switch %s, then keera sandbox list)", org.Name,
			plural(len(live), "live sandbox", "live sandboxes"), orgID)
	}

	return confirm(fmt.Sprintf("Deleting %s (%s) removes:", org.Name, org.ID), []string{
		"  " + plural(len(projects), "project"),
		"  " + plural(len(users), "user") + ", signed out everywhere",
		"  " + plural(len(keys), "API key") + ", which stop working at once",
		"  its guardrails and its budget counters",
		"  its models and their stored provider keys, MCP servers, filters, routers,",
		"  sandbox classes and the record of its finished sandboxes",
		"Usage history and the audit log are kept.",
		styleErr.bad("This cannot be undone."),
	}, "organisation's name", org.Name, "nothing was deleted")
}
