package cli

// What the commands for entries named by an alias share: 'model', 'mcp',
// 'filter' and 'router'. Each kind is a short list in one organisation, read
// whole, because the control API has no endpoint for a single entry.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"
)

// aliasPath is a control API path for one entry of kind, such as "filters",
// in orgID. suffix addresses a route under it, such as "/check".
func aliasPath(kind, orgID, alias, suffix string) string {
	return inOrg("/v1/"+kind+"/"+url.PathEscape(alias)+suffix, orgID)
}

// findAlias reads the entry called alias from the organisation's list.
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

// alreadyExists refuses an 'add' that would quietly replace the entry of the
// same alias. Changing one is what 'set' is for.
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

// startPut is where 'add' and 'set' begin. 'add' refuses an alias that is
// taken and starts from fresh. 'set' starts from the stored entry, so a flag
// left out keeps what is there.
func startPut[T any](ctx context.Context, a *aliasRun, alias string, fresh T, aliasOf func(T) string) (T, error) {
	if a.verb == "add" {
		return fresh, alreadyExists(ctx, a, alias, aliasOf)
	}
	if !changesSomething(a.fs) {
		return fresh, nothingToChange(a.cmd + " set")
	}
	return findAlias(ctx, a, alias, aliasOf)
}

// checkProbe runs the check at path and prints what came back. A check that
// did not pass fails the command, so a pipeline can gate on it.
func checkProbe[T any](ctx context.Context, c *client, path, what string, asJSON bool,
	print func(*table, T), passed func(T) bool,
) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	var probe T
	if err := c.do(ctx, "POST", path, nil, &probe); err != nil {
		return err
	}
	if err := out(asJSON, probe, func(w *table) { print(w, probe) }); err != nil {
		return err
	}
	if !passed(probe) {
		return fmt.Errorf("%s did not pass its check", what)
	}
	return nil
}

// aliasRun is one invocation of a command for entries named by an alias.
type aliasRun struct {
	*cmdRun
	// kind is the control API's name for the list, such as "mcp-servers",
	// and noun what one entry is called, such as "MCP server".
	kind, noun string
	// addHint is the command that adds one, for an empty listing.
	addHint string
	orgID   string
}

// newAliasRun is newCmdRun with the names the four alias commands print. The
// caller declares its own flags, then calls parse.
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

func listAliases[T any](ctx context.Context, a *aliasRun, print func(*table, []T)) error {
	entries, err := list[T](ctx, a.c, inOrg("/v1/"+a.kind, a.orgID))
	if err != nil {
		return err
	}
	return out(a.asJSON, entries, func(w *table) {
		if len(entries) == 0 {
			printNone(w, a.noun+"s", a.addHint)
			return
		}
		print(w, entries)
	})
}

// reportAlias prints what the entry named by the first argument has done over
// the since window.
func reportAlias[T any](ctx context.Context, a *aliasRun, since time.Duration,
	print func(w *table, alias string, since time.Duration, res T),
) error {
	alias := a.fs.Arg(0)
	path := a.path(alias, "/report") + "&from=" + url.QueryEscape(sinceParam(since))
	return doOut(ctx, a.c, "GET", path, nil, a.asJSON, func(w *table, res T) {
		print(w, alias, since, res)
	})
}

// putAlias writes an entry and prints what was stored.
func putAlias[T any](ctx context.Context, a *aliasRun, alias string, body any,
	print func(*table, T),
) error {
	return doOut(ctx, a.c, "PUT", a.path(alias, ""), body, a.asJSON, print)
}

// deleteEntry deletes an entry and says so. Unless --yes was given it asks
// first, saying what goes with it in lines. Clients and guardrails name it by
// its alias, so the alias is what has to be typed back. --json prints the
// control API's answer unchanged.
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
