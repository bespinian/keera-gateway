package cli

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

func usageCmd(ctx context.Context, args []string) error {
	c := newClient()
	fs := flag.NewFlagSet("usage", flag.ExitOnError)
	org := fs.String("org", "", "restrict to one organisation")
	groupBy := fs.String("by", "model", "group by: model, client, team, key, user, day or org")
	since := fs.Duration("since", 30*24*time.Hour, "how far back to report")
	asJSON := fs.Bool("json", false, jsonUsage)
	fs.Usage = func() { _ = printHelp(fs, "usage", "") }
	if want, ok := wantsHelp(args); ok {
		return printHelp(fs, "usage", want)
	}
	if err := parse(fs, args); err != nil {
		return err
	}

	path := fmt.Sprintf("/v1/usage?group_by=%s&from=%s", *groupBy, sinceParam(*since))
	if *org != "" {
		path += "&org_id=" + *org
	}
	var res struct {
		Currency string              `json:"currency"`
		Data     []store.UsageBucket `json:"data"`
	}
	if err := c.do(ctx, "GET", path, nil, &res); err != nil {
		return err
	}
	return out(*asJSON, res, func(w *table) {
		w.header(fmt.Sprintf("%s\tREQUESTS\tIN\tOUT\tCOST (%s)",
			strings.ToUpper(*groupBy), res.Currency))
		var totalCost int64
		for _, b := range res.Data {
			group := b.Group
			if group == "" {
				group = "(none)"
			}
			_, _ = fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%s\n", group, b.Requests,
				b.InputTokens, b.OutputTokens, policy.FormatMicros(b.CostMicros))
			totalCost += b.CostMicros
		}
		_, _ = fmt.Fprintf(w, "total\t\t\t\t%s\n", policy.FormatMicros(totalCost))
	})
}

// failuresCmd is the panel's Failures screen, in a terminal.
//
// The backend's message is printed beside the model and the key, because only
// its wording says whose problem a failure is.
func failuresCmd(ctx context.Context, args []string) error {
	c := newClient()
	fs := flag.NewFlagSet("failures", flag.ExitOnError)
	org := fs.String("org", "", "restrict to one organisation")
	kind := fs.String("kind", "failed", "failed, refused, interrupted or all")
	alias := fs.String("model", "", "restrict to one model")
	teamID := fs.String("team", "", "restrict to one team id")
	keyID := fs.String("key", "", "restrict to one key id")
	status := fs.Int("status", 0, "restrict to one status")
	since := fs.Duration("since", 7*24*time.Hour, "how far back to look")
	limit := fs.Int("limit", 50, "how many to print")
	asJSON := fs.Bool("json", false, jsonUsage)
	fs.Usage = func() { _ = printHelp(fs, "failures", "") }
	if want, ok := wantsHelp(args); ok {
		return printHelp(fs, "failures", want)
	}
	if err := parse(fs, args); err != nil {
		return err
	}
	if *kind == "all" {
		*kind = ""
	}

	q := url.Values{}
	q.Set("kind", *kind)
	q.Set("limit", strconv.Itoa(*limit))
	q.Set("from", sinceParam(*since))
	setIfGiven(q, map[string]string{
		"org_id": *org, "alias": *alias, "team_id": *teamID, "key_id": *keyID,
	})
	if *status != 0 {
		q.Set("status", strconv.Itoa(*status))
	}

	var res failuresResponse
	if err := c.do(ctx, "GET", "/v1/failures?"+q.Encode(), nil, &res); err != nil {
		return err
	}
	return out(*asJSON, res, func(w *table) { printFailures(w, res, *since) })
}

// setIfGiven sets each query parameter that has a value.
func setIfGiven(q url.Values, params map[string]string) {
	for name, v := range params {
		if v != "" {
			q.Set(name, v)
		}
	}
}

type failuresResponse struct {
	Data       []store.Failure     `json:"data"`
	Filters    store.FailureFacets `json:"filters"`
	KeyAliases map[string]string   `json:"key_aliases"`
}

func printFailures(w *table, res failuresResponse, since time.Duration) {
	if len(res.Data) == 0 {
		_, _ = fmt.Fprintf(w, "nothing matched in the last %s\n", since)
		return
	}
	w.header("WHEN\tMODEL\tSTATUS\tKEY\tMESSAGE")
	for _, f := range res.Data {
		key := f.KeyID
		if alias, ok := res.KeyAliases[f.KeyID]; ok && alias != "" {
			key = alias
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			f.TS.Local().Format("2006-01-02 15:04:05"), dash(f.Alias),
			httpStatus(f.Status), dash(key), oneLine(f.Error))
	}
	// The counts cover the whole window, not just the page printed.
	if res.Filters.Total > int64(len(res.Data)) {
		_, _ = fmt.Fprintf(w, "\n%d in the last %s; showing %d\n",
			res.Filters.Total, since, len(res.Data))
	}
	if len(res.Filters.Models) > 1 {
		parts := make([]string, 0, len(res.Filters.Models))
		for _, a := range res.Filters.Models {
			parts = append(parts, fmt.Sprintf("%s %d", a.Value, a.Count))
		}
		_, _ = fmt.Fprintf(w, "by model: %s\n", strings.Join(parts, ", "))
	}
}

// httpStatus is a response status, painted by whose problem it is: a 5xx is
// the deployment's, a 4xx the caller's.
func httpStatus(code int) string {
	s := strconv.Itoa(code)
	switch {
	case code >= 500:
		return style.bad(s)
	case code >= 400:
		return style.warn(s)
	}
	return s
}
