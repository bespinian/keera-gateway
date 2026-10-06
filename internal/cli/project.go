package cli

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"strings"

	"github.com/bespinian/keera-gateway/internal/store"
)

// projectRun is one 'keera project' invocation.
type projectRun struct {
	*cmdRun
	name        string
	description string
}

func projectCmd(ctx context.Context, args []string) error {
	r := &projectRun{cmdRun: newCmdRun("project", args, "org", "yes", "json")}
	fs := r.fs
	fs.StringVar(&r.name, "name", "", "the project's new name")
	fs.StringVar(&r.description, "description", "",
		"what the project is for; an empty one clears it")
	if done, err := r.parse(); done {
		return err
	}
	switch r.verb {
	case "create":
		return r.create(ctx)
	case "set":
		return r.set(ctx)
	case "delete":
		return r.delete(ctx)
	default:
		return r.list(ctx)
	}
}

func (r *projectRun) create(ctx context.Context) error {
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	var project store.Project
	if err := r.c.do(ctx, "POST", "/v1/projects",
		map[string]string{"org_id": orgID, "name": r.fs.Arg(0), "description": r.description},
		&project); err != nil {
		return err
	}
	return out(r.asJSON, project, func(w *table) {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", project.ID, project.Name, dash(project.Description))
	})
}

func (r *projectRun) list(ctx context.Context) error {
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	projects, err := list[store.Project](ctx, r.c, inOrg("/v1/projects", orgID))
	if err != nil {
		return err
	}
	return out(r.asJSON, projects, func(w *table) {
		if len(projects) == 0 {
			printNone(w, "projects", "keera project create <name>")
			return
		}
		w.header("ID\tNAME\tDESCRIPTION")
		for _, t := range projects {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", t.ID, t.Name, dash(t.Description))
		}
	})
}

func (r *projectRun) set(ctx context.Context) error {
	change := map[string]string{}
	r.fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "name":
			change["name"] = strings.TrimSpace(r.name)
		case "description":
			change["description"] = r.description
		}
	})
	if !changesSomething(r.fs) {
		return nothingToChange("project set")
	}
	project, err := r.find(ctx)
	if err != nil {
		return err
	}
	var changed store.Project
	if err := r.c.do(ctx, "PATCH", "/v1/projects/"+url.PathEscape(project.ID),
		change, &changed); err != nil {
		return err
	}
	return out(r.asJSON, changed, func(w *table) {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", changed.ID, changed.Name, dash(changed.Description))
	})
}

func (r *projectRun) delete(ctx context.Context) error {
	project, err := r.find(ctx)
	if err != nil {
		return err
	}
	if !r.yes {
		if err := confirmProjectDelete(ctx, r.c, project); err != nil {
			return err
		}
	}
	var gone deletedProject
	raw, err := keepRaw(ctx, r.c, "DELETE", "/v1/projects/"+url.PathEscape(project.ID), nil, &gone)
	if err != nil {
		return err
	}
	return out(r.asJSON, raw, func(w *table) {
		_, _ = fmt.Fprintf(w, "deleted %s (%s)\t%s kept\n",
			gone.ID, gone.Name, plural(gone.DetachedKeys, "revoked key"))
	})
}

// find resolves the project named by the first argument.
func (r *projectRun) find(ctx context.Context) (store.Project, error) {
	return findProject(ctx, r.c, r.org, r.fs.Arg(0))
}

// deletedProject is what DELETE /v1/projects/{id} reports it removed, as far
// as the table shows it.
type deletedProject struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	DetachedKeys int    `json:"detached_keys"`
}

// findProject takes an id or a name and returns the project. Names are unique
// in an organisation and are what people remember. The id is tried first, so
// a project named like an id still resolves to itself. org is resolved as
// resolveOrg does.
func findProject(ctx context.Context, c *client, org, given string) (store.Project, error) {
	orgID, err := resolveOrg(ctx, c, org)
	if err != nil {
		return store.Project{}, err
	}
	projects, err := list[store.Project](ctx, c, inOrg("/v1/projects", orgID))
	if err != nil {
		return store.Project{}, err
	}
	for _, t := range projects {
		if t.ID == given {
			return t, nil
		}
	}
	for _, t := range projects {
		if strings.EqualFold(t.Name, given) {
			return t, nil
		}
	}
	return store.Project{}, notFound("project", given, orgID, "project")
}

// confirmProjectDelete says what the deletion takes with it and makes the
// administrator type the project's name back.
//
// Live keys are counted here because they block the deletion, while revoked
// ones stay behind as history.
func confirmProjectDelete(ctx context.Context, c *client, project store.Project) error {
	keys, err := list[store.KeyInfo](ctx, c, inOrg("/v1/keys", project.OrgID)+
		"&project_id="+url.QueryEscape(project.ID))
	if err != nil {
		return err
	}
	var live int
	for _, k := range keys {
		if k.RevokedAt == nil {
			live++
		}
	}
	lines := []string{
		"  its guardrails - budget, rate limit, allowed models and system prompt",
		"  its budget counters",
	}
	if live > 0 {
		lines = append(lines, styleErr.bad(fmt.Sprintf(
			"  this project still has %s; the deletion will be refused until they are revoked",
			plural(live, "live key"))))
	}
	lines = append(lines, "Revoked keys, usage history and the audit log are kept.")
	return confirm(fmt.Sprintf("Deleting %s (%s) removes:", project.Name, project.ID), lines,
		"project's name", project.Name, "nothing was deleted")
}
