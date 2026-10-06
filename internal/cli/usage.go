package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

func usageCmd(ctx context.Context, args []string) error {
	r := newCmdRun("usage", args, "orgs", "json")
	fs := r.fs
	groupBy := fs.String("by", "model", "group by: model, client, project, key, user, day or org")
	since := fs.Duration("since", reportWindow, "how far back to report")
	if done, err := r.parse(); done {
		return err
	}
	c := r.c

	q, err := reportQuery(ctx, c, r.org, nil, *since, map[string]string{"group_by": *groupBy})
	if err != nil {
		return err
	}
	var res usageResponse
	if err := c.do(ctx, "GET", "/v1/usage?"+q.Encode(), nil, &res); err != nil {
		return err
	}
	return out(r.asJSON, res, func(w *table) {
		if len(res.Data) == 0 {
			printNone(w, "usage in the last "+shortDuration(*since), "")
			return
		}
		w.header(fmt.Sprintf("%s\tREQUESTS\tIN\tOUT\tCOST (%s)",
			strings.ToUpper(*groupBy), res.Currency))
		var totalCost int64
		for _, b := range res.Data {
			group := res.label(*groupBy, b.Group)
			if b.OrgID != "" {
				group += " (" + b.OrgID + ")"
			}
			_, _ = fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%s\n", group, b.Requests,
				b.InputTokens, b.OutputTokens, policy.FormatMicros(b.CostMicros))
			totalCost += b.CostMicros
		}
		_, _ = fmt.Fprintf(w, "total\t\t\t\t%s\n", policy.FormatMicros(totalCost))
	})
}

type usageResponse struct {
	Currency     string              `json:"currency"`
	Data         []store.UsageBucket `json:"data"`
	ProjectNames map[string]string   `json:"project_names"`
	KeyNames     map[string]string   `json:"key_names"`
	UserNames    map[string]string   `json:"user_names"`
	ClientNames  map[string]string   `json:"client_names"`
}

// label is what a group is called: a project's name, a key's name, a person's
// email, rather than the id the rows are grouped by.
func (res usageResponse) label(groupBy, group string) string {
	if group == "" {
		return "(none)"
	}
	names := map[string]map[string]string{
		"project": res.ProjectNames, "key": res.KeyNames,
		"user": res.UserNames, "client": res.ClientNames,
	}[groupBy]
	return labelled(names, group)
}

// failuresCmd lists the calls that did not deliver: the failed, refused and
// interrupted requests of the panel's request log, in a terminal.
//
// The backend's message is printed beside the model and the key, because only
// its wording says whose problem a failure is.
func failuresCmd(ctx context.Context, args []string) error {
	r := newCmdRun("failures", args, "orgs", "json")
	fs := r.fs
	kind := fs.String("kind", "failed", "failed, refused, interrupted or all")
	alias := fs.String("model", "", "restrict to one model")
	w := registerWho(fs)
	status := fs.Int("status", 0, "restrict to one status")
	since := fs.Duration("since", reportWindow, "how far back to look")
	limit := fs.Int("limit", reportLimit, "how many to print")
	if done, err := r.parse(); done {
		return err
	}
	c := r.c
	// 'all' is every request that did not deliver, which the request log
	// calls unhappy.
	if *kind == "all" {
		*kind = string(store.OutcomeUnhappy)
	}
	q, err := reportQuery(ctx, c, r.org, w, *since, map[string]string{
		"outcome": *kind, "facets": "1", "limit": strconv.Itoa(*limit), "alias": *alias,
	})
	if err != nil {
		return err
	}
	if *status != 0 {
		q.Set("status", strconv.Itoa(*status))
	}

	var res failuresResponse
	if err := c.do(ctx, "GET", "/v1/requests?"+q.Encode(), nil, &res); err != nil {
		return err
	}
	return out(r.asJSON, res, func(w *table) { printFailures(w, res, store.Outcome(*kind), *since) })
}

type failuresResponse struct {
	Data     []store.Request       `json:"data"`
	Outcomes store.RequestOutcomes `json:"outcomes"`
	Filters  store.RequestFacets   `json:"filters"`
	KeyNames map[string]string     `json:"key_names"`
}

// matched is how many requests of the outcome asked for are in the window.
func (r failuresResponse) matched(o store.Outcome) int64 {
	switch o {
	case store.OutcomeFailed:
		return r.Outcomes.Failed
	case store.OutcomeRefused:
		return r.Outcomes.Refused
	case store.OutcomeInterrupted:
		return r.Outcomes.Interrupted
	}
	return r.Outcomes.Failed + r.Outcomes.Refused + r.Outcomes.Interrupted
}

func printFailures(w *table, res failuresResponse, o store.Outcome, since time.Duration) {
	if len(res.Data) == 0 {
		what := string(o) + " requests"
		if o == store.OutcomeUnhappy {
			what = "failed, refused or interrupted requests"
		}
		printNone(w, what+" in the last "+shortDuration(since), "")
		return
	}
	// The request id opens the whole task: keera session show <id>.
	w.header("REQUEST\tWHEN\tMODEL\tSTATUS\tKEY\tMESSAGE")
	for _, f := range res.Data {
		key := f.KeyID
		if name, ok := res.KeyNames[f.KeyID]; ok && name != "" {
			key = name
		}
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n",
			f.ID, f.TS.Local().Format("2006-01-02 15:04:05"), dash(f.Alias),
			httpStatus(f.Status), dash(key), oneLine(f.Error))
	}
	// The counts cover the whole window, not just the page printed.
	if total := res.matched(o); total > int64(len(res.Data)) {
		_, _ = fmt.Fprintf(w, "\n%d in the last %s; showing %d\n", total, shortDuration(since), len(res.Data))
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
