package cli

import (
	"cmp"
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

	"github.com/bespinian/keera-gateway/internal/id"
	"github.com/bespinian/keera-gateway/internal/store"
)

// Flag descriptions several commands share.
const (
	orgUsage  = "organisation, by name or id"
	jsonUsage = "print raw JSON"
	yesUsage  = "do not ask for confirmation"

	sinceUsage = "how far back to look"
	limitUsage = "how many to print"
	modelUsage = "restrict to one model"
)

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

func doOut[T any](ctx context.Context, c *client, method, path string, body any, asJSON bool,
	show func(*table, T),
) error {
	var res T
	if err := c.do(ctx, method, path, body, &res); err != nil {
		return err
	}
	return out(asJSON, res, func(w *table) { show(w, res) })
}

// split returns an empty verb when a flag comes first, which parseVerb reads
// as the listing.
func split(args []string) (sub string, rest []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", args
	}
	return args[0], args[1:]
}

// list reads a {"data": [...]} listing.
func list[T any](ctx context.Context, c *client, path string) ([]T, error) {
	var res struct {
		Data []T `json:"data"`
	}
	err := c.do(ctx, "GET", path, nil, &res)
	return res.Data, err
}

func inOrg(path, orgID string) string {
	return path + "?org_id=" + url.QueryEscape(orgID)
}

func sinceParam(d time.Duration) string {
	return time.Now().Add(-d).UTC().Format(time.RFC3339)
}

const reportWindow = 7 * 24 * time.Hour

const reportLimit = 50

// reportQuery leaves out empty values. w may be nil, for a report that takes
// no --project, --key or --user.
func reportQuery(ctx context.Context, c *client, org string, w *who, since time.Duration,
	extra map[string]string,
) (url.Values, error) {
	org, err := orgIfGiven(ctx, c, org)
	if err != nil {
		return nil, err
	}
	q := url.Values{"from": {sinceParam(since)}}
	setIfGiven(q, map[string]string{"org_id": org})
	setIfGiven(q, extra)
	if w == nil {
		return q, nil
	}
	params, err := w.params(ctx, c, org)
	if err != nil {
		return nil, err
	}
	setIfGiven(q, params)
	return q, nil
}

func setIfGiven(q url.Values, params map[string]string) {
	for name, v := range params {
		if v != "" {
			q.Set(name, v)
		}
	}
}

// errNotFound tells "not there" apart from "the control API is down".
var errNotFound = errors.New("not found")

type notFoundError string

func (e notFoundError) Error() string      { return string(e) }
func (notFoundError) Is(target error) bool { return target == errNotFound }

// notFound says what was looked for, where, and the command that lists what
// there is.
func notFound(noun, name, orgID, cmd string) error {
	where := ""
	if orgID != "" {
		where = " in " + orgID
	}
	return notFoundError(fmt.Sprintf("no %s %q%s (see: keera %s list)", noun, name, where, cmd))
}

// findIn matches by id, or else by name, ignoring case.
func findIn[T any](entries []T, given string, idName func(T) (string, string)) (T, bool) {
	given = strings.TrimSpace(given)
	for _, byName := range []bool{false, true} {
		for _, e := range entries {
			id, name := idName(e)
			if (!byName && id == given) || (byName && strings.EqualFold(name, given)) {
				return e, true
			}
		}
	}
	var zero T
	return zero, false
}

// findNamed lists path and picks an entry as findIn does. noun and cmd word
// the error when there is none.
func findNamed[T any](ctx context.Context, c *client, org, given, path, noun, cmd string,
	idName func(T) (string, string),
) (T, error) {
	var zero T
	orgID, err := resolveOrg(ctx, c, org)
	if err != nil {
		return zero, err
	}
	entries, err := list[T](ctx, c, inOrg(path, orgID))
	if err != nil {
		return zero, err
	}
	if e, ok := findIn(entries, given, idName); ok {
		return e, nil
	}
	return zero, notFound(noun, given, orgID, cmd)
}

// keepRaw also returns the raw answer, so --json prints everything, not only
// the fields a table shows.
func keepRaw(ctx context.Context, c *client, method, path string, body, v any) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.do(ctx, method, path, body, &raw); err != nil {
		return nil, err
	}
	return raw, json.Unmarshal(raw, v)
}

// printNone avoids a bare heading that looks like a broken command. add is the
// command that makes one.
func printNone(w io.Writer, what, add string) {
	if add == "" {
		_, _ = fmt.Fprintf(w, "No %s.\n", what)
		return
	}
	_, _ = fmt.Fprintf(w, "No %s. Add one with: %s\n", what, add)
}

// handOver prints the secret alone on stdout, so 'KEY=$(keera key create …)'
// captures it.
func handOver(secret, said string) {
	fmt.Println(secret)
	fmt.Fprintf(os.Stderr, "\n%s %s\n", said, styleErr.warn("This is the only time it is shown."))
}

// resolveOrg turns a name or id into an id. With none given, it is the only
// organisation, as in every dedicated and on-premises deployment.
func resolveOrg(ctx context.Context, c *client, given string) (string, error) {
	// An id needs no lookup.
	if strings.HasPrefix(given, "org_") {
		return given, nil
	}
	if given == "" {
		return theOnlyOrg(ctx, c, "choose one with: keera org switch <org>")
	}
	orgs, err := list[store.Org](ctx, c, "/v1/orgs")
	if err != nil {
		return "", err
	}
	if o, ok := findIn(orgs, given, func(o store.Org) (string, string) { return o.ID, o.Name }); ok {
		return o.ID, nil
	}
	return "", notFound("organisation", given, "", "org")
}

// orgIfGiven is resolveOrg for a report, which spans every organisation when
// none is given.
func orgIfGiven(ctx context.Context, c *client, given string) (string, error) {
	if given == "" {
		return "", nil
	}
	return resolveOrg(ctx, c, given)
}

// who narrows a report to one project or key by name, a person by email, or
// any of them by id.
type who struct{ project, key, user string }

func registerWho(fs *flag.FlagSet) *who {
	w := &who{}
	fs.StringVar(&w.project, "project", "", "restrict to one project, by name or id")
	fs.StringVar(&w.key, "key", "", "restrict to one key, by name or id")
	fs.StringVar(&w.user, "user", "", "restrict to one person, by email or id")
	return w
}

// params resolves names to ids. The organisation is only resolved when a name
// needs looking up, since a report may span every one.
func (w *who) params(ctx context.Context, c *client, org string) (map[string]string, error) {
	p := map[string]string{}
	orgID := org
	for _, f := range []struct {
		param, given string
		find         func(context.Context, *client, string, string) (string, error)
	}{
		{"project_id", w.project, projectID},
		{"key_id", w.key, keyID},
		{"user_id", w.user, userID},
	} {
		if f.given == "" {
			continue
		}
		var err error
		if orgID == "" {
			if orgID, err = resolveOrg(ctx, c, org); err != nil {
				return nil, err
			}
		}
		if p[f.param], err = f.find(ctx, c, orgID, f.given); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// projectID, keyID and userID pass an id on unchanged, with no lookup.
func projectID(ctx context.Context, c *client, org, given string) (string, error) {
	if given == "" || id.HasPrefix(given, "project") {
		return given, nil
	}
	t, err := findProject(ctx, c, org, given)
	return t.ID, err
}

func keyID(ctx context.Context, c *client, org, given string) (string, error) {
	if given == "" || id.HasPrefix(given, "key") {
		return given, nil
	}
	k, err := findKey(ctx, c, org, given)
	return k.ID, err
}

func userID(ctx context.Context, c *client, org, given string) (string, error) {
	if given == "" || id.HasPrefix(given, "user") {
		return given, nil
	}
	u, err := findUser(ctx, c, org, given)
	return u.ID, err
}

// theOnlyOrg is used when a command was not told one. howToSay tells the
// caller how to name one instead.
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

// textOrFile reads @path as a file, because long text loses its newlines when
// quoted through a shell.
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

// setInt treats -1 as "not given" and 0 as clear.
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

// stringList may be repeated and takes comma-separated values, because people
// type both.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	*l = append(*l, splitList(v)...)
	return nil
}

// splitList drops spaces and empty items, so "a, b" means "a,b".
func splitList(v string) []string {
	var items []string
	for s := range strings.SplitSeq(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			items = append(items, s)
		}
	}
	return items
}

// show writes one "name<tab>value" row.
func show(w *table, name string, v any) {
	_, _ = fmt.Fprintf(w, "%s\t%v\n", name, v)
}

func showIfSet(w *table, name, v string) {
	if v != "" {
		show(w, name, v)
	}
}

func firstLine(s string) string {
	line, rest, cut := strings.Cut(s, "\n")
	if r := []rune(line); len(r) > 60 {
		line, cut = string(r[:59]), true
	}
	if cut || strings.TrimSpace(rest) != "" {
		return strings.TrimRight(line, " ") + "…"
	}
	return line
}

// oneLine keeps a backend's message on its row, since some send a stack
// trace.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	// By character, so a multi-byte one is never split.
	if r := []rune(s); len(r) > 100 {
		return string(r[:99]) + "…"
	}
	return dash(s)
}

func dash(s string) string { return cmp.Or(s, "-") }

func yesNo(b bool) string {
	if b {
		return statusWord("yes")
	}
	return statusWord("no")
}

// plural takes an irregular plural too: plural(n, "sandbox", "sandboxes").
func plural(n int, noun string, many ...string) string {
	if n == 1 {
		return "1 " + noun
	}
	if len(many) > 0 {
		return strconv.Itoa(n) + " " + many[0]
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// share renders a percentage, which compares at a glance where "173 of 4,219"
// does not.
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

// shortDuration avoids time.Duration's "11m30.000000001s" and "168h0m0s".
func shortDuration(d time.Duration) string {
	const day = 24 * time.Hour
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d >= 2*day && d%day == 0:
		return fmt.Sprintf("%dd", d/day)
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
