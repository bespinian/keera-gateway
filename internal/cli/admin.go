package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/store"
)

// Flag descriptions that several commands share word for word.
const (
	orgUsage  = "organisation id (defaults to the only one, if there is only one)"
	jsonUsage = "print raw JSON"
	yesUsage  = "do not ask for confirmation"
)

// out renders v as a table via render, or as JSON when --json was given.
func out(asJSON bool, v any, render func(*table)) error {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	w := newTable(os.Stdout)
	render(w)
	return w.Flush()
}

// split takes the verb off a command's arguments. A leading flag means the
// verb was left out, which is a listing.
func split(args []string) (sub string, rest []string) {
	if len(args) == 0 {
		return "", nil
	}
	if strings.HasPrefix(args[0], "-") {
		return "list", args
	}
	return args[0], args[1:]
}

// parseArgs parses a verb's flags and checks that exactly n arguments are left.
func parseArgs(fs *flag.FlagSet, args []string, n int, usage string) error {
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() != n {
		return errors.New(usage)
	}
	return nil
}

// list reads one of the control API's {"data": [...]} listings.
func list[T any](ctx context.Context, c *client, path string) ([]T, error) {
	var res struct {
		Data []T `json:"data"`
	}
	err := c.do(ctx, "GET", path, nil, &res)
	return res.Data, err
}

// inOrg scopes a control API path to one organisation.
func inOrg(path, orgID string) string {
	return path + "?org_id=" + url.QueryEscape(orgID)
}

// sinceParam is the start of a report window, as the control API reads it.
func sinceParam(d time.Duration) string {
	return time.Now().Add(-d).UTC().Format(time.RFC3339)
}

// resolveOrg fills in the organisation when there is only one, which is the
// case in every dedicated and on-premises deployment.
func resolveOrg(ctx context.Context, c *client, given string) (string, error) {
	if given != "" {
		return given, nil
	}
	return theOnlyOrg(ctx, c, "pass --org <id>")
}

// theOnlyOrg is the organisation a command means when it was not told one.
// howToSay tells the caller how to name one instead, because not every
// command has a --org flag.
func theOnlyOrg(ctx context.Context, c *client, howToSay string) (string, error) {
	orgs, err := list[store.Org](ctx, c, "/v1/orgs")
	if err != nil {
		return "", err
	}
	switch len(orgs) {
	case 0:
		return "", fmt.Errorf("no organisation exists yet; create one with: keera org create <name>")
	case 1:
		return orgs[0].ID, nil
	default:
		return "", fmt.Errorf("several organisations exist; %s (see: keera org list)", howToSay)
	}
}

// textOrFile resolves a flag value that may name a file instead of carrying
// the text. Long text such as a system prompt loses its newlines when quoted
// through a shell.
func textOrFile(v string) (string, error) {
	if !strings.HasPrefix(v, "@") {
		return v, nil
	}
	// @- is stdin, so a credential stays out of shell history.
	if v == "@-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("reading stdin: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	b, err := os.ReadFile(v[1:])
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// setInt applies a flag that uses -1 for "not given" and 0 for "unlimited".
func setInt(dst **int, v int) {
	switch {
	case v < 0:
		return
	case v == 0:
		*dst = nil
	default:
		n := v
		*dst = &n
	}
}

// stringList is a flag that may be repeated and also takes comma-separated
// values, because people type both.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	for s := range strings.SplitSeq(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			*l = append(*l, s)
		}
	}
	return nil
}

// show writes one "name<tab>value" row, for the printers that describe one
// thing rather than list many.
func show(w *table, name string, v any) {
	_, _ = fmt.Fprintf(w, "%s\t%v\n", name, v)
}

// firstLine renders a multi-line value as the one line a table has room for.
func firstLine(s string) string {
	line, rest, cut := strings.Cut(s, "\n")
	if len(line) > 60 {
		line, cut = line[:59], true
	}
	if cut || strings.TrimSpace(rest) != "" {
		return strings.TrimRight(line, " ") + "…"
	}
	return line
}

// oneLine keeps a backend's message on its row. Some answer with a stack
// trace, and a table that reflows is not a table.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 100 {
		return s[:99] + "…"
	}
	if s == "" {
		return "-"
	}
	return s
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// plural renders a count with its noun, so a single one is not "1 rules".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// share renders one count as a percentage of another, which compares at a
// glance where "173 of 4,219" does not.
func share(part, whole int64) string {
	if whole <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", float64(part)/float64(whole)*100)
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func orUnlimited(p *int) any {
	if p == nil {
		return "(unlimited)"
	}
	return *p
}

// shortDuration is how long something ran, in the units a person would say.
// time.Duration's own string is "11m30.000000001s".
func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
