package cli

// What the commands for entries named by an alias share: 'model', 'mcp',
// 'filter' and 'router'. Each kind is a short list in one organisation, read
// whole, because the control API has no endpoint for a single entry.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// aliasPath is a control API path for one entry of kind, such as "filters",
// in orgID. suffix addresses a route under it, such as "/check".
func aliasPath(kind, orgID, alias, suffix string) string {
	return inOrg("/v1/"+kind+"/"+url.PathEscape(alias)+suffix, orgID)
}

// findAlias reads the entry called alias from orgID's list of kind. noun
// names the kind of entry, for the error; its first word is the command that
// lists them, as in "MCP server" and 'keera mcp list'.
func findAlias[T any](ctx context.Context, c *client, kind, orgID, alias, noun string,
	aliasOf func(T) string,
) (T, error) {
	var zero T
	entries, err := list[T](ctx, c, inOrg("/v1/"+kind, orgID))
	if err != nil {
		return zero, err
	}
	for _, e := range entries {
		if aliasOf(e) == alias {
			return e, nil
		}
	}
	return zero, notFound(noun, alias, orgID, commandOf(noun))
}

// alreadyExists refuses an 'add' that would quietly replace the entry of the
// same alias. Changing one is what 'set' is for.
func alreadyExists[T any](ctx context.Context, c *client, kind, orgID, alias, noun string,
	aliasOf func(T) string,
) error {
	_, err := findAlias(ctx, c, kind, orgID, alias, noun, aliasOf)
	switch {
	case err == nil:
		return fmt.Errorf("the %s %s already exists; change it with: keera %s set %s",
			noun, alias, commandOf(noun), alias)
	case errors.Is(err, errNotFound):
		return nil
	}
	return err
}

// commandOf is the command that manages a kind of entry: the first word of
// its noun.
func commandOf(noun string) string {
	return strings.ToLower(strings.Fields(noun)[0])
}

// deleteAlias deletes the model, MCP server, filter or router at path and says
// so. --json prints the control API's answer unchanged.
func deleteAlias(ctx context.Context, c *client, path, alias string, asJSON bool) error {
	var res map[string]any
	if err := c.do(ctx, "DELETE", path, nil, &res); err != nil {
		return err
	}
	return out(asJSON, res, func(w *table) {
		_, _ = fmt.Fprintf(w, "deleted\t%s\n", alias)
	})
}

// confirmAliasDelete asks before removing an entry. Clients and guardrails
// name it by its alias, so the alias is what has to be typed back.
func confirmAliasDelete(noun, alias string, lines []string) error {
	return confirm("Deleting the "+noun+" "+alias+":", lines, "alias", alias, "nothing was deleted")
}

// checkProbe runs the check at path and prints what came back. A check that
// did not pass fails the command, so a pipeline can gate on it.
func checkProbe[T any](ctx context.Context, c *client, path, what string, asJSON bool,
	print func(*table, T), passed func(T) bool,
) error {
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

// aliasRun is what one 'keera filter' or 'keera router' invocation shares
// with the other: the flags every verb of both reads, and the organisation.
type aliasRun struct {
	c     *client
	fs    *flag.FlagSet
	cmd   string
	sub   string
	org   string
	orgID string
	since time.Duration
	yes   bool
	// asJSON prints the control API's answer instead of a table.
	asJSON bool
}

// newAliasRun takes the verb off args and declares the flags both commands
// share. The caller declares its own, then calls parse.
func newAliasRun(cmd string, args []string) (*aliasRun, []string) {
	sub, rest := split(args)
	fs := flag.NewFlagSet(cmd+" "+sub, flag.ExitOnError)
	a := &aliasRun{c: newClient(), fs: fs, cmd: cmd, sub: sub}
	fs.StringVar(&a.org, "org", "", orgUsage)
	fs.DurationVar(&a.since, "since", reportWindow, "how far back 'report' looks")
	fs.BoolVar(&a.yes, "yes", false, yesUsage)
	fs.BoolVar(&a.asJSON, "json", false, jsonUsage)
	fs.Usage = func() { _ = printHelp(fs, cmd, sub) }
	return a, rest
}

// parse reads the verb and its flags. An empty verb with no error means help
// was asked for and printed.
func (a *aliasRun) parse(args, rest []string) (string, error) {
	if want, ok := wantsHelp(args); ok {
		return "", printHelp(a.fs, a.cmd, want)
	}
	verb, err := parseVerb(a.fs, a.cmd, a.sub, rest)
	a.sub = verb
	return verb, err
}

// kind is the control API's name for the list, such as "filters".
func (a *aliasRun) kind() string { return a.cmd + "s" }

// path addresses one entry, or with a suffix a route under it.
func (a *aliasRun) path(alias, suffix string) string {
	return aliasPath(a.kind(), a.orgID, alias, suffix)
}

// aliasVerbs is what 'filter' and 'router' each do their own way.
type aliasVerbs interface {
	list(context.Context) error
	put(context.Context) error
	check(context.Context) error
	report(context.Context) error
	delete(context.Context) error
}

// run resolves the organisation, then runs the verb.
func (a *aliasRun) run(ctx context.Context, verb string, v aliasVerbs) (err error) {
	if a.orgID, err = resolveOrg(ctx, a.c, a.org); err != nil {
		return err
	}
	switch verb {
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

// listAliases prints every entry of the command's kind, or says there are
// none.
func listAliases[T any](ctx context.Context, a *aliasRun, print func(*table, []T)) error {
	entries, err := list[T](ctx, a.c, inOrg("/v1/"+a.kind(), a.orgID))
	if err != nil {
		return err
	}
	return out(a.asJSON, entries, func(w *table) {
		if len(entries) == 0 {
			printNone(w, a.kind(), "keera "+a.cmd+" add <alias>")
			return
		}
		print(w, entries)
	})
}

// reportAlias prints what the entry named by the first argument has done over
// the --since window.
func reportAlias[T any](ctx context.Context, a *aliasRun,
	print func(w *table, alias string, since time.Duration, res T),
) error {
	alias := a.fs.Arg(0)
	path := a.path(alias, "/report") + "&from=" + url.QueryEscape(sinceParam(a.since))
	var res T
	if err := a.c.do(ctx, "GET", path, nil, &res); err != nil {
		return err
	}
	return out(a.asJSON, res, func(w *table) { print(w, alias, a.since, res) })
}

// putAlias writes an entry and prints what was stored.
func putAlias[T any](ctx context.Context, a *aliasRun, alias string, body map[string]any,
	print func(*table, T),
) error {
	var saved T
	if err := a.c.do(ctx, "PUT", a.path(alias, ""), body, &saved); err != nil {
		return err
	}
	return out(a.asJSON, saved, func(w *table) { print(w, saved) })
}

// deleteEntry asks unless --yes was given, then deletes the entry.
func (a *aliasRun) deleteEntry(ctx context.Context, alias string, lines []string) error {
	if !a.yes {
		if err := confirmAliasDelete(a.cmd, alias, lines); err != nil {
			return err
		}
	}
	return deleteAlias(ctx, a.c, a.path(alias, ""), alias, a.asJSON)
}
