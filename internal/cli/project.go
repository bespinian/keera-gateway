package cli

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"strings"

	"github.com/bespinian/keera-gateway/internal/store"
)

type projectRun struct {
	*cmdRun
	name        string
	description string
}

func projectCmd(ctx context.Context, args []string) error {
	r := &projectRun{cmdRun: newCmdRun("project", args)}
	fs := r.fs
	fs.StringVar(&r.name, "name", "", "the project's new name")
	fs.StringVar(&r.description, "description", "", "what the project is for")
	r.reword("set", "description", "what the project is for; an empty one clears it")
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
	body := map[string]string{"org_id": orgID, "name": r.fs.Arg(0), "description": r.description}
	return doOut(ctx, r.c, "POST", "/v1/projects", body, r.asJSON, projectRow)
}

func projectRow(w *table, t store.Project) {
	_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", t.ID, t.Name, dash(t.Description))
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
			projectRow(w, t)
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
	return doOut(ctx, r.c, "PATCH", "/v1/projects/"+url.PathEscape(project.ID), change, r.asJSON,
		projectRow)
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

func (r *projectRun) find(ctx context.Context) (store.Project, error) {
	return findProject(ctx, r.c, r.org, r.fs.Arg(0))
}

// deletedProject is the part of the DELETE /v1/projects/{id} reply the table
// shows.
type deletedProject struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	DetachedKeys int    `json:"detached_keys"`
}

// findProject takes an id or a name. The id is tried first, so a project
// named like an id still resolves to itself.
func findProject(ctx context.Context, c *client, org, given string) (store.Project, error) {
	return findNamed(ctx, c, org, given, "/v1/projects", "project", "project",
		func(p store.Project) (string, string) { return p.ID, p.Name })
}

// confirmProjectDelete says what the deletion removes and asks for the
// project's name. Live keys are counted because they block the deletion.
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
