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
	r := newCmdRun("session", args, "orgs", "json")
	fs := r.fs
	sort := fs.String("sort", "cost", "cost, requests, duration or recent")
	alias := fs.String("model", "", "only the sessions that used one model")
	w := registerWho(fs)
	unhappy := fs.Bool("unhappy", false, "only the sessions with problems")
	since := fs.Duration("since", reportWindow, "how far back to look")
	limit := fs.Int("limit", reportLimit, "how many to print")
	if done, err := r.parse(); done {
		return err
	}
	c := r.c
	if r.verb == "show" {
		return sessionShow(ctx, c, fs.Arg(0), r.org, r.asJSON)
	}
	return sessionList(ctx, c, sessionQuery{org: r.org, sort: *sort, alias: *alias, who: w,
		unhappy: *unhappy, since: *since, limit: *limit}, r.asJSON)
}

type sessionQuery struct {
	org, sort, alias string
	who              *who
	unhappy          bool
	since            time.Duration
	limit            int
}

// sessionList reports calls, minutes and cost per task rather than per
// request, because nobody decides how many calls a task takes. It sorts by
// cost by default: the row worth reading is the task that cost the most.
func sessionList(ctx context.Context, c *client, sq sessionQuery, asJSON bool) error {
	q, err := reportQuery(ctx, c, sq.org, sq.who, sq.since, map[string]string{
		"sort": sq.sort, "limit": strconv.Itoa(sq.limit), "alias": sq.alias,
	})
	if err != nil {
		return err
	}
	if sq.unhappy {
		q.Set("unhappy", "1")
	}

	var res sessionsResponse
	if err := c.do(ctx, "GET", "/v1/sessions?"+q.Encode(), nil, &res); err != nil {
		return err
	}
	return out(asJSON, res, func(w *table) { printSessions(w, res, sq.since) })
}

type sessionsResponse struct {
	Data       []store.AgentSession     `json:"data"`
	Totals     store.AgentSessionTotals `json:"totals"`
	Currency   string                   `json:"currency"`
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
		res.Currency))
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
			policy.FormatMicros(s.CostMicros), endedAs(s))
	}
	// Per task, and the middle task rather than the mean: one runaway task
	// makes an average that describes none of the others.
	t := res.Totals
	_, _ = fmt.Fprintf(w, "\n%d sessions over %d requests in the last %s; showing %d\n",
		t.Sessions, t.Requests, shortDuration(since), len(res.Data))
	_, _ = fmt.Fprintf(w, "middle session: %d requests, %s, %s %s\n",
		t.MedianRequests, shortDuration(time.Duration(t.MedianDurationMS)*time.Millisecond),
		policy.FormatMicros(t.MedianCostMicros), res.Currency)
	_, _ = fmt.Fprintf(w, "worst session: %d requests, %s %s; %d of %d had problems\n",
		t.LongestRequests, policy.FormatMicros(t.CostliestMicros), res.Currency,
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

	path := fmt.Sprintf("/v1/sessions/%d", requestID)
	if org != "" {
		path = inOrg(path, org)
	}
	var res sessionResponse
	if err := c.do(ctx, "GET", path, nil, &res); err != nil {
		return err
	}
	return out(asJSON, res, func(w *table) { printSession(w, res) })
}

type sessionResponse struct {
	Session      store.AgentSession `json:"session"`
	Requests     []store.Request    `json:"requests"`
	Currency     string             `json:"currency"`
	KeyNames     map[string]string  `json:"key_names"`
	ProjectNames map[string]string  `json:"project_names"`
	UserNames    map[string]string  `json:"user_names"`
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
	_, _ = fmt.Fprintf(w, "cost\t%s %s\n", policy.FormatMicros(s.CostMicros), res.Currency)
	show(w, "ended", endedAs(s))
	if s.LastError != "" {
		show(w, "message", oneLine(s.LastError))
	}

	// Oldest first: a task that went wrong is read from its beginning.
	_, _ = fmt.Fprintln(w)
	w.header(fmt.Sprintf("REQUEST\tWHEN\tMODEL\tSTATUS\tCOST (%s)\tFIRST TOKEN\tMESSAGE", res.Currency))
	for _, r := range res.Requests {
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%dms\t%s\n",
			r.ID, r.TS.Local().Format("15:04:05"), dash(r.Alias), httpStatus(r.Status),
			policy.FormatMicros(r.CostMicros), r.TTFTMS, oneLine(r.Error))
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
