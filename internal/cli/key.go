package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
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
	// noUser issues a key attributed to nobody, such as a pipeline's.
	noUser bool
	// pending leaves the key for its holder to activate.
	pending bool
}

// defaultKeyLife is how long a new key lasts unless --expires says otherwise.
// The panel offers the same default, so a key does not live for ever because
// nobody chose.
const defaultKeyLife = "2160h"

// createdKey is a new key as the control API returns it, secret included. A
// pending key has no secret yet.
type createdKey struct {
	store.KeyInfo
	Key string `json:"key,omitempty"`
}

func keyCmd(ctx context.Context, args []string) error {
	r := &keyRun{cmdRun: newCmdRun("key", args)}
	fs := r.fs
	fs.StringVar(&r.project, "project", "",
		"project, by name or id; without it, the organisation's oldest project")
	r.reword("list", "project", "only the keys of one project, by name or id")
	fs.StringVar(&r.user, "user", "",
		"the person this key belongs to, by email or id (default: you)")
	fs.BoolVar(&r.noUser, "no-user", false,
		"a key that belongs to nobody, such as a pipeline's")
	fs.BoolVar(&r.pending, "pending", false,
		"leave the key for the person it belongs to to activate; only they see its secret")
	fs.StringVar(&r.name, "name", "", "what this key is called; what it is for, in a few words")
	fs.StringVar(&r.expires, "expires", "",
		"how long the key lasts, such as 720h, the last day it works, such as 2026-12-31, "+
			"or 'never'. A new key lasts "+defaultKeyLife+" (90 days) unless told otherwise")
	r.reword("rotate", "expires", "how long the new key lasts, such as 720h, or the last day "+
		"it works, such as 2026-12-31 (default: the old key's lifetime)")
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
	case "activate":
		return r.activate(ctx)
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
	// Required, as in the panel: a key without a name is a row nobody can
	// identify later.
	if strings.TrimSpace(r.name) == "" {
		return errors.New("name the key after what it is for, such as: keera key create \"Ada's laptop\"")
	}
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	project, err := projectID(ctx, r.c, orgID, r.project)
	if err != nil {
		return err
	}
	req := map[string]any{"org_id": orgID, "project_id": project, "name": r.name}
	// Sent only when set, so a plain create still works against a v0.9.0
	// gateway, which refuses fields it does not know.
	if r.pending {
		req["pending"] = true
	}
	// A key with a person on it is what makes `keera usage --by user` work,
	// and what puts it on that person's own screens.
	owner, err := r.owner(ctx, orgID)
	if err != nil {
		return err
	}
	if owner.ID != "" {
		req["user_id"] = owner.ID
	}
	if r.pending && owner.ID == "" {
		return errors.New("--pending leaves the key for the person it belongs to; " +
			"name them with --user")
	}
	// The control API reads no expiry as a key that never expires.
	switch r.expires {
	case "":
		req["expires_in"] = defaultKeyLife
	case "never":
	default:
		field, v, err := expiryField(r.expires)
		if err != nil {
			return err
		}
		req[field] = v
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
	if created.Pending {
		fmt.Printf("key %s created for %s. It has no secret until they activate it: in the "+
			"panel, or with 'keera key activate %s'.\n", created.ID, owner.Email, created.ID)
		return nil
	}
	handOver(created.Key, fmt.Sprintf("key %s created for %s.", created.ID, orgID))
	return nil
}

// owner is the person a new key belongs to: --user, or else the caller, so
// the key shows on their own screens. --no-user, and the operator key, which
// is nobody, give a key that belongs to nobody.
func (r *keyRun) owner(ctx context.Context, orgID string) (store.User, error) {
	switch {
	case r.noUser && r.user != "":
		return store.User{}, opposites("user", "no-user")
	case r.noUser:
		return store.User{}, nil
	case r.user != "":
		return findUser(ctx, r.c, orgID, r.user)
	}
	me, err := whoami(ctx, r.c)
	if err != nil {
		return store.User{}, err
	}
	// An operator working in another organisation is not a person there.
	if me.UserID == "" || me.OrgID != orgID {
		return store.User{}, nil
	}
	return store.User{ID: me.UserID, Email: me.Email}, nil
}

// activate gives a pending key its secret and prints it, once.
func (r *keyRun) activate(ctx context.Context) error {
	k, err := r.find(ctx)
	if err != nil {
		return err
	}
	created, err := activateKey(ctx, r.c, k.ID, "")
	if err != nil {
		return err
	}
	if r.asJSON {
		return out(true, created, nil)
	}
	handOver(created.Key, fmt.Sprintf("key %s (%s) is activated.", created.ID, created.Name))
	return nil
}

// activateKey gives a pending key its secret. A name renames it in the same
// step; an empty one keeps its name.
func activateKey(ctx context.Context, c *client, keyID, name string) (createdKey, error) {
	var created createdKey
	err := c.do(ctx, "POST", "/v1/keys/"+url.PathEscape(keyID)+"/activate",
		map[string]string{"name": name}, &created)
	return created, err
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
			// The control plane reads use since the start of the month.
			last := "not this month"
			if k.LastUsedAt != nil {
				last = k.LastUsedAt.Format(time.DateOnly)
			}
			prefix := "-"
			if k.Prefix != "" {
				prefix = k.Prefix + "…"
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				k.ID, k.Name, prefix, dash(string(k.Kind)), dash(k.ProjectID),
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
	// Checked before the prompt, so a mistyped day does not cost a confirmation.
	field, v, err := expiryField(r.expires)
	if err != nil {
		return err
	}
	expiry := map[string]string{field: v}
	old, err := r.find(ctx)
	if err != nil {
		return err
	}
	if old.RevokedAt != nil {
		return fmt.Errorf("%s was already revoked on %s; there is nothing to rotate - %s",
			old.ID, old.RevokedAt.Format(time.DateOnly), freshKeyHint(ctx, r.c, old.ProjectID, old.Name))
	}
	if old.Pending {
		return fmt.Errorf("%s is not activated yet, so it has no secret to replace; "+
			"activate it with: keera key activate %s", old.ID, old.ID)
	}
	if !r.yes {
		if err := confirmKeyRotate(old); err != nil {
			return err
		}
	}
	created, err := rotateKey(ctx, r.c, old.ID, r.name, expiry)
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

// lastUsed says when a key was last used, for a prompt about it. The control
// plane only reads use since the start of the month, so no use is not "never".
func lastUsed(k store.KeySummary) string {
	if k.LastUsedAt == nil {
		return "  it has not been used this month"
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
// An empty name or expiry keeps the old key's. expiry holds what expiryField
// gave.
func rotateKey(ctx context.Context, c *client, keyID, name string, expiry map[string]string) (rotatedKey, error) {
	req := map[string]string{"name": name, "expires_in": ""}
	maps.Copy(req, expiry)
	var rotated rotatedKey
	err := c.do(ctx, "POST", "/v1/keys/"+url.PathEscape(keyID)+"/rotate", req, &rotated)
	return rotated, err
}

// expiryField is --expires as a key request carries it. A lifetime such as
// 720h goes as it is. A day such as 2026-12-31 is the last day the key works,
// so it expires at the end of that day, in local time.
func expiryField(expires string) (field, value string, err error) {
	day, err := time.ParseInLocation(time.DateOnly, expires, time.Local)
	if err != nil {
		return "expires_in", expires, nil
	}
	end := day.AddDate(0, 0, 1).Add(-time.Second)
	if !end.After(time.Now()) {
		return "", "", fmt.Errorf("--expires %s has already passed; give a day from today on", expires)
	}
	return "expires_at", end.Format(time.RFC3339), nil
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
