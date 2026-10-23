package cli

import (
	"cmp"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// sessionCmd is the panel's Sessions screen, in a terminal: 'list' ranks the
// tasks, 'show' opens one.
func sessionCmd(ctx context.Context, args []string) error {
	// 'keera session <id>' opened a task before 'show' existed, and a row's id
	// pasted after 'session' should still open it.
	if len(args) > 0 {
		if _, err := strconv.ParseInt(args[0], 10, 64); err == nil {
			args = append([]string{"show"}, args...)
		}
	}
	r := newCmdRun("session", args)
	fs := r.fs
	sort := fs.String("sort", "cost", "cost, tokens, requests, duration or recent")
	alias := fs.String("model", "", modelUsage)
	w := registerWho(fs)
	unhappy := fs.Bool("unhappy", false, "only the sessions with problems")
	since := fs.Duration("since", reportWindow, sinceUsage)
	limit := fs.Int("limit", reportLimit, limitUsage)
	if done, err := r.parse(); done {
		return err
	}
	c := r.c
	if r.verb == "show" {
		return sessionShow(ctx, c, fs.Arg(0), r.org, r.asJSON)
	}

	// The list reports calls, minutes and cost per task rather than per
	// request, because nobody decides how many calls a task takes. It sorts by
	// cost by default: the row worth reading is the task that cost the most.
	q, err := reportQuery(ctx, c, r.org, w, *since, map[string]string{
		"sort": *sort, "limit": strconv.Itoa(*limit), "alias": *alias,
	})
	if err != nil {
		return err
	}
	if *unhappy {
		q.Set("outcome", string(store.OutcomeUnhappy))
	}
	return doOut(ctx, c, "GET", "/v1/sessions?"+q.Encode(), nil, r.asJSON,
		func(t *table, res sessionsResponse) { printSessions(t, res, *since) })
}

type sessionsResponse struct {
	Data       []store.AgentSession     `json:"data"`
	Totals     store.AgentSessionTotals `json:"totals"`
	GapSeconds int64                    `json:"gap_seconds"`
	KeyNames   map[string]string        `json:"key_names"`
	UserNames  map[string]string        `json:"user_names"`
}

func printSessions(w *table, res sessionsResponse, since time.Duration) {
	if len(res.Data) == 0 {
		printNone(w, "sessions in the last "+shortDuration(since), "")
		return
	}
	w.header(fmt.Sprintf("SESSION\tSTARTED\tTOOK\tREQUESTS\tMODELS\tWHO\tCOST (%s)\tENDED",
		policy.Currency))
	for _, s := range res.Data {
		who := res.UserNames[s.UserID]
		if who == "" {
			who = res.KeyNames[s.KeyID]
		}
		// The id, not the conversation hash: it is what `keera session show` takes.
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			s.ID, s.StartedAt.Local().Format("2006-01-02 15:04"),
			shortDuration(s.Duration()), s.Requests,
			dash(strings.Join(s.Models, ",")), dash(who),
			policy.FormatAmount(s.CostMicros), endedAs(s))
	}
	// Per task, and the middle task rather than the mean: one runaway task
	// makes an average that describes none of the others.
	t := res.Totals
	_, _ = fmt.Fprintf(w, "\n%d sessions over %d requests in the last %s; showing %d\n",
		t.Sessions, t.Requests, shortDuration(since), len(res.Data))
	_, _ = fmt.Fprintf(w, "middle session: %d requests, %s, %s\n",
		t.RequestsMedian, shortDuration(time.Duration(t.DurationMedianMS)*time.Millisecond),
		policy.FormatMoney(t.CostMedianMicros))
	_, _ = fmt.Fprintf(w, "worst session: %d requests, %s; %d of %d had problems\n",
		t.LongestRequests, policy.FormatMoney(t.CostliestMicros),
		t.Unhappy, t.Sessions)
	_, _ = fmt.Fprintf(w, "grouped by conversation, cut after %s idle\n",
		shortDuration(time.Duration(res.GapSeconds)*time.Second))
}

// sessionShow is one task from start to end, named by any request in it.
func sessionShow(ctx context.Context, c *client, arg, org string, asJSON bool) error {
	requestID, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		return fmt.Errorf("a session is named by the id of one of its requests")
	}

	org, err = orgIfGiven(ctx, c, org)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/v1/sessions/%d", requestID)
	if org != "" {
		path = inOrg(path, org)
	}
	return doOut(ctx, c, "GET", path, nil, asJSON, printSession)
}

type sessionResponse struct {
	Session      store.AgentSession  `json:"session"`
	Requests     []store.Request     `json:"requests"`
	ToolCalls    []store.ToolCallRow `json:"tool_calls"`
	GapSeconds   int64               `json:"gap_seconds"`
	KeyNames     map[string]string   `json:"key_names"`
	ProjectNames map[string]string   `json:"project_names"`
	UserNames    map[string]string   `json:"user_names"`
}

func printSession(w *table, res sessionResponse) {
	s := res.Session
	show(w, "session", strconv.FormatInt(s.ID, 10))
	show(w, "grouping", groupedBy(s))
	show(w, "started", s.StartedAt.Local().Format(time.RFC3339))
	_, _ = fmt.Fprintf(w, "took\t%s over %d requests\n", shortDuration(s.Duration()), s.Requests)
	show(w, "models", dash(strings.Join(s.Models, ", ")))
	show(w, "key", dash(labelled(res.KeyNames, s.KeyID)))
	show(w, "project", dash(labelled(res.ProjectNames, s.ProjectID)))
	show(w, "who", dash(labelled(res.UserNames, s.UserID)))
	_, _ = fmt.Fprintf(w, "tokens\t%d in, %d out\n", s.InputTokens, s.OutputTokens)
	show(w, "cost", policy.FormatMoney(s.CostMicros))
	show(w, "ended", endedAs(s))
	if s.LastError != "" {
		show(w, "message", oneLine(s.LastError))
	}

	// Oldest first: a task that went wrong is read from its beginning.
	_, _ = fmt.Fprintln(w)
	w.header(fmt.Sprintf("REQUEST\tWHEN\tMODEL\tSTATUS\tCOST (%s)\tFIRST TOKEN\tMESSAGE", policy.Currency))
	for _, r := range res.Requests {
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%dms\t%s\n",
			r.ID, r.CreatedAt.Local().Format("15:04:05"), dash(r.Alias), httpStatus(r.Status),
			policy.FormatAmount(r.CostMicros), r.TTFTMS, oneLine(cmp.Or(r.Error, r.Note)))
	}

	// What the agent did between its requests, through the MCP servers.
	if len(res.ToolCalls) > 0 {
		_, _ = fmt.Fprintln(w)
		printToolCalls(w, res.ToolCalls)
	}
}

// endedAs says how a task finished, in the panel's words.
func endedAs(s store.AgentSession) string {
	switch {
	case s.LastStatus >= 500:
		return "failed"
	case s.LastStatus >= 400:
		return "refused"
	case s.LastError != "":
		return "interrupted"
	case s.Failed+s.Refused+s.Interrupted > 0:
		// It finished, but not everything in it did: the agent recovered.
		return "served, after trouble"
	default:
		return "served"
	}
}

// groupedBy says whether the client named the task or it was inferred, which
// is how far to trust the grouping.
func groupedBy(s store.AgentSession) string {
	if s.Stated {
		return "an id the client sent"
	}
	return "a hash of the prompt that opened the task"
}

func labelled(names map[string]string, id string) string {
	if id == "" {
		return ""
	}
	if name := names[id]; name != "" {
		return name
	}
	return id
}
