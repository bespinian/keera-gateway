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

// sessionsCmd is the panel's Sessions screen, in a terminal.
//
// It reports calls, minutes and cost per task rather than per request,
// because nobody decides how many calls a task takes. It sorts by cost by
// default: the row worth reading is the task that cost the most.
func sessionsCmd(ctx context.Context, args []string) error {
	c := newClient()
	fs := flag.NewFlagSet("sessions", flag.ExitOnError)
	org := fs.String("org", "", "restrict to one organisation")
	sort := fs.String("sort", "cost", "cost, requests, duration or recent")
	alias := fs.String("model", "", "restrict to the tasks that used one model")
	teamID := fs.String("team", "", "restrict to one team id")
	keyID := fs.String("key", "", "restrict to one key id")
	userID := fs.String("user", "", "restrict to one person's id")
	unhappy := fs.Bool("unhappy", false, "only the tasks that hit trouble")
	since := fs.Duration("since", 7*24*time.Hour, "how far back to look")
	limit := fs.Int("limit", 25, "how many to print")
	asJSON := fs.Bool("json", false, jsonUsage)
	fs.Usage = func() { _ = printHelp(fs, "sessions", "") }
	if want, ok := wantsHelp(args); ok {
		return printHelp(fs, "sessions", want)
	}
	if err := parse(fs, args); err != nil {
		return err
	}

	q := url.Values{}
	q.Set("sort", *sort)
	q.Set("limit", strconv.Itoa(*limit))
	q.Set("from", sinceParam(*since))
	if *unhappy {
		q.Set("unhappy", "1")
	}
	setIfGiven(q, map[string]string{
		"org_id": *org, "alias": *alias, "team_id": *teamID,
		"key_id": *keyID, "user_id": *userID,
	})

	var res sessionsResponse
	if err := c.do(ctx, "GET", "/v1/sessions?"+q.Encode(), nil, &res); err != nil {
		return err
	}
	return out(*asJSON, res, func(w *table) { printSessions(w, res, *since) })
}

type sessionsResponse struct {
	Data       []store.AgentSession     `json:"data"`
	Totals     store.AgentSessionTotals `json:"totals"`
	Currency   string                   `json:"currency"`
	GapSeconds int64                    `json:"gap_seconds"`
	KeyAliases map[string]string        `json:"key_aliases"`
	UserNames  map[string]string        `json:"user_names"`
}

func printSessions(w *table, res sessionsResponse, since time.Duration) {
	if len(res.Data) == 0 {
		_, _ = fmt.Fprintf(w, "no sessions in the last %s\n", since)
		return
	}
	w.header(fmt.Sprintf("SESSION\tSTARTED\tTOOK\tCALLS\tMODELS\tWHO\tCOST (%s)\tENDED",
		res.Currency))
	for _, s := range res.Data {
		who := res.UserNames[s.UserID]
		if who == "" {
			who = res.KeyAliases[s.KeyID]
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			s.Key, s.StartedAt.Local().Format("2006-01-02 15:04"),
			shortDuration(s.Duration()), s.Requests,
			dash(strings.Join(s.Models, ",")), dash(who),
			policy.FormatMicros(s.CostMicros), endedAs(s))
	}
	// Per task, and the middle task rather than the mean: one runaway task
	// makes an average that describes none of the others.
	t := res.Totals
	_, _ = fmt.Fprintf(w, "\n%d sessions over %d calls in the last %s; showing %d\n",
		t.Sessions, t.Requests, since, len(res.Data))
	_, _ = fmt.Fprintf(w, "middle task: %d calls, %s, %s %s\n",
		t.MedianRequests, shortDuration(time.Duration(t.MedianDurationMS)*time.Millisecond),
		policy.FormatMicros(t.MedianCostMicros), res.Currency)
	_, _ = fmt.Fprintf(w, "worst task: %d calls, %s %s; %d of %d hit trouble\n",
		t.LongestRequests, policy.FormatMicros(t.CostliestMicros), res.Currency,
		t.Unhappy, t.Sessions)
	_, _ = fmt.Fprintf(w, "grouped by conversation, cut after %s idle\n",
		shortDuration(time.Duration(res.GapSeconds)*time.Second))
}

// sessionCmd is one task from start to end, named by any request in it.
func sessionCmd(ctx context.Context, args []string) error {
	c := newClient()
	fs := flag.NewFlagSet("session", flag.ExitOnError)
	org := fs.String("org", "", "the organisation the request belongs to")
	asJSON := fs.Bool("json", false, jsonUsage)
	fs.Usage = func() { _ = printHelp(fs, "session", "") }
	if want, ok := wantsHelp(args); ok {
		return printHelp(fs, "session", want)
	}
	if err := parseArgs(fs, args, 1, "usage: keera session <request-id>"); err != nil {
		return err
	}
	requestID, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil {
		return fmt.Errorf("a session is named by the id of one of its requests")
	}

	path := fmt.Sprintf("/v1/sessions/%d", requestID)
	if *org != "" {
		path += "?" + url.Values{"org_id": {*org}}.Encode()
	}
	var res sessionResponse
	if err := c.do(ctx, "GET", path, nil, &res); err != nil {
		return err
	}
	return out(*asJSON, res, func(w *table) { printSession(w, res) })
}

type sessionResponse struct {
	Session    store.AgentSession `json:"session"`
	Requests   []store.Request    `json:"requests"`
	Currency   string             `json:"currency"`
	KeyAliases map[string]string  `json:"key_aliases"`
	TeamNames  map[string]string  `json:"team_names"`
	UserNames  map[string]string  `json:"user_names"`
}

func printSession(w *table, res sessionResponse) {
	s := res.Session
	show(w, "session", s.Key)
	show(w, "grouping", groupedBy(s))
	show(w, "started", s.StartedAt.Local().Format(time.RFC3339))
	_, _ = fmt.Fprintf(w, "took\t%s over %d calls\n", shortDuration(s.Duration()), s.Requests)
	show(w, "models", dash(strings.Join(s.Models, ", ")))
	show(w, "key", dash(labelled(res.KeyAliases, s.KeyID)))
	show(w, "team", dash(labelled(res.TeamNames, s.TeamID)))
	show(w, "who", dash(labelled(res.UserNames, s.UserID)))
	_, _ = fmt.Fprintf(w, "tokens\t%d in, %d out\n", s.InputTokens, s.OutputTokens)
	_, _ = fmt.Fprintf(w, "cost\t%s %s\n", policy.FormatMicros(s.CostMicros), res.Currency)
	show(w, "ended", endedAs(s))
	if s.LastError != "" {
		show(w, "message", oneLine(s.LastError))
	}

	// Oldest first: a task that went wrong is read from its beginning.
	_, _ = fmt.Fprintf(w, "\nCALL\tWHEN\tMODEL\tSTATUS\tCOST (%s)\tFIRST TOKEN\tMESSAGE\n",
		res.Currency)
	for i, r := range res.Requests {
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%s\t%dms\t%s\n",
			i+1, r.TS.Local().Format("15:04:05"), dash(r.Alias), r.Status,
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
