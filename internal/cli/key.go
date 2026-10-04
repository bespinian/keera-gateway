package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/id"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// keyRun is one 'keera key' invocation.
type keyRun struct {
	c       *client
	fs      *flag.FlagSet
	org     string
	project string
	user    string
	name    string
	expires string
	// subscription issues a key for Claude Code signed in to a Claude plan.
	subscription bool
	yes          bool
	asJSON       bool
}

// createdKey is a new key as the control API returns it, secret included.
type createdKey struct {
	store.KeyInfo
	Key string `json:"key"`
}

func keyCmd(ctx context.Context, args []string) error {
	sub, rest := split(args)
	fs := flag.NewFlagSet("key "+sub, flag.ExitOnError)
	r := &keyRun{c: newClient(), fs: fs}
	fs.StringVar(&r.org, "org", "", orgUsage)
	fs.StringVar(&r.project, "project", "", "project, by name or id; without it, the organisation's oldest project")
	fs.StringVar(&r.user, "user", "", "the person this key belongs to, by email or id")
	fs.StringVar(&r.name, "name", "", "what this key is called; what it is for, in a few words")
	fs.StringVar(&r.expires, "expires", "", "lifetime, e.g. 720h")
	fs.BoolVar(&r.subscription, "subscription", false,
		"a key that reaches only subscription models, for Claude Code signed in to a Claude plan")
	fs.BoolVar(&r.yes, "yes", false, yesUsage)
	fs.BoolVar(&r.asJSON, "json", false, jsonUsage)

	fs.Usage = func() { _ = printHelp(fs, "key", sub) }
	if want, ok := wantsHelp(args); ok {
		return printHelp(fs, "key", want)
	}
	verb, err := parseVerb(fs, "key", sub, rest)
	if err != nil {
		return err
	}
	switch verb {
	case "create":
		return r.create(ctx)
	case "revoke":
		return r.revoke(ctx)
	case "rotate":
		return r.rotate(ctx)
	case "set":
		return r.set(ctx)
	default:
		return r.list(ctx)
	}
}

func (r *keyRun) create(ctx context.Context) error {
	if r.fs.NArg() == 1 {
		if r.name != "" {
			return errors.New("give the name once: as the argument or with --name")
		}
		r.name = r.fs.Arg(0)
	}
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	project, err := projectID(ctx, r.c, orgID, r.project)
	if err != nil {
		return err
	}
	req := map[string]string{"org_id": orgID, "project_id": project, "name": r.name}
	// A key with a person on it is what makes `keera usage --by user` work.
	if r.user != "" {
		owner, err := findUser(ctx, r.c, orgID, r.user)
		if err != nil {
			return err
		}
		req["user_id"] = owner.ID
	}
	if r.expires != "" {
		req["expires_in"] = r.expires
	}
	if r.subscription {
		req["kind"] = string(policy.KeySubscription)
	}
	var created createdKey
	if err := r.c.do(ctx, "POST", "/v1/keys", req, &created); err != nil {
		return err
	}
	if r.asJSON {
		return out(true, created, nil)
	}
	fmt.Println(created.Key)
	fmt.Fprintf(os.Stderr, "\nkey %s created for %s. %s\n",
		created.ID, orgID, styleErr.warn("This is the only time it is shown."))
	return nil
}

func (r *keyRun) list(ctx context.Context) error {
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	project, err := projectID(ctx, r.c, orgID, r.project)
	if err != nil {
		return err
	}
	path := inOrg("/v1/keys", orgID)
	if project != "" {
		path += "&project_id=" + url.QueryEscape(project)
	}
	keys, err := list[store.KeySummary](ctx, r.c, path)
	if err != nil {
		return err
	}
	return out(r.asJSON, keys, func(w *table) {
		// Last use decides whether a key is safe to revoke, so it is a column.
		w.header("ID\tNAME\tPREFIX\tKIND\tPROJECT\tSTATE\tLAST USED\tCLAUDE PLAN USED")
		for _, k := range keys {
			last := "never"
			if k.LastUsedAt != nil {
				last = k.LastUsedAt.Format(time.DateOnly)
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s…\t%s\t%s\t%s\t%s\t%s\n",
				k.ID, k.Name, k.Prefix, dash(string(k.Kind)), dash(k.ProjectID),
				statusWord(k.State(time.Now())), statusWord(last), planText(k.Plan))
		}
	})
}

// planText is how much of a Claude plan a subscription key's holder has used,
// in each of the plan's two windows.
func planText(p *policy.PlanUsage) string {
	if p == nil {
		return "-"
	}
	var parts []string
	for _, w := range []struct {
		name string
		used *float64
	}{{"5h", p.FiveHour}, {"7d", p.SevenDay}} {
		if w.used != nil {
			parts = append(parts, fmt.Sprintf("%s %.0f%%", w.name, *w.used*100))
		}
	}
	if p.Status == "rejected" {
		parts = append(parts, "limit reached")
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " · ")
}

func (r *keyRun) revoke(ctx context.Context) error {
	// An id names a key outright, so --yes with an id needs no lookup. A
	// name only means something inside an organisation, and the prompt needs
	// the record to say what it is about.
	target := r.fs.Arg(0)
	if !r.yes || !id.HasPrefix(target, "key") {
		orgID, err := resolveOrg(ctx, r.c, r.org)
		if err != nil {
			return err
		}
		found, err := findKey(ctx, r.c, orgID, target)
		if err != nil {
			return err
		}
		target = found.ID
		if !r.yes {
			if err := confirmKeyRevoke(found); err != nil {
				return err
			}
		}
	}
	var gone struct {
		ID      string `json:"id"`
		Revoked bool   `json:"revoked"`
	}
	if err := r.c.do(ctx, "DELETE", "/v1/keys/"+url.PathEscape(target), nil, &gone); err != nil {
		return err
	}
	return out(r.asJSON, gone, func(w *table) {
		_, _ = fmt.Fprintf(w, "revoked\t%s\n", gone.ID)
	})
}

func (r *keyRun) rotate(ctx context.Context) error {
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	return rotateKey(ctx, r.c, orgID, r.fs.Arg(0), r.name, r.expires, r.asJSON)
}

func (r *keyRun) set(ctx context.Context) error {
	name := strings.TrimSpace(r.name)
	if name == "" {
		return errors.New("nothing to change; pass --name <name>")
	}
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	key, err := findKey(ctx, r.c, orgID, r.fs.Arg(0))
	if err != nil {
		return err
	}
	var renamed struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := r.c.do(ctx, "PATCH", "/v1/keys/"+url.PathEscape(key.ID),
		map[string]string{"name": name}, &renamed); err != nil {
		return err
	}
	return out(r.asJSON, renamed, func(w *table) {
		_, _ = fmt.Fprintf(w, "%s\t%s\n", renamed.ID, renamed.Name)
	})
}

// confirmKeyRevoke says what stops working and makes the administrator type
// the key back. A revoked key cannot be printed again or restored, so the
// prompt points at rotation, which replaces a key with no outage.
func confirmKeyRevoke(k store.KeySummary) error {
	named, noun := k.Name, "name"
	if named == "" {
		named, noun = k.ID, "id"
	}
	used := "  it has never been used"
	if k.LastUsedAt != nil {
		used = "  it was last used " + k.LastUsedAt.Format(time.DateOnly)
	}
	return confirm(fmt.Sprintf("Revoking %s (%s):", named, k.ID), []string{
		"  every client still holding it is refused from its next call",
		"  nothing can print it again, so it cannot be put back",
		used,
		"To replace it without an outage instead: keera key rotate " + named,
		"Usage history and the audit log are kept.",
	}, noun, named, "nothing was revoked")
}

// rotateKey replaces a key with one carrying the same project, owner, name and
// guardrails, then revokes the old one. The control plane does it in one
// transaction, so it never leaves both keys live or drops the key's limits.
func rotateKey(ctx context.Context, c *client, orgID, who, newName, expires string,
	asJSON bool,
) error {
	old, err := findKey(ctx, c, orgID, who)
	if err != nil {
		return err
	}
	if old.RevokedAt != nil {
		return fmt.Errorf("%s was already revoked on %s; there is nothing to rotate - %s",
			old.ID, old.RevokedAt.Format(time.DateOnly), freshKeyHint(ctx, c, old.ProjectID, old.Name))
	}

	var created struct {
		createdKey
		Replaced string `json:"replaced"`
	}
	req := map[string]string{"name": newName, "expires_in": expires}
	if err := c.do(ctx, "POST", "/v1/keys/"+url.PathEscape(old.ID)+"/rotate", req, &created); err != nil {
		return err
	}

	if asJSON {
		return out(true, created, nil)
	}
	// The secret alone on stdout, so `KEY=$(keera key rotate …)` captures it.
	fmt.Println(created.Key)
	fmt.Fprintf(os.Stderr, "\nkey %s replaces %s (%s). %s\n",
		created.ID, old.ID, old.Name, styleErr.warn("This is the only time it is shown."))
	if !old.Limits.IsZero() {
		fmt.Fprintln(os.Stderr, "Its guardrails were copied from the key it replaces.")
	}
	fmt.Fprintf(os.Stderr, "%s\n", styleErr.warn(fmt.Sprintf(
		"%s is revoked: every client still using it is already failing.", old.ID)))
	return nil
}

// findKey resolves an id or a name to a key.
//
// A name only matches keys that still work, because rotation leaves the old
// key revoked under the same name. If several live keys share the name, it
// refuses and lists their ids: acting on the wrong key breaks its clients.
func findKey(ctx context.Context, c *client, orgID, who string) (store.KeySummary, error) {
	keys, err := list[store.KeySummary](ctx, c, inOrg("/v1/keys", orgID))
	if err != nil {
		return store.KeySummary{}, err
	}
	want := strings.ToLower(strings.TrimSpace(who))
	var named, retired []store.KeySummary
	for _, k := range keys {
		if k.ID == who {
			return k, nil
		}
		if strings.ToLower(k.Name) != want {
			continue
		}
		if k.RevokedAt == nil {
			named = append(named, k)
		} else {
			retired = append(retired, k)
		}
	}
	switch {
	case len(named) == 1:
		return named[0], nil
	case len(named) > 1:
		ids := make([]string, 0, len(named))
		for _, k := range named {
			ids = append(ids, k.ID)
		}
		return store.KeySummary{}, fmt.Errorf(
			"%d keys in %s are called %q; name one by its id: %s",
			len(named), orgID, who, strings.Join(ids, ", "))
	case len(retired) > 0:
		return store.KeySummary{}, fmt.Errorf(
			"every key called %q in %s is already revoked; %s",
			who, orgID, freshKeyHint(ctx, c, retired[0].ProjectID, retired[0].Name))
	}
	return store.KeySummary{}, fmt.Errorf("no key %s in %s (see: keera key list)", who, orgID)
}

// freshKeyHint says how to get a new key in place of a revoked one. Only an
// administrator issues keys, so a member is sent to one.
func freshKeyHint(ctx context.Context, c *client, projectID, name string) string {
	if me, err := whoami(ctx, c); err == nil && me.Role == string(authn.RoleMember) {
		return "ask an administrator for a new key in your name"
	}
	fresh := "keera key create"
	if projectID != "" {
		fresh += " --project " + projectID
	}
	return fmt.Sprintf("issue a fresh key with: %s --name %q", fresh, name)
}
