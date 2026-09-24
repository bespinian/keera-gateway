package cli

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"strings"

	"github.com/bespinian/keera-gateway/internal/store"
)

// teamRun is one 'keera team' invocation.
type teamRun struct {
	c      *client
	fs     *flag.FlagSet
	args   []string
	org    string
	asJSON bool
}

func teamCmd(ctx context.Context, args []string) error {
	sub, rest := split(args)
	fs := flag.NewFlagSet("team "+sub, flag.ExitOnError)
	r := &teamRun{c: newClient(), fs: fs, args: rest}
	fs.StringVar(&r.org, "org", "", orgUsage)
	fs.BoolVar(&r.asJSON, "json", false, jsonUsage)
	fs.Usage = func() { _ = printHelp(fs, "team", sub) }
	if want, ok := wantsHelp(args); ok {
		// 'delete' declares --yes itself; help needs it on the set too.
		fs.Bool("yes", false, yesUsage)
		return printHelp(fs, "team", want)
	}

	switch sub {
	case "create", "add", "new":
		return r.create(ctx)
	case "list", "ls", "":
		return r.list(ctx)
	case "rename", "set", "edit", "update":
		return r.rename(ctx)
	case "delete", "rm", "remove":
		return r.delete(ctx)
	default:
		return unknownSub("team", sub)
	}
}

func (r *teamRun) create(ctx context.Context) error {
	if err := parseArgs(r.fs, r.args, 1, "usage: keera team create --org <id> <name>"); err != nil {
		return err
	}
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	var team store.Team
	if err := r.c.do(ctx, "POST", "/v1/teams",
		map[string]string{"org_id": orgID, "name": r.fs.Arg(0)}, &team); err != nil {
		return err
	}
	return out(r.asJSON, team, func(w *table) {
		_, _ = fmt.Fprintf(w, "%s\t%s\n", team.ID, team.Name)
	})
}

func (r *teamRun) list(ctx context.Context) error {
	if err := parse(r.fs, r.args); err != nil {
		return err
	}
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	teams, err := list[store.Team](ctx, r.c, inOrg("/v1/teams", orgID))
	if err != nil {
		return err
	}
	return out(r.asJSON, teams, func(w *table) {
		w.header("ID\tNAME")
		for _, t := range teams {
			_, _ = fmt.Fprintf(w, "%s\t%s\n", t.ID, t.Name)
		}
	})
}

func (r *teamRun) rename(ctx context.Context) error {
	if err := parseArgs(r.fs, r.args, 2, "usage: keera team rename <team> <new-name>"); err != nil {
		return err
	}
	team, err := r.find(ctx)
	if err != nil {
		return err
	}
	var renamed store.Team
	if err := r.c.do(ctx, "PATCH", "/v1/teams/"+url.PathEscape(team.ID),
		map[string]string{"name": r.fs.Arg(1)}, &renamed); err != nil {
		return err
	}
	return out(r.asJSON, renamed, func(w *table) {
		_, _ = fmt.Fprintf(w, "%s\t%s -> %s\n", renamed.ID, team.Name, renamed.Name)
	})
}

func (r *teamRun) delete(ctx context.Context) error {
	yes := r.fs.Bool("yes", false, yesUsage)
	if err := parseArgs(r.fs, r.args, 1, "usage: keera team delete <team> [--yes]"); err != nil {
		return err
	}
	team, err := r.find(ctx)
	if err != nil {
		return err
	}
	if !*yes {
		if err := confirmTeamDelete(ctx, r.c, team); err != nil {
			return err
		}
	}
	var gone deletedTeam
	if err := r.c.do(ctx, "DELETE", "/v1/teams/"+url.PathEscape(team.ID), nil, &gone); err != nil {
		return err
	}
	return out(r.asJSON, gone, func(w *table) {
		_, _ = fmt.Fprintf(w, "deleted %s (%s)\t%d revoked key(s) kept\n",
			gone.ID, gone.Name, gone.DetachedKeys)
	})
}

// find resolves the team named by the first argument.
func (r *teamRun) find(ctx context.Context) (store.Team, error) {
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return store.Team{}, err
	}
	return findTeam(ctx, r.c, orgID, r.fs.Arg(0))
}

// deletedTeam is what DELETE /v1/teams/{id} reports it removed.
type deletedTeam struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	DetachedKeys int    `json:"detached_keys"`
}

// findTeam takes an id or a name and returns the team. Names are unique in an
// organisation and are what people remember. The id is tried first, so a team
// named like an id still resolves to itself.
func findTeam(ctx context.Context, c *client, orgID, given string) (store.Team, error) {
	teams, err := list[store.Team](ctx, c, inOrg("/v1/teams", orgID))
	if err != nil {
		return store.Team{}, err
	}
	for _, t := range teams {
		if t.ID == given {
			return t, nil
		}
	}
	for _, t := range teams {
		if strings.EqualFold(t.Name, given) {
			return t, nil
		}
	}
	return store.Team{}, fmt.Errorf("no team %q in %s (see: keera team list)", given, orgID)
}

// confirmTeamDelete says what the deletion takes with it and makes the
// administrator type the team's name back.
//
// Live keys are counted here because they block the deletion, while revoked
// ones stay behind as history.
func confirmTeamDelete(ctx context.Context, c *client, team store.Team) error {
	keys, err := list[store.KeyInfo](ctx, c, inOrg("/v1/keys", team.OrgID)+
		"&team_id="+url.QueryEscape(team.ID))
	if err != nil {
		return err
	}
	var live int
	for _, k := range keys {
		if k.RevokedAt == nil {
			live++
		}
	}
	fmt.Printf("%s\n", style.head(fmt.Sprintf("Deleting %s (%s) removes:", team.Name, team.ID)))
	fmt.Println("  its guardrails - budget, rate limit, allowed models and system prompt")
	fmt.Println("  its budget counters")
	if live > 0 {
		fmt.Printf("%s\n", style.bad(fmt.Sprintf(
			"  %d key(s) in this team still work; the deletion will be refused until they are revoked",
			live)))
	}
	fmt.Println("Revoked keys, usage history and the audit log are kept.")
	return confirmTyping("team's name", team.Name, "nothing was deleted")
}
