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

	"github.com/bespinian/keera-gateway/internal/store"
)

// roleNames is what --role and `keera user role` accept, in the panel's order.
//
// The operator role is left out: it spans organisations, so it only comes from
// KEERA_OPERATORS or KEERA_OIDC_OPERATOR_GROUPS. The control plane refuses it
// too; refusing here gives the clearer message.
var roleNames = []string{"member", "admin"}

const roleHint = " (the operator role comes from KEERA_OPERATORS or " +
	"KEERA_OIDC_OPERATOR_GROUPS)"

// userRun is one 'keera user' invocation.
type userRun struct {
	c      *client
	fs     *flag.FlagSet
	args   []string
	org    string
	asJSON bool
}

func userCmd(ctx context.Context, args []string) error {
	sub, rest := split(args)
	fs := flag.NewFlagSet("user "+sub, flag.ExitOnError)
	r := &userRun{c: newClient(), fs: fs, args: rest}
	fs.StringVar(&r.org, "org", "", orgUsage)
	fs.BoolVar(&r.asJSON, "json", false, jsonUsage)

	fs.Usage = func() { _ = printHelp(fs, "user", sub) }
	if want, ok := wantsHelp(args); ok {
		// 'add' and 'disable' declare these themselves; help needs them on the
		// set too.
		registerUserAddFlags(fs)
		fs.Bool("yes", false, yesUsage)
		return printHelp(fs, "user", want)
	}
	switch sub {
	case "add", "create", "invite", "new":
		return r.add(ctx)
	case "list", "ls", "":
		return r.list(ctx)
	case "role", "set-role":
		return r.role(ctx)
	case "disable", "offboard":
		return r.disable(ctx)
	case "enable":
		return r.enable(ctx)
	default:
		return unknownSub("user", sub)
	}
}

func registerUserAddFlags(fs *flag.FlagSet) (role, externalID *string) {
	role = fs.String("role", "member", "member or admin")
	externalID = fs.String("external-id", "",
		"the identity provider's subject, when it is known before the first sign-in")
	return role, externalID
}

func (r *userRun) add(ctx context.Context) error {
	role, externalID := registerUserAddFlags(r.fs)
	if err := parseArgs(r.fs, r.args, 1, "usage: keera user add <email> [--role member|admin]"); err != nil {
		return err
	}
	if !slices.Contains(roleNames, *role) {
		return fmt.Errorf("--role must be one of: %s%s",
			strings.Join(roleNames, ", "), roleHint)
	}
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	email := strings.TrimSpace(r.fs.Arg(0))
	// The endpoint upserts, so "add" would quietly re-role somebody already
	// here without signing them out. That is `keera user role`'s job.
	switch existing, err := findUser(ctx, r.c, orgID, email); {
	case err == nil:
		return fmt.Errorf("%s is already in %s as %s; change that with: keera user role %s <role>",
			existing.Email, orgID, existing.Role, existing.Email)
	case !errors.Is(err, errNoUser):
		return err
	}
	var user store.User
	if err := r.c.do(ctx, "POST", "/v1/users", map[string]string{
		"org_id": orgID, "email": email, "role": *role, "external_id": *externalID,
	}, &user); err != nil {
		return err
	}
	return out(r.asJSON, user, func(w *table) {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", user.ID, user.Email, user.Role)
		_, _ = fmt.Fprintln(w, "\nThe matching identity adopts this row on its first sign-in.")
	})
}

func (r *userRun) list(ctx context.Context) error {
	if err := parse(r.fs, r.args); err != nil {
		return err
	}
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
			if subject == "" {
				subject = "(never signed in)"
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
	if err := parseArgs(r.fs, r.args, 2, "usage: keera user role <email-or-id> <member|admin>"); err != nil {
		return err
	}
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
	yes := r.fs.Bool("yes", false, yesUsage)
	if err := parseArgs(r.fs, r.args, 1, "usage: keera user disable <email-or-id> [--yes]"); err != nil {
		return err
	}
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	user, err := findUser(ctx, r.c, orgID, r.fs.Arg(0))
	if err != nil {
		return err
	}
	if !*yes {
		fmt.Printf("%s\n", style.head("Disabling "+user.Email+":"))
		fmt.Println("  they cannot sign in, and are signed out everywhere")
		fmt.Println("  every key attributed to them is revoked, for good")
		if me, err := whoami(ctx, r.c); err == nil && me.Sandboxes {
			fmt.Println("  their agent sandboxes are terminated, and their own are suspended")
		}
		fmt.Println("Usage history and the audit log are kept. 'keera user enable' lets them back in.")
		if err := confirmTyping("email", user.Email, "nobody was disabled"); err != nil {
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
		_, _ = fmt.Fprintf(w, "Revoked %s.\n", keyCount(res.RevokedKeys))
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
	if err := parseArgs(r.fs, r.args, 1, "usage: keera user enable <email-or-id>"); err != nil {
		return err
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

// keyCount writes "1 key" or "n keys".
func keyCount(n int) string {
	if n == 1 {
		return "1 key"
	}
	return fmt.Sprintf("%d keys", n)
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
