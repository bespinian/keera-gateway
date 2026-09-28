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

	"github.com/bespinian/keera-gateway/internal/id"
	"github.com/bespinian/keera-gateway/internal/store"
)

// keyRun is one 'keera key' invocation.
type keyRun struct {
	c       *client
	fs      *flag.FlagSet
	args    []string
	sub     string
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
	r := &keyRun{c: newClient(), fs: fs, args: rest, sub: sub}
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

// parse reads the verb's flags and refuses another verb's. n is how many
// arguments it takes, or -1 for any.
func (r *keyRun) parse(n int, usage string) error {
	if err := parse(r.fs, r.args); err != nil {
		return err
	}
	if n >= 0 && r.fs.NArg() != n {
		return errors.New(usage)
	}
	return verbFlags(r.fs, "key", r.sub)
}

func (r *keyRun) create(ctx context.Context) error {
	if err := r.parse(-1, ""); err != nil {
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
	if err := r.parse(-1, ""); err != nil {
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
	if err := r.parse(1, "usage: keera key revoke <alias> [--yes]"); err != nil {
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
	if err := r.parse(1,
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
// guardrails, then revokes the old one. The control plane does it in one
// transaction, so it never leaves both keys live or drops the key's limits.
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

	var created struct {
		createdKey
		Replaced string `json:"replaced"`
	}
	req := map[string]string{"alias": newAlias, "expires_in": expires}
	if err := c.do(ctx, "POST", "/v1/keys/"+url.PathEscape(old.ID)+"/rotate", req, &created); err != nil {
		return err
	}

	if asJSON {
		return out(true, created, nil)
	}
	// The secret alone on stdout, so `KEY=$(keera key rotate …)` captures it.
	fmt.Println(created.Key)
	fmt.Fprintf(os.Stderr, "\nkey %s replaces %s (%s). %s\n",
		created.ID, old.ID, old.Alias, styleErr.warn("This is the only time it is shown."))
	if !old.Limits.IsZero() {
		fmt.Fprintln(os.Stderr, "Its guardrails were copied from the key it replaces.")
	}
	fmt.Fprintf(os.Stderr, "%s\n", styleErr.warn(fmt.Sprintf(
		"%s is revoked: every client still using it is already failing.", old.ID)))
	return nil
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
