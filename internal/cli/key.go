package cli

import (
	"context"
	"errors"
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
	*cmdRun
	project string
	user    string
	name    string
	expires string
	// subscription issues a key for Claude Code signed in to a Claude plan.
	subscription bool
}

// defaultKeyLife is how long a new key lasts unless --expires says otherwise.
// The panel offers the same default, so a key does not live for ever because
// nobody chose.
const defaultKeyLife = "2160h"

// createdKey is a new key as the control API returns it, secret included.
type createdKey struct {
	store.KeyInfo
	Key string `json:"key"`
}

func keyCmd(ctx context.Context, args []string) error {
	r := &keyRun{cmdRun: newCmdRun("key", args, "org", "yes", "json")}
	fs := r.fs
	fs.StringVar(&r.project, "project", "", "project, by name or id. A new key without it goes in the organisation's oldest project; "+
		"a list without it shows every project")
	fs.StringVar(&r.user, "user", "", "the person this key belongs to, by email or id")
	fs.StringVar(&r.name, "name", "", "what this key is called; what it is for, in a few words")
	fs.StringVar(&r.expires, "expires", "",
		"how long the key lasts, such as 720h, or 'never'. A new key lasts "+defaultKeyLife+
			" (90 days) unless told otherwise; a rotated key keeps the old one's lifetime")
	fs.BoolVar(&r.subscription, "subscription", false,
		"a key that reaches only subscription models, for Claude Code signed in to a Claude plan")
	if done, err := r.parse(); done {
		return err
	}
	switch r.verb {
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
	// The control API reads no expiry as a key that never expires.
	switch r.expires {
	case "":
		req["expires_in"] = defaultKeyLife
	case "never":
	default:
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
	handOver(created.Key, fmt.Sprintf("key %s created for %s.", created.ID, orgID))
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
		if len(keys) == 0 {
			printNone(w, "keys", "keera key create <name>")
			return
		}
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

// find resolves the key named by the first argument in the organisation.
func (r *keyRun) find(ctx context.Context) (store.KeySummary, error) {
	return findKey(ctx, r.c, r.org, r.fs.Arg(0))
}

func (r *keyRun) revoke(ctx context.Context) error {
	// An id names a key outright, so --yes with an id needs no lookup. A
	// name only means something inside an organisation, and the prompt needs
	// the record to say what it is about.
	target := r.fs.Arg(0)
	if !r.yes || !id.HasPrefix(target, "key") {
		found, err := r.find(ctx)
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
	// The control API reads no lifetime as "keep the old one's", so a rotated
	// key cannot be told to never expire.
	if r.expires == "never" {
		return errors.New("--expires never is for 'keera key create'; a rotated key keeps " +
			"the old one's lifetime unless --expires gives another")
	}
	old, err := r.find(ctx)
	if err != nil {
		return err
	}
	if old.RevokedAt != nil {
		return fmt.Errorf("%s was already revoked on %s; there is nothing to rotate - %s",
			old.ID, old.RevokedAt.Format(time.DateOnly), freshKeyHint(ctx, r.c, old.ProjectID, old.Name))
	}
	if !r.yes {
		if err := confirmKeyRotate(old); err != nil {
			return err
		}
	}
	created, err := rotateKey(ctx, r.c, old.ID, r.name, r.expires)
	if err != nil {
		return err
	}
	if r.asJSON {
		return out(true, created, nil)
	}
	handOver(created.Key, fmt.Sprintf("key %s replaces %s (%s).", created.ID, old.ID, old.Name))
	if !old.Limits.IsZero() {
		fmt.Fprintln(os.Stderr, "Its guardrails were copied from the key it replaces.")
	}
	fmt.Fprintf(os.Stderr, "%s\n", styleErr.warn(fmt.Sprintf(
		"%s is revoked: every client still using it is already failing.", old.ID)))
	return nil
}

func (r *keyRun) set(ctx context.Context) error {
	if !changesSomething(r.fs) {
		return nothingToChange("key set")
	}
	name := strings.TrimSpace(r.name)
	if name == "" {
		return errors.New("--name is empty; a key's name says what it is for")
	}
	key, err := r.find(ctx)
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

// keyNamed is what a person types back to confirm: the key's name, or its id
// when it has none.
func keyNamed(k store.KeySummary) (named, noun string) {
	if k.Name == "" {
		return k.ID, "id"
	}
	return k.Name, "name"
}

// lastUsed says when a key was last used, for a prompt about it.
func lastUsed(k store.KeySummary) string {
	if k.LastUsedAt == nil {
		return "  it has never been used"
	}
	return "  it was last used " + k.LastUsedAt.Format(time.DateOnly)
}

// confirmKeyRevoke says what stops working and makes the administrator type
// the key back. A revoked key cannot be printed again or restored, so the
// prompt points at rotation, which replaces a key with no outage.
func confirmKeyRevoke(k store.KeySummary) error {
	named, noun := keyNamed(k)
	return confirm(fmt.Sprintf("Revoking %s (%s):", named, k.ID), []string{
		"  every client still holding it is refused from its next call",
		"  nothing can print it again, so it cannot be put back",
		lastUsed(k),
		"To replace it without an outage instead: keera key rotate " + named,
		"Usage history and the audit log are kept.",
	}, noun, named, "nothing was revoked")
}

// confirmKeyRotate asks before a rotation, which revokes the old key at once:
// whatever still holds it fails until it is given the new one.
func confirmKeyRotate(k store.KeySummary) error {
	named, noun := keyNamed(k)
	return confirm(fmt.Sprintf("Rotating %s (%s):", named, k.ID), []string{
		"  a new key with the same project, owner and guardrails is printed once",
		"  the old key is revoked at once: every client still holding it is refused " +
			"from its next call",
		lastUsed(k),
	}, noun, named, "nothing was rotated")
}

// rotatedKey is a new key from a rotation, and the id of the key it replaced.
type rotatedKey struct {
	createdKey
	Replaced string `json:"replaced"`
}

// rotateKey replaces a key with one carrying the same project, owner and
// guardrails, then revokes the old one. The control plane does it in one
// transaction, so it never leaves both keys live or drops the key's limits.
// An empty name or expires keeps the old key's.
func rotateKey(ctx context.Context, c *client, keyID, name, expires string) (rotatedKey, error) {
	var rotated rotatedKey
	err := c.do(ctx, "POST", "/v1/keys/"+url.PathEscape(keyID)+"/rotate",
		map[string]string{"name": name, "expires_in": expires}, &rotated)
	return rotated, err
}

// findKey resolves an id or a name to a key.
//
// A name only matches keys that still work, because rotation leaves the old
// key revoked under the same name. If several live keys share the name, it
// refuses and lists their ids: acting on the wrong key breaks its clients.
// org is resolved as resolveOrg does.
func findKey(ctx context.Context, c *client, org, who string) (store.KeySummary, error) {
	orgID, err := resolveOrg(ctx, c, org)
	if err != nil {
		return store.KeySummary{}, err
	}
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
	return store.KeySummary{}, notFound("key", who, orgID, "key")
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
