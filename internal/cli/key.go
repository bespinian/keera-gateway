package cli

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/id"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// keyRun is one 'keera key' invocation.
type keyRun struct {
	c       *client
	fs      *flag.FlagSet
	args    []string
	org     string
	team    string
	user    string
	alias   string
	expires string
	asJSON  bool
}

// createdKey is a new key as the control API returns it, secret included.
type createdKey struct {
	store.KeyInfo
	Key string `json:"key"`
}

func keyCmd(ctx context.Context, args []string) error {
	sub, rest := split(args)
	fs := flag.NewFlagSet("key "+sub, flag.ExitOnError)
	r := &keyRun{c: newClient(), fs: fs, args: rest}
	fs.StringVar(&r.org, "org", "", orgUsage)
	fs.StringVar(&r.team, "team", "", "team id")
	fs.StringVar(&r.user, "user", "", "the person this key belongs to, by email or id")
	fs.StringVar(&r.alias, "alias", "", "what this key is called; what it is for, in one label")
	fs.StringVar(&r.expires, "expires", "", "lifetime, e.g. 720h")
	fs.BoolVar(&r.asJSON, "json", false, jsonUsage)

	fs.Usage = func() { _ = printHelp(fs, "key", sub) }
	if want, ok := wantsHelp(args); ok {
		// 'revoke' declares --yes itself; help needs it on the set too.
		fs.Bool("yes", false, yesUsage)
		return printHelp(fs, "key", want)
	}
	switch sub {
	case "create", "add", "new":
		return r.create(ctx)
	case "list", "ls", "":
		return r.list(ctx)
	case "revoke", "delete", "rm", "remove":
		return r.revoke(ctx)
	case "rotate":
		return r.rotate(ctx)
	default:
		return unknownSub("key", sub)
	}
}

func (r *keyRun) create(ctx context.Context) error {
	if err := parse(r.fs, r.args); err != nil {
		return err
	}
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	if r.alias == "" && r.fs.NArg() == 1 {
		r.alias = r.fs.Arg(0)
	}
	req := map[string]string{"org_id": orgID, "team_id": r.team, "alias": r.alias}
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
	if err := parse(r.fs, r.args); err != nil {
		return err
	}
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	path := inOrg("/v1/keys", orgID)
	if r.team != "" {
		path += "&team_id=" + url.QueryEscape(r.team)
	}
	keys, err := list[store.KeySummary](ctx, r.c, path)
	if err != nil {
		return err
	}
	return out(r.asJSON, keys, func(w *table) {
		// Last use decides whether a key is safe to revoke, so it is a column.
		w.header("ID\tALIAS\tPREFIX\tTEAM\tSTATE\tLAST USED")
		for _, k := range keys {
			last := "never"
			if k.LastUsedAt != nil {
				last = k.LastUsedAt.Format(time.DateOnly)
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s…\t%s\t%s\t%s\n",
				k.ID, k.Alias, k.Prefix, k.TeamID, statusWord(keyState(k)), statusWord(last))
		}
	})
}

func keyState(k store.KeySummary) string {
	switch {
	case k.RevokedAt != nil:
		return "revoked"
	case k.ExpiresAt != nil && k.ExpiresAt.Before(time.Now()):
		return "expired"
	default:
		return "active"
	}
}

func (r *keyRun) revoke(ctx context.Context) error {
	yes := r.fs.Bool("yes", false, yesUsage)
	if err := parseArgs(r.fs, r.args, 1, "usage: keera key revoke <alias> [--yes]"); err != nil {
		return err
	}
	// An id names a key outright, so --yes with an id needs no lookup. An
	// alias only means something inside an organisation, and the prompt needs
	// the record to say what it is about.
	target := r.fs.Arg(0)
	if !*yes || !id.HasPrefix(target, "key") {
		orgID, err := resolveOrg(ctx, r.c, r.org)
		if err != nil {
			return err
		}
		found, err := findKey(ctx, r.c, orgID, target)
		if err != nil {
			return err
		}
		target = found.ID
		if !*yes {
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
	if err := parseArgs(r.fs, r.args, 1,
		"usage: keera key rotate <alias> [--alias <new-alias>] [--expires 720h]"); err != nil {
		return err
	}
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	return rotateKey(ctx, r.c, orgID, r.fs.Arg(0), r.alias, r.expires, r.asJSON)
}

// confirmKeyRevoke says what stops working and makes the administrator type
// the key back. A revoked key cannot be printed again or restored, so the
// prompt points at rotation, which replaces a key with no outage.
func confirmKeyRevoke(k store.KeySummary) error {
	named, noun := k.Alias, "alias"
	if named == "" {
		named, noun = k.ID, "id"
	}
	fmt.Printf("%s\n", style.head(fmt.Sprintf("Revoking %s (%s):", named, k.ID)))
	fmt.Println("  every client still holding it is refused from its next call")
	fmt.Println("  nothing can print it again, so it cannot be put back")
	if k.LastUsedAt != nil {
		fmt.Printf("  it was last used %s\n", k.LastUsedAt.Format(time.DateOnly))
	} else {
		fmt.Println("  it has never been used")
	}
	fmt.Printf("To replace it without an outage instead: keera key rotate %s\n", named)
	fmt.Println("Usage history and the audit log are kept.")
	return confirmTyping(noun, named, "nothing was revoked")
}

// rotateKey replaces a key with one carrying the same team, owner, alias and
// guardrails, then revokes the old one.
//
// Done by hand, a rotation easily drops the team or the key's own limits,
// which quietly loosens a guardrail. The new key is issued first, so an
// interruption leaves a working key rather than none.
func rotateKey(ctx context.Context, c *client, orgID, who, newAlias, expires string,
	asJSON bool,
) error {
	old, err := findKey(ctx, c, orgID, who)
	if err != nil {
		return err
	}
	if old.RevokedAt != nil {
		return fmt.Errorf("%s was already revoked on %s; there is nothing to rotate - "+
			"issue a fresh key with: keera key create --team %s --alias %q",
			old.ID, old.RevokedAt.Format(time.DateOnly), old.TeamID, old.Alias)
	}

	created, err := issueReplacement(ctx, c, orgID, old, newAlias, expires)
	if err != nil {
		return err
	}

	// Copy the key's own guardrails before revoking the old one. If that
	// fails, stop with both keys live rather than finish with the limits gone.
	if hasLimits(old.Limits) {
		if err := c.do(ctx, "PUT", guardrailPath("key", created.ID),
			old.Limits, nil); err != nil {
			return fmt.Errorf("the new key %s was issued but its guardrails could not be "+
				"copied from %s: %w\nBoth keys are live. Revoke the new one with "+
				"'keera key revoke %s' and try again",
				created.ID, old.ID, err, created.ID)
		}
	}

	if err := c.do(ctx, "DELETE", "/v1/keys/"+url.PathEscape(old.ID), nil, nil); err != nil {
		return fmt.Errorf("the new key %s is ready, but %s could not be revoked: %w\n"+
			"Both keys are live. Revoke the old one with 'keera key revoke %s'",
			created.ID, old.ID, err, old.ID)
	}

	if asJSON {
		return out(true, struct {
			store.KeyInfo
			Key      string `json:"key"`
			Replaced string `json:"replaced"`
		}{KeyInfo: created.KeyInfo, Key: created.Key, Replaced: old.ID}, nil)
	}
	// The secret alone on stdout, so `KEY=$(keera key rotate …)` captures it.
	fmt.Println(created.Key)
	fmt.Fprintf(os.Stderr, "\nkey %s replaces %s (%s). %s\n",
		created.ID, old.ID, old.Alias, styleErr.warn("This is the only time it is shown."))
	if hasLimits(old.Limits) {
		fmt.Fprintln(os.Stderr, "Its guardrails were copied from the key it replaces.")
	}
	fmt.Fprintf(os.Stderr, "%s\n", styleErr.warn(fmt.Sprintf(
		"%s is revoked: every client still using it is already failing.", old.ID)))
	return nil
}

// issueReplacement issues the key that replaces old.
func issueReplacement(ctx context.Context, c *client, orgID string, old store.KeySummary,
	newAlias, expires string,
) (createdKey, error) {
	req := map[string]string{
		"org_id": orgID, "team_id": old.TeamID, "user_id": old.UserID, "alias": old.Alias,
	}
	if newAlias != "" {
		req["alias"] = newAlias
	}
	// The old key's lifetime, not its expiry date: a key rotated a week before
	// it lapses should not be replaced by one that lapses in a week.
	lifetime := expires
	if lifetime == "" {
		lifetime = keyLifetime(old)
	}
	if lifetime != "" {
		req["expires_in"] = lifetime
	}
	var created createdKey
	err := c.do(ctx, "POST", "/v1/keys", req, &created)
	return created, err
}

// keyLifetime is how long a key was issued for, as a duration the control API
// accepts. Empty for a key that never expires.
func keyLifetime(k store.KeySummary) string {
	if k.ExpiresAt == nil {
		return ""
	}
	d := max(k.ExpiresAt.Sub(k.CreatedAt).Round(time.Hour), time.Hour)
	return d.String()
}

// hasLimits reports whether a scope sets any guardrail of its own.
func hasLimits(l policy.Limits) bool {
	return len(l.AllowedModels) > 0 || l.MaxOutputTokens != nil || l.RPM != nil ||
		l.TPM != nil || l.BudgetMicros != nil || l.BudgetPeriod != nil ||
		l.SystemPrompt != nil || len(l.Filters) > 0
}

// findKey resolves an id or an alias to a key.
//
// An alias only matches keys that still work, because rotation leaves the old
// key revoked under the same alias. If several live keys share the alias, it
// refuses and lists their ids: acting on the wrong key breaks its clients.
func findKey(ctx context.Context, c *client, orgID, who string) (store.KeySummary, error) {
	keys, err := list[store.KeySummary](ctx, c, inOrg("/v1/keys", orgID))
	if err != nil {
		return store.KeySummary{}, err
	}
	want := strings.ToLower(strings.TrimSpace(who))
	var aliased, retired []store.KeySummary
	for _, k := range keys {
		if k.ID == who {
			return k, nil
		}
		if strings.ToLower(k.Alias) != want {
			continue
		}
		if k.RevokedAt == nil {
			aliased = append(aliased, k)
		} else {
			retired = append(retired, k)
		}
	}
	switch {
	case len(aliased) == 1:
		return aliased[0], nil
	case len(aliased) > 1:
		ids := make([]string, 0, len(aliased))
		for _, k := range aliased {
			ids = append(ids, k.ID)
		}
		return store.KeySummary{}, fmt.Errorf(
			"%d keys in %s use the alias %q; name one by its id: %s",
			len(aliased), orgID, who, strings.Join(ids, ", "))
	case len(retired) > 0:
		return store.KeySummary{}, fmt.Errorf(
			"every key using the alias %q in %s is already revoked; issue a fresh one with: keera key create",
			who, orgID)
	}
	return store.KeySummary{}, fmt.Errorf("no key %s in %s (see: keera key list)", who, orgID)
}
