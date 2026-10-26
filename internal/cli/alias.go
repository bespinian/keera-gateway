package cli

// Shared by 'model', 'mcp', 'filter' and 'router'. Each list is read whole,
// because the control API has no endpoint for a single entry.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"
)

// aliasPath builds a path for one entry; suffix is a route under it, such as
// "/check".
func aliasPath(kind, orgID, alias, suffix string) string {
	return inOrg("/v1/"+kind+"/"+url.PathEscape(alias)+suffix, orgID)
}

func findAlias[T any](ctx context.Context, a *aliasRun, alias string, aliasOf func(T) string) (T, error) {
	var zero T
	entries, err := list[T](ctx, a.c, inOrg("/v1/"+a.kind, a.orgID))
	if err != nil {
		return zero, err
	}
	for _, e := range entries {
		if aliasOf(e) == alias {
			return e, nil
		}
	}
	return zero, notFound(a.noun, alias, a.orgID, a.cmd)
}

// alreadyExists stops 'add' from quietly replacing an entry; 'set' does that.
func alreadyExists[T any](ctx context.Context, a *aliasRun, alias string, aliasOf func(T) string) error {
	_, err := findAlias(ctx, a, alias, aliasOf)
	switch {
	case err == nil:
		return fmt.Errorf("the %s %s already exists; change it with: keera %s set %s",
			a.noun, alias, a.cmd, alias)
	case errors.Is(err, errNotFound):
		return nil
	}
	return err
}

// startPut begins 'add' from fresh and 'set' from the stored entry, so a flag
// left out keeps its value.
func startPut[T any](ctx context.Context, a *aliasRun, alias string, fresh T, aliasOf func(T) string) (T, error) {
	if a.verb == "add" {
		return fresh, alreadyExists(ctx, a, alias, aliasOf)
	}
	if !changesSomething(a.fs) {
		return fresh, nothingToChange(a.cmd + " set")
	}
	return findAlias(ctx, a, alias, aliasOf)
}

// checkProbe fails the command when the check does not pass, so a pipeline
// can gate on it.
func checkProbe[T any](ctx context.Context, c *client, path, what string, asJSON bool,
	show func(*table, T), passed func(T) bool,
) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	var probe T
	if err := c.do(ctx, "POST", path, nil, &probe); err != nil {
		return err
	}
	if err := out(asJSON, probe, func(w *table) { show(w, probe) }); err != nil {
		return err
	}
	if !passed(probe) {
		return fmt.Errorf("%s did not pass its check", what)
	}
	return nil
}

type aliasRun struct {
	*cmdRun
	// kind is the API's name, such as "mcp-servers"; noun is for people, such
	// as "MCP server".
	kind, noun string
	// addHint is shown on an empty listing.
	addHint string
	orgID   string
}

// newAliasRun is newCmdRun for the alias commands. The caller declares its
// flags, then calls parse.
func newAliasRun(cmd, kind, noun string, args []string) *aliasRun {
	return &aliasRun{
		cmdRun: newCmdRun(cmd, args),
		kind:   kind, noun: noun, addHint: "keera " + cmd + " add <alias>",
	}
}

func (a *aliasRun) resolveOrg(ctx context.Context) (err error) {
	a.orgID, err = resolveOrg(ctx, a.c, a.org)
	return err
}

func (a *aliasRun) path(alias, suffix string) string {
	return aliasPath(a.kind, a.orgID, alias, suffix)
}

// aliasVerbs is what 'filter' and 'router' each do their own way.
type aliasVerbs interface {
	list(context.Context) error
	put(context.Context) error
	check(context.Context) error
	report(context.Context) error
	delete(context.Context) error
}

func (a *aliasRun) run(ctx context.Context, v aliasVerbs) error {
	if err := a.resolveOrg(ctx); err != nil {
		return err
	}
	switch a.verb {
	case "add", "set":
		return v.put(ctx)
	case "check":
		return v.check(ctx)
	case "report":
		return v.report(ctx)
	case "delete":
		return v.delete(ctx)
	default:
		return v.list(ctx)
	}
}

func listAliases[T any](ctx context.Context, a *aliasRun, show func(*table, []T)) error {
	entries, err := list[T](ctx, a.c, inOrg("/v1/"+a.kind, a.orgID))
	if err != nil {
		return err
	}
	return out(a.asJSON, entries, func(w *table) {
		if len(entries) == 0 {
			printNone(w, a.noun+"s", a.addHint)
			return
		}
		show(w, entries)
	})
}

func reportAlias[T any](ctx context.Context, a *aliasRun, since time.Duration,
	show func(w *table, alias string, since time.Duration, res T),
) error {
	alias := a.fs.Arg(0)
	path := a.path(alias, "/report") + "&from=" + url.QueryEscape(sinceParam(since))
	return doOut(ctx, a.c, "GET", path, nil, a.asJSON, func(w *table, res T) {
		show(w, alias, since, res)
	})
}

func putAlias[T any](ctx context.Context, a *aliasRun, alias string, body any,
	show func(*table, T),
) error {
	return doOut(ctx, a.c, "PUT", a.path(alias, ""), body, a.asJSON, show)
}

// deleteEntry asks first unless --yes is given. The alias is typed back,
// because clients and guardrails name the entry by it.
func (a *aliasRun) deleteEntry(ctx context.Context, alias string, lines []string) error {
	if !a.yes {
		if err := confirm("Deleting the "+a.noun+" "+alias+":", lines, "alias", alias,
			"nothing was deleted"); err != nil {
			return err
		}
	}
	return doOut(ctx, a.c, "DELETE", a.path(alias, ""), nil, a.asJSON,
		func(w *table, _ map[string]any) { _, _ = fmt.Fprintf(w, "deleted\t%s\n", alias) })
}
