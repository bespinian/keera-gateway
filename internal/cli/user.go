package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/store"
)

// roleNames is what --role and `keera user role` accept, in the panel's order.
//
// The operator role is left out: it spans organisations, so it only comes from
// KEERA_OPERATORS or KEERA_OIDC_<NAME>_OPERATOR_GROUPS. The control plane
// refuses it too; refusing here gives the clearer message.
var roleNames = []string{"member", "admin"}

const roleHint = " (the operator role comes from KEERA_OPERATORS or " +
	"KEERA_OIDC_<NAME>_OPERATOR_GROUPS)"

// userRun is one 'keera user' invocation.
type userRun struct {
	c          *client
	fs         *flag.FlagSet
	org        string
	addRole    string
	externalID string
	passkey    bool
	yes        bool
	asJSON     bool
}

func userCmd(ctx context.Context, args []string) error {
	sub, rest := split(args)
	fs := flag.NewFlagSet("user "+sub, flag.ExitOnError)
	r := &userRun{c: newClient(), fs: fs}
	fs.StringVar(&r.org, "org", "", orgUsage)
	fs.StringVar(&r.addRole, "role", "member", "member or admin")
	fs.StringVar(&r.externalID, "external-id", "",
		"the identity provider's subject, when it is known before the first sign-in")
	fs.BoolVar(&r.passkey, "passkey", false,
		"they sign in with a passkey instead of an identity provider; prints their set-up link")
	fs.BoolVar(&r.yes, "yes", false, yesUsage)
	fs.BoolVar(&r.asJSON, "json", false, jsonUsage)

	fs.Usage = func() { _ = printHelp(fs, "user", sub) }
	if want, ok := wantsHelp(args); ok {
		return printHelp(fs, "user", want)
	}
	verb, err := parseVerb(fs, "user", sub, rest)
	if err != nil {
		return err
	}
	switch verb {
	case "add":
		return r.add(ctx)
	case "role":
		return r.role(ctx)
	case "disable":
		return r.disable(ctx)
	case "enable":
		return r.enable(ctx)
	case "passkey-link":
		return r.passkeyLink(ctx)
	default:
		return r.list(ctx)
	}
}

// passkeyLinkOut is a set-up link as the control plane hands it out.
type passkeyLinkOut struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// printPasskeyLink tells the administrator what to do with a set-up link.
func printPasskeyLink(w *table, email string, link passkeyLinkOut) {
	_, _ = fmt.Fprintf(w, "\nSend this set-up link to %s privately. It works once, until %s:\n\n  %s\n",
		email, link.ExpiresAt.Local().Format("2006-01-02 15:04"), style.cmd(link.URL))
}

func (r *userRun) add(ctx context.Context) error {
	if r.passkey && r.externalID != "" {
		return opposites("passkey", "external-id")
	}
	if !slices.Contains(roleNames, r.addRole) {
		return fmt.Errorf("--role must be one of: %s%s",
			strings.Join(roleNames, ", "), roleHint)
	}
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	email := strings.TrimSpace(r.fs.Arg(0))
	// The endpoint refuses somebody already here too. Asking first names
	// their current role and the command that changes it.
	switch existing, err := findUser(ctx, r.c, orgID, email); {
	case err == nil:
		return fmt.Errorf("%s is already in %s as %s; change that with: keera user role %s <role>",
			existing.Email, orgID, existing.Role, existing.Email)
	case !errors.Is(err, errNoUser):
		return err
	}
	signIn := ""
	if r.passkey {
		signIn = "passkey"
	}
	var user struct {
		store.User
		PasskeyLink *passkeyLinkOut `json:"passkey_link,omitempty"`
	}
	if err := r.c.do(ctx, "POST", "/v1/users", map[string]string{
		"org_id": orgID, "email": email, "role": r.addRole, "external_id": r.externalID,
		"sign_in": signIn,
	}, &user); err != nil {
		return err
	}
	return out(r.asJSON, user, func(w *table) {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", user.ID, user.Email, user.Role)
		if user.PasskeyLink != nil {
			printPasskeyLink(w, user.Email, *user.PasskeyLink)
			return
		}
		_, _ = fmt.Fprintln(w, "\nThe matching identity adopts this row on its first sign-in.")
	})
}

// passkeyLink hands out a new set-up link: for a new device, after a lost
// passkey, or to move someone who has not signed in yet to passkeys.
func (r *userRun) passkeyLink(ctx context.Context) error {
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	user, err := findUser(ctx, r.c, orgID, r.fs.Arg(0))
	if err != nil {
		return err
	}
	var link passkeyLinkOut
	if err := r.c.do(ctx, "POST", "/v1/users/"+url.PathEscape(user.ID)+"/passkey-link",
		nil, &link); err != nil {
		return err
	}
	return out(r.asJSON, link, func(w *table) {
		printPasskeyLink(w, user.Email, link)
		_, _ = fmt.Fprintln(w, "\nIt replaces any earlier link. Their passkeys so far keep working.")
	})
}

func (r *userRun) list(ctx context.Context) error {
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	users, err := list[store.User](ctx, r.c, inOrg("/v1/users", orgID))
	if err != nil {
		return err
	}
	return out(r.asJSON, users, func(w *table) {
		w.header("ID\tEMAIL\tROLE\tSTATE\tIDP SUBJECT\tCREATED")
		for _, u := range users {
			subject := u.ExternalID
			switch {
			case subject == "":
				subject = "(never signed in)"
			case authn.IsPasskeyAccount(subject):
				subject = "(passkey)"
			}
			state := style.ok("active")
			if u.Disabled() {
				state = style.bad("disabled " + u.DisabledAt.Format(time.DateOnly))
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				u.ID, u.Email, u.Role, state, subject, u.CreatedAt.Format(time.DateOnly))
		}
	})
}

func (r *userRun) role(ctx context.Context) error {
	role := r.fs.Arg(1)
	if !slices.Contains(roleNames, role) {
		return fmt.Errorf("the role must be one of: %s%s",
			strings.Join(roleNames, ", "), roleHint)
	}
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	user, err := findUser(ctx, r.c, orgID, r.fs.Arg(0))
	if err != nil {
		return err
	}
	var res struct {
		ID   string `json:"id"`
		Role string `json:"role"`
	}
	if err := r.c.do(ctx, "PATCH", "/v1/users/"+url.PathEscape(user.ID),
		map[string]string{"role": role}, &res); err != nil {
		return err
	}
	return out(r.asJSON, res, func(w *table) {
		// A role takes effect on the next request, so the change signs them out.
		_, _ = fmt.Fprintf(w, "%s is now %s, and has been signed out everywhere.\n",
			user.Email, res.Role)
	})
}

func (r *userRun) disable(ctx context.Context) error {
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	user, err := findUser(ctx, r.c, orgID, r.fs.Arg(0))
	if err != nil {
		return err
	}
	if !r.yes {
		lines := []string{
			"  they cannot sign in, and are signed out everywhere",
			"  every key attributed to them is revoked, for good",
		}
		if me, err := whoami(ctx, r.c); err == nil && me.Sandboxes {
			lines = append(lines, "  their agent sandboxes and any not started yet are terminated; "+
				"the rest are suspended")
		}
		lines = append(lines, "Usage history and the audit log are kept. 'keera user enable' lets them back in.")
		if err := confirm("Disabling "+user.Email+":", lines, "email", user.Email,
			"nobody was disabled"); err != nil {
			return err
		}
	}
	var res struct {
		ID          string `json:"id"`
		Email       string `json:"email"`
		RevokedKeys int    `json:"revoked_keys"`
		Sandboxes   *struct {
			Terminated int `json:"terminated"`
			Suspended  int `json:"suspended"`
			Failed     int `json:"failed"`
		} `json:"sandboxes,omitempty"`
		Warning string `json:"warning,omitempty"`
	}
	if err := r.c.do(ctx, "POST", "/v1/users/"+url.PathEscape(user.ID)+"/disable",
		nil, &res); err != nil {
		return err
	}
	return out(r.asJSON, res, func(w *table) {
		_, _ = fmt.Fprintf(w, "%s is disabled and signed out everywhere.\n", res.Email)
		_, _ = fmt.Fprintf(w, "Revoked %s.\n", plural(res.RevokedKeys, "key"))
		if sb := res.Sandboxes; sb != nil && sb.Terminated+sb.Suspended > 0 {
			_, _ = fmt.Fprintf(w, "Sandboxes: %d terminated, %d suspended.\n",
				sb.Terminated, sb.Suspended)
		}
		if res.Warning != "" {
			_, _ = fmt.Fprintln(w, style.warn("Warning: "+res.Warning+"."))
		}
	})
}

func (r *userRun) enable(ctx context.Context) error {
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	user, err := findUser(ctx, r.c, orgID, r.fs.Arg(0))
	if err != nil {
		return err
	}
	var res struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	if err := r.c.do(ctx, "POST", "/v1/users/"+url.PathEscape(user.ID)+"/enable",
		nil, &res); err != nil {
		return err
	}
	return out(r.asJSON, res, func(w *table) {
		_, _ = fmt.Fprintf(w, "%s can sign in again. Their old keys stay revoked.\n", res.Email)
	})
}

// errNoUser is what findUser returns when nobody matches, so a caller can
// tell "not there" from "the control API is down".
var errNoUser = errors.New("no such person")

// findUser resolves an email address or an id to a person.
func findUser(ctx context.Context, c *client, orgID, who string) (store.User, error) {
	users, err := list[store.User](ctx, c, inOrg("/v1/users", orgID))
	if err != nil {
		return store.User{}, err
	}
	want := strings.ToLower(strings.TrimSpace(who))
	for _, u := range users {
		if u.ID == who || strings.ToLower(u.Email) == want {
			return u, nil
		}
	}
	return store.User{}, fmt.Errorf("%w in %s: %s (see: keera user list)", errNoUser, orgID, who)
}
