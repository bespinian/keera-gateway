package cli

import (
	"context"
	"errors"
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
	*cmdRun
	addRole    string
	externalID string
	passkey    bool
}

func userCmd(ctx context.Context, args []string) error {
	r := &userRun{cmdRun: newCmdRun("user", args)}
	fs := r.fs
	fs.StringVar(&r.addRole, "role", "member", "member or admin")
	fs.StringVar(&r.externalID, "external-id", "",
		"operators only: the person's id at the identity provider, as <provider>:<subject>, "+
			"when it is known before their first sign-in")
	fs.BoolVar(&r.passkey, "passkey", false,
		"they sign in with a passkey instead of an identity provider; prints their set-up link")
	if done, err := r.parse(); done {
		return err
	}
	switch r.verb {
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
	if err := checkRole(r.addRole); err != nil {
		return err
	}
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	email := strings.ToLower(strings.TrimSpace(r.fs.Arg(0)))
	// The endpoint refuses somebody already here too. Asking first names
	// their current role and the command that changes it.
	switch existing, err := findUser(ctx, r.c, orgID, email); {
	case err == nil:
		return fmt.Errorf("%s is already in %s as %s; change that with: keera user role %s <role>",
			existing.Email, orgID, existing.Role, existing.Email)
	case !errors.Is(err, errNotFound):
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
	user, err := r.find(ctx)
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
		if len(users) == 0 {
			printNone(w, "people", "keera user add <email>")
			return
		}
		// The same columns as the panel's Users screen. The subject itself is
		// in --json.
		w.header("ID\tEMAIL\tNAME\tROLE\tSTATE\tSIGNS IN WITH\tLAST ACTIVE\tADDED")
		for _, u := range users {
			state := statusWord("active")
			if u.Disabled() {
				state = statusWord("disabled") + " " + u.DisabledAt.Format(time.DateOnly)
			}
			active := "never"
			if u.LastActiveAt != nil {
				active = u.LastActiveAt.Local().Format(time.DateOnly)
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				u.ID, u.Email, dash(u.Name), u.Role, state, signsInWith(u.ExternalID),
				statusWord(active),
				u.CreatedAt.Format(time.DateOnly))
		}
	})
}

// signsInWith names where a person signs in: the identity provider from the
// part of their subject before the first colon, or a passkey.
func signsInWith(subject string) string {
	provider, _, _ := strings.Cut(subject, ":")
	switch {
	case authn.IsPasskeyAccount(subject):
		return "passkey"
	case provider == "" || provider == "keera":
		return "(never signed in)"
	}
	return provider
}

func (r *userRun) role(ctx context.Context) error {
	role := r.fs.Arg(1)
	if err := checkRole(role); err != nil {
		return err
	}
	user, err := r.find(ctx)
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
	user, err := r.find(ctx)
	if err != nil {
		return err
	}
	if !r.yes {
		lines := []string{
			"  they cannot sign in, and are signed out everywhere",
			"  every key attributed to them is revoked, for good",
		}
		if authn.IsPasskeyAccount(user.ExternalID) {
			lines = append(lines, "  their passkeys are removed")
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
	raw, err := keepRaw(ctx, r.c, "POST", "/v1/users/"+url.PathEscape(user.ID)+"/disable", nil, &res)
	if err != nil {
		return err
	}
	return out(r.asJSON, raw, func(w *table) {
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
	user, err := r.find(ctx)
	if err != nil {
		return err
	}
	var res struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	raw, err := keepRaw(ctx, r.c, "POST", "/v1/users/"+url.PathEscape(user.ID)+"/enable", nil, &res)
	if err != nil {
		return err
	}
	return out(r.asJSON, raw, func(w *table) {
		if authn.IsPasskeyAccount(user.ExternalID) {
			_, _ = fmt.Fprintf(w, "%s is enabled. Their old keys stay revoked, and their "+
				"passkeys are gone. Send them a new set-up link:\n  %s\n",
				res.Email, style.cmd("keera user passkey-link "+res.Email))
			return
		}
		_, _ = fmt.Fprintf(w, "%s can sign in again. Their old keys stay revoked.\n", res.Email)
	})
}

// find resolves the person named by the first argument in the organisation.
func (r *userRun) find(ctx context.Context) (store.User, error) {
	return findUser(ctx, r.c, r.org, r.fs.Arg(0))
}

// checkRole refuses a role the control plane would refuse, before anything is
// sent.
func checkRole(role string) error {
	if slices.Contains(roleNames, role) {
		return nil
	}
	return fmt.Errorf("the role must be one of: %s%s", strings.Join(roleNames, ", "), roleHint)
}

// findUser resolves an email address or an id to a person. org is resolved
// as resolveOrg does.
func findUser(ctx context.Context, c *client, org, who string) (store.User, error) {
	return findNamed(ctx, c, org, who, "/v1/users", "person", "user",
		func(u store.User) (string, string) { return u.ID, u.Email })
}
