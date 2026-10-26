package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/id"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

type keyRun struct {
	*cmdRun
	project      string
	user         string
	name         string
	expires      string
	subscription bool
	noUser       bool
	pending      bool
	all          bool
}

// defaultKeyLife matches the panel's default, so a key does not live for ever
// because nobody chose.
const defaultKeyLife = 90 * 24 * time.Hour

// createdKey includes the secret, except for a pending key.
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
		"leave the key for the person it belongs to to activate; no secret is printed now")
	fs.StringVar(&r.name, "name", "", "what this key is called; what it is for, in a few words")
	fs.StringVar(&r.expires, "expires", "",
		"how long the key lasts, such as 90d, the last day it works, such as 2026-12-31, "+
			"or 'never'. A new key lasts 90 days unless told otherwise")
	r.reword("rotate", "expires", "how long the new key lasts, such as 90d, or the last day "+
		"it works, such as 2026-12-31 (default: the old key's lifetime)")
	fs.BoolVar(&r.subscription, "subscription", false,
		"a key that reaches only subscription models, for Claude Code signed in to a Claude plan")
	fs.BoolVar(&r.all, "all", false, "include revoked keys")
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
	// Required, as in the panel: a key without a name cannot be told apart.
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
	// Sent only when set, because a v0.9.0 gateway refuses unknown fields.
	if r.pending {
		req["pending"] = true
	}
	// A key with a person on it shows in `keera usage --by user` and on their
	// own screens.
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
	// The control API reads no expiry as never.
	switch r.expires {
	case "":
		req["expires_in_seconds"] = int64(defaultKeyLife / time.Second)
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

// owner is --user, or else the caller. --no-user and the operator key give a
// key that belongs to nobody.
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

// activateKey renames the key too when name is not empty.
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
	// Filtered here, because other callers need revoked keys and older
	// gateways cannot filter.
	if !r.all {
		keys = slices.DeleteFunc(keys, func(k store.KeySummary) bool { return k.RevokedAt != nil })
	}
	return out(r.asJSON, keys, func(w *table) {
		if len(keys) == 0 {
			printNone(w, "keys", "keera key create <name>")
			return
		}
		// Last use tells whether a key is safe to revoke.
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

// planText is how much of a Claude plan is used, in each of its two windows.
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

func (r *keyRun) find(ctx context.Context) (store.KeySummary, error) {
	return findKey(ctx, r.c, r.org, r.fs.Arg(0))
}

func (r *keyRun) revoke(ctx context.Context) error {
	// --yes with an id needs no lookup. A name needs the organisation, and
	// the prompt needs the record.
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
	// The control API reads no lifetime as "keep the old one's", so "never"
	// cannot be sent.
	if r.expires == "never" {
		return errors.New("--expires never is for 'keera key create'; a rotated key keeps " +
			"the old one's lifetime unless --expires gives another")
	}
	// Checked before the prompt.
	var expiry map[string]any
	if r.expires != "" {
		field, v, err := expiryField(r.expires)
		if err != nil {
			return err
		}
		expiry = map[string]any{field: v}
	}
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

// keyNamed is what is typed back to confirm: the name, or the id without one.
func keyNamed(k store.KeySummary) (named, noun string) {
	if k.Name == "" {
		return k.ID, "id"
	}
	return k.Name, "name"
}

// lastUsed does not say "never", because the control plane only reads use
// since the start of the month.
func lastUsed(k store.KeySummary) string {
	if k.LastUsedAt == nil {
		return "  it has not been used this month"
	}
	return "  it was last used " + k.LastUsedAt.Format(time.DateOnly)
}

// confirmKeyRevoke points at rotation, because a revoked key cannot be
// restored and rotation replaces a key with no outage.
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

// confirmKeyRotate asks first because rotation revokes the old key at once.
func confirmKeyRotate(k store.KeySummary) error {
	named, noun := keyNamed(k)
	return confirm(fmt.Sprintf("Rotating %s (%s):", named, k.ID), []string{
		"  a new key with the same project, owner and guardrails is printed once",
		"  the old key is revoked at once: every client still holding it is refused " +
			"from its next call",
		lastUsed(k),
	}, noun, named, "nothing was rotated")
}

type rotatedKey struct {
	createdKey
	ReplacedKeyID string `json:"replaced_key_id"`
}

// rotateKey keeps the project, owner and guardrails. An empty name or expiry
// keeps the old key's; expiry comes from expiryField.
func rotateKey(ctx context.Context, c *client, keyID, name string, expiry map[string]any) (rotatedKey, error) {
	req := map[string]any{"name": name}
	maps.Copy(req, expiry)
	var rotated rotatedKey
	err := c.do(ctx, "POST", "/v1/keys/"+url.PathEscape(keyID)+"/rotate", req, &rotated)
	return rotated, err
}

// expiryField sends a lifetime such as 90d in seconds. A day such as
// 2026-12-31 is the last day the key works, until its end in local time.
func expiryField(expires string) (field string, value any, err error) {
	day, err := time.ParseInLocation(time.DateOnly, expires, time.Local)
	if err != nil {
		life, err := parseDuration(expires)
		if err != nil || life < time.Second {
			return "", nil, fmt.Errorf("--expires %s is neither a lifetime such as 90d nor "+
				"a day such as 2026-12-31", expires)
		}
		return "expires_in_seconds", int64(life / time.Second), nil
	}
	end := day.AddDate(0, 0, 1).Add(-time.Second)
	if !end.After(time.Now()) {
		return "", nil, fmt.Errorf("--expires %s has already passed; give a day from today on", expires)
	}
	return "expires_at", end.Format(time.RFC3339), nil
}

// findKey takes an id or a name. A name only matches live keys, because
// rotation leaves the old key revoked under the same name. If several live
// keys share it, it refuses and lists their ids.
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

// freshKeyHint sends a member to an administrator, who alone issues keys.
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
