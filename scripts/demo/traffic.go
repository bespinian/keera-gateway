package main

import (
	"context"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// member is one developer, with the coding agent they use and the models or
// routers it names.
type member struct {
	name, project, client string
	admin                 bool
	uses                  []weighted
	// sessions is how many tasks they give their agent on a working day.
	sessions float64
}

type weighted struct {
	alias  string
	weight float64
}

var team = []member{
	{
		name: "Anna Meier", project: "payments", client: "claude-code", admin: true, sessions: 5,
		uses: []weighted{{"auto", 8}, {"claude-opus-5-5", 2}},
	},
	{
		name: "Luca Rossi", project: "payments", client: "claude-code", sessions: 6,
		uses: []weighted{{"auto", 1}},
	},
	{
		name: "Noah Keller", project: "payments", client: "codex", sessions: 4,
		uses: []weighted{{"auto", 7}, {"keera-speed", 3}},
	},
	{
		name: "Sophie Müller", project: "customer-portal", client: "claude-code", sessions: 5,
		uses: []weighted{{"qwen3-coder-next", 7}, {"swiss-first", 2}, {"keera-speed", 1}},
	},
	{
		name: "Jonas Weber", project: "customer-portal", client: "opencode", sessions: 4,
		uses: []weighted{{"qwen3-coder-next", 5}, {"codestral-2508", 5}},
	},
	{
		name: "Mia Schneider", project: "customer-portal", client: "pi", sessions: 3,
		uses: []weighted{{"swiss-first", 6}, {"keera-speed", 4}},
	},
	{
		name: "Lea Brunner", project: "support-assistant", client: "claude-code", sessions: 3,
		uses: []weighted{{"qwen3-coder-next", 1}},
	},
	{
		name: "Nina Frei", project: "data-science", client: "codex", sessions: 3,
		uses: []weighted{{"codestral-2508", 6}, {"qwen3-coder-next", 4}},
	},
}

// email folds the name into an address: Sophie Müller is sophie.mueller.
func (m member) email(domain string) string {
	local := strings.NewReplacer(" ", ".", "ä", "ae", "ö", "oe", "ü", "ue").
		Replace(strings.ToLower(m.name))
	return local + "@" + domain
}

func (m member) keyName() string {
	first, _, _ := strings.Cut(m.name, " ")
	return first + "'s laptop"
}

// services are the applications with keys of their own.
var services = []struct{ name, project, client string }{
	{supportBotKey, "support-assistant", "python"},
	{ciKey, "payments", "curl"},
	{notebooksKey, "data-science", "python"},
}

const (
	supportBotKey = "support-bot"
	ciKey         = "ci-review"
	notebooksKey  = "notebooks"
)

// speed is how a model answers: time to the first token, the prompt tokens
// it reads per millisecond on top, and the tokens it writes per second. When
// its provider has an incident, it answers status and reason instead.
type speed struct {
	ttft          time.Duration
	prefillPerMS  float64
	tokensPerSec  float64
	status        int
	failureReason string
}

var speeds = map[string]speed{
	localModel:        {250 * time.Millisecond, 40, 140, 0, ""},
	"claude-opus-5-5": {1100 * time.Millisecond, 60, 55, 529, "Overloaded"},
	"qwen3-coder-next": {
		700 * time.Millisecond, 25, 75, 503,
		"upstream connect error or disconnect/reset before headers. reset reason: connection timeout",
	},
	"codestral-2508": {500 * time.Millisecond, 30, 95, 0, ""},
	"bge-m3": {
		60 * time.Millisecond, 200, 0, 503,
		"upstream connect error or disconnect/reset before headers. reset reason: connection timeout",
	},
}

// incident is a while in which a provider failed many of its requests. Apart
// from them, nothing fails: an outage is the exception, not the norm.
type incident struct {
	from, to time.Time
	models   []string
	share    float64 // of their requests that failed
	// who is working on target when it starts, so it is seen whatever else
	// was going on.
	who, target string
}

// planIncidents puts two short outages into the window, on weekdays: one at
// stepping stone, which swiss-first rides out by falling back to the EU, and
// one at Anthropic.
func (t *traffic) planIncidents(first time.Time, days float64) {
	plan := []struct {
		at                    float64 // how far into the window
		hour, minute, minutes int
		models                []string
		share                 float64
		who, target           string
	}{
		{
			0.4, 14, 5, 45,
			[]string{"qwen3-coder-next", "bge-m3"},
			0.7,
			"Mia Schneider", "swiss-first",
		},
		{0.75, 10, 20, 30, []string{"claude-opus-5-5"}, 0.4, "Anna Meier", "claude-opus-5-5"},
	}
	for _, p := range plan {
		day := first.AddDate(0, 0, int(days*p.at))
		for day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			day = day.AddDate(0, 0, 1)
		}
		from := time.Date(day.Year(), day.Month(), day.Day(), p.hour, p.minute, 0, 0, t.zone)
		if from.After(t.now) {
			continue
		}
		t.incidents = append(t.incidents, incident{
			from, from.Add(time.Duration(p.minutes) * time.Minute), p.models, p.share,
			p.who, p.target,
		})
	}
}

// fails says whether a request to alias at this moment failed.
func (t *traffic) fails(alias string, at time.Time) bool {
	for _, in := range t.incidents {
		if !at.Before(in.from) && at.Before(in.to) && slices.Contains(in.models, alias) {
			return t.chance(in.share)
		}
	}
	return false
}

// filter is one filter a request passes through, as the guardrails set it.
type filter struct {
	alias  string
	mode   policy.FilterMode
	shadow bool
	by     string // who set it, in the words of a refusal
}

var (
	orgFilter      = filter{"redact-secrets", policy.FilterModePattern, false, "your organisation"}
	projectFilters = map[string][]filter{
		"payments":          {{"no-client-data", policy.FilterModeGate, false, "your project"}},
		"support-assistant": {{"redact-names", policy.FilterModeRewrite, true, "your project"}},
	}
)

// actor is a key and what it belongs to.
type actor struct {
	keyID, projectID, userID, client, project string
}

// traffic makes the events. Each request is built as the gateway would have
// recorded it, steps and all.
type traffic struct {
	rnd    *rand.Rand
	org    string
	models map[string]policy.Model
	now    time.Time
	zone   *time.Location
	// dailyKey is the one key with a budget per day.
	dailyKey  string
	incidents []incident
	events    []store.Event
}

func (d *demo) traffic(ctx context.Context, start time.Time) (requests int, cost int64, err error) {
	list, err := d.st.ListModels(ctx, d.orgID)
	if err != nil {
		return 0, 0, err
	}
	models := map[string]policy.Model{}
	for _, m := range list {
		models[m.Alias] = m
	}
	zone, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		zone = time.Local
	}
	t := &traffic{
		rnd:      rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())),
		org:      d.orgID,
		models:   models,
		now:      time.Now(),
		zone:     zone,
		dailyKey: d.keyIDs[notebooksKey],
	}
	members := make([]actor, len(team))
	for i, m := range team {
		email := m.email(d.domain)
		members[i] = actor{
			d.keyIDs[m.keyName()], d.projectIDs[m.project], d.userIDs[email], m.client, m.project,
		}
	}
	svc := map[string]actor{}
	for _, s := range services {
		svc[s.name] = actor{d.keyIDs[s.name], d.projectIDs[s.project], "", s.client, s.project}
	}

	first := start.In(zone)
	first = time.Date(first.Year(), first.Month(), first.Day(), 0, 0, 0, 0, zone)
	total := t.now.Sub(first).Hours() / 24
	t.planIncidents(first, total)
	for day := first; day.Before(t.now); day = day.AddDate(0, 0, 1) {
		// Adoption grows over the month, as it does after a rollout.
		growth := 0.55 + 0.45*day.Sub(first).Hours()/24/total
		weekend := day.Weekday() == time.Saturday || day.Weekday() == time.Sunday
		for i, m := range team {
			t.developer(m, members[i], day, weekend, growth)
		}
		for _, in := range t.incidents {
			if i := slices.IndexFunc(team, func(m member) bool { return m.name == in.who }); i >= 0 &&
				in.from.Year() == day.Year() && in.from.YearDay() == day.YearDay() {
				t.session(team[i], members[i], in.from.Add(3*time.Minute), in.target)
			}
		}
		t.supportBot(svc[supportBotKey], day, weekend, growth)
		t.ci(svc[ciKey], day, weekend, growth)
		t.notebooks(svc[notebooksKey], day, weekend)
	}

	// The request log lists by id, so rows go in in the order they happened.
	events := slices.DeleteFunc(t.events, func(e store.Event) bool {
		return e.TS.Before(start) || e.TS.After(t.now)
	})
	slices.SortFunc(events, func(a, b store.Event) int { return a.TS.Compare(b.TS) })
	for batch := range slices.Chunk(events, 500) {
		if err := d.st.WriteEvents(ctx, batch); err != nil {
			return 0, 0, err
		}
	}
	for _, e := range events {
		if e.Tool == nil {
			requests++
			cost += e.CostMicros
		}
	}
	return requests, cost, nil
}

func (t *traffic) developer(m member, a actor, day time.Time, weekend bool, growth float64) {
	switch {
	case weekend:
		if t.chance(0.1) {
			t.session(m, a, t.clock(day, 10, 17), t.pick(m.uses))
		}
		return
	case t.chance(0.08): // out of the office
		return
	}
	for range t.poisson(m.sessions * growth) {
		t.session(m, a, t.workTime(day), t.pick(m.uses))
	}
}

// session is one task given to a coding agent: a run of calls whose prompt
// grows with every step, and the tools the agent calls in between.
func (t *traffic) session(m member, a actor, at time.Time, target string) {
	// Claude Code names its session in a header, so its tool calls can be
	// matched to it. The others are recognised by their opening prompt.
	stated := m.client == "claude-code"
	session := t.sessionKey(stated)
	prompt := t.jitter(18000, 0.3)
	for turn := range max(2, min(120, int(t.jitter(18, 0.7)))) {
		e := t.chat(a, call{
			target: target, at: at, session: session, turn: turn, stream: true,
			input: int(prompt), output: int(min(6000, t.jitter(380, 0.8))),
			interruptible: m.client == "claude-code",
		})
		at = at.Add(e.Latency + time.Duration(t.jitter(5000, 0.8))*time.Millisecond)
		prompt += t.jitter(3000, 0.7)
		if e.Canceled && t.chance(0.6) {
			return // the developer stopped the agent and started over
		}
		if stated && t.chance(0.15) {
			at = at.Add(t.toolCall(a, at, session).Latency)
		}
	}
}

// supportBot answers the chat on the support pages: one embedding to find the
// help articles, then a few turns with them in the prompt.
func (t *traffic) supportBot(a actor, day time.Time, weekend bool, growth float64) {
	chats := 140 * growth
	if weekend {
		chats = 45
	}
	for range t.poisson(chats) {
		at := t.clock(day, 7, 21)
		session := t.sessionKey(true)
		for turn := range 1 + t.rnd.IntN(4) {
			e := t.embed(a, at, 1, 120)
			at = at.Add(e.Latency)
			e = t.chat(a, call{
				target: "qwen3-coder-next", at: at, session: session, turn: turn, stream: true,
				input: int(t.jitter(3200, 0.4)) + turn*400, output: int(t.jitter(260, 0.5)),
			})
			at = at.Add(e.Latency + time.Duration(t.jitter(40, 0.6))*time.Second)
		}
	}
}

// ci reviews every pull request of the payments team, through the router.
func (t *traffic) ci(a actor, day time.Time, weekend bool, growth float64) {
	if weekend {
		return
	}
	for range t.poisson(22 * growth) {
		t.chat(a, call{
			target: "auto", at: t.workTime(day),
			input: int(t.jitter(16000, 0.6)), output: int(t.jitter(900, 0.4)),
		})
	}
}

// notebooks are the data scientists' scheduled jobs: a summary run every
// night, and now and then a corpus embedded for a new model.
func (t *traffic) notebooks(a actor, day time.Time, weekend bool) {
	at := time.Date(day.Year(), day.Month(), day.Day(), 2, t.rnd.IntN(20), 0, 0, t.zone)
	for range t.poisson(60) {
		e := t.chat(a, call{
			target: "codestral-2508", at: at,
			input: int(t.jitter(1800, 0.5)), output: int(t.jitter(450, 0.5)),
		})
		at = at.Add(e.Latency + 200*time.Millisecond)
	}
	if weekend || !t.chance(0.3) {
		return
	}
	at = t.workTime(day)
	for range 300 + t.rnd.IntN(500) {
		e := t.embed(a, at, 64, 380)
		at = at.Add(e.Latency)
	}
}

// call is what a client asked for.
type call struct {
	target        string // the model or router the client named
	at            time.Time
	session       string
	turn          int
	input, output int
	stream        bool
	// interruptible is a client whose user can stop it mid-answer.
	interruptible bool
}

// request is one event being built, with its steps.
type request struct {
	e  store.Event
	at int64 // microseconds since the gateway took the request
}

func (r *request) step(name, of string, us int64, note string) {
	r.e.Spans = append(r.e.Spans, store.Span{Name: name, Of: of, At: r.at, For: us, Note: note})
	r.at += us
}

// done records the request as refused or failed with status, after the
// steps so far.
func (r *request) done(status int, msg string) store.Event {
	r.e.Status, r.e.Error = status, msg
	r.e.Latency = time.Duration(r.at) * time.Microsecond
	return r.e
}

func (t *traffic) start(a actor, at time.Time) *request {
	return &request{e: store.Event{
		TS: at, OrgID: t.org, ProjectID: a.projectID, UserID: a.userID, KeyID: a.keyID,
		Client: a.client, RequestID: requestID(at, t.rnd),
	}}
}

func (t *traffic) chat(a actor, c call) store.Event {
	r := t.start(a, c.at)
	r.e.SessionKey, r.e.Stream = c.session, c.stream
	r.step(store.SpanAuth, "", t.us(20, 80), "")
	r.step(store.SpanReceive, "", 150+int64(c.input)/8+t.us(0, 400), "")
	r.step(store.SpanAdmit, "", t.us(80, 400), "")

	r.e.Alias = c.target
	switch {
	case c.target == "claude-opus-5-5" && a.project == "customer-portal":
		return t.add(r.done(404, "the model 'claude-opus-5-5' does not exist or this key may not use it"))
	case a.project == "customer-portal" && t.chance(0.004):
		return t.add(r.done(429, "your project is over its rate limit of 400000 tokens per minute"))
	}

	var routerScope *policy.Scope
	switch c.target {
	case "auto":
		r.e.Router, r.e.RouterOutcome = "auto", store.RouterChose
		r.e.Alias = localModel
		if t.chance(0.38) {
			r.e.Alias = "claude-opus-5-5"
			routerScope = &policy.Scope{
				Type: policy.ScopeRouter, ID: t.org + "/auto/" + autoBudgetStep,
				Period: policy.PeriodMonth,
			}
		}
		took := time.Duration(t.jitter(650, 0.35)) * time.Millisecond
		note := "chose " + r.e.Alias
		if t.chance(0.004) {
			r.e.RouterOutcome, r.e.Alias, routerScope = store.RouterFellBack, localModel, nil
			took = 5 * time.Second
			note = "fell back to " + localModel
			r.e.Note = "the router 'auto' could not choose: the model it decides with, '" +
				decisionModel + "', did not answer within 5s"
		}
		r.e.RouterMS = took.Milliseconds()
		r.step(store.SpanRoute, "auto/route", took.Microseconds(), note)
	case "swiss-first":
		r.e.Router, r.e.RouterOutcome, r.e.Alias = "swiss-first", store.RouterChose, "qwen3-coder-next"
	}

	for _, f := range append([]filter{orgFilter}, projectFilters[a.project]...) {
		if refusal, status := t.filter(r, f, c); refusal != "" {
			return t.add(r.done(status, refusal))
		}
	}
	r.step(store.SpanPrepare, "", t.us(30, 200), "")

	if r.e.Router == "swiss-first" && t.fails("qwen3-coder-next", c.at) {
		// Switzerland was down, so the EU answered.
		r.step(store.SpanUpstream, "qwen3-coder-next", t.us(2_000_000, 10_000_000), "answered 503")
		r.e.Alias, r.e.RouterOutcome = "codestral-2508", store.RouterFellBack
		r.e.Note = "tried before this one: qwen3-coder-next answered 503"
	}
	return t.add(t.answer(r, c, routerScope))
}

// filter runs one filter over the request, and says why it was refused, if
// it was.
func (t *traffic) filter(r *request, f filter, c call) (refusal string, status int) {
	run := store.FilterRun{Filter: f.alias, Mode: f.mode, Shadow: f.shadow, Segments: 2 + c.turn*2}
	var us int64
	switch f.mode {
	case policy.FilterModePattern:
		us = t.us(40, 300) + int64(c.input)/40
		switch {
		case t.chance(0.0015):
			run.Outcome = store.FilterRefuse
		case t.chance(0.02):
			run.Outcome, run.Changed = store.FilterRewrite, 1
		default:
			run.Outcome = store.FilterPass
		}
	case policy.FilterModeGate:
		us = int64(t.jitter(450, 0.4)) * 1000
		switch {
		case t.chance(0.012):
			run.Outcome = store.FilterRefuse
		default:
			run.Outcome = store.FilterPass
		}
	default:
		us = int64(t.jitter(900, 0.5)) * 1000
		switch {
		case t.chance(0.001):
			run.Outcome = store.FilterError
		case t.chance(0.18):
			run.Outcome, run.Changed = store.FilterRewrite, 1+t.rnd.IntN(3)
		default:
			run.Outcome = store.FilterPass
		}
	}
	run.LatencyMS = us / 1000
	r.e.FilterRuns = append(r.e.FilterRuns, run)
	note := map[store.FilterOutcome]string{
		store.FilterPass: "passed", store.FilterRewrite: "rewrote",
		store.FilterRefuse: "refused", store.FilterError: "could not run",
	}[run.Outcome]
	if f.shadow {
		note += ", shadow"
	}
	r.step(store.SpanFilter, f.alias, us, note)
	if f.shadow {
		return "", 0
	}
	switch {
	case run.Outcome == store.FilterRefuse && f.mode == policy.FilterModePattern:
		return f.by + " checks every request against the rules in '" + f.alias +
			"' before it is forwarded, and one of them matched this request, saying: " +
			"that moves a private key out. Nothing was sent to the model", 403
	case run.Outcome == store.FilterRefuse:
		return f.by + " checks every request against '" + f.alias +
			"' before it is forwarded, and it refused this one. Nothing was sent to the model", 403
	}
	return "", 0
}

// answer asks the model and records what came back.
func (t *traffic) answer(r *request, c call, routerScope *policy.Scope) store.Event {
	alias := r.e.Alias
	m, sp := t.models[alias], speeds[alias]
	input := c.input
	if m.MaxContext > 0 {
		input = min(input, m.MaxContext*9/10)
	}
	tokens := policy.Tokens{Input: input, Output: c.output}
	// A provider with a cache price serves an agent's growing prompt from its
	// cache, and writes the new part.
	if m.CachedInputMicrosPerMTok > 0 && c.session != "" {
		if c.turn > 0 {
			tokens.CachedInput = int(float64(input) * (0.8 + 0.15*t.rnd.Float64()))
		}
		tokens.CacheWrite = (input - tokens.CachedInput) * 4 / 5
	}
	prefill := time.Duration(float64(input-tokens.CachedInput)/sp.prefillPerMS) * time.Millisecond
	ttft := time.Duration(t.jitter(float64(sp.ttft), 0.35)) + prefill
	var gen time.Duration
	if sp.tokensPerSec > 0 {
		gen = time.Duration(float64(c.output) / sp.tokensPerSec * float64(time.Second))
	}

	if t.fails(alias, c.at) {
		r.step(store.SpanUpstream, alias, (ttft / 2).Microseconds(),
			fmt.Sprintf("answered %d", sp.status))
		return r.done(sp.status, sp.failureReason)
	}
	interrupted := c.interruptible && t.chance(0.008)
	if interrupted {
		cut := 0.1 + 0.5*t.rnd.Float64()
		tokens.Output = int(float64(tokens.Output) * cut)
		gen = time.Duration(float64(gen) * cut)
		r.e.Canceled, r.e.Estimated = true, true
		r.e.Error = "the stream ended before the model was done: context canceled"
	}

	headers := min(ttft, time.Duration(t.jitter(90, 0.5))*time.Millisecond)
	r.step(store.SpanUpstream, alias, headers.Microseconds(), "answered 200")
	if c.stream {
		r.step(store.SpanWait, alias, (ttft - headers).Microseconds(), "")
		r.e.TTFT = time.Duration(r.at) * time.Microsecond
		r.step(store.SpanStream, alias, gen.Microseconds(), "")
	} else {
		r.step(store.SpanRespond, alias, (ttft - headers + gen).Microseconds(), "")
		r.e.TTFT = time.Duration(r.at) * time.Microsecond
	}
	r.e.Status = 200
	r.e.Latency = time.Duration(r.at) * time.Microsecond
	r.e.InputTokens, r.e.OutputTokens = tokens.Input, tokens.Output
	r.e.CachedInputTokens, r.e.CacheWriteTokens = tokens.CachedInput, tokens.CacheWrite
	r.e.CostMicros = m.Cost(tokens)
	r.e.Scopes = t.scopes(r.e)
	if routerScope != nil {
		r.e.Scopes = append(r.e.Scopes, *routerScope)
	}
	return r.e
}

// embed is one embedding request of a batch of texts, each about size
// tokens.
func (t *traffic) embed(a actor, at time.Time, texts, size int) store.Event {
	r := t.start(a, at)
	r.e.Alias = "bge-m3"
	input := texts * int(t.jitter(float64(size), 0.4))
	r.step(store.SpanAuth, "", t.us(20, 80), "")
	r.step(store.SpanReceive, "", 100+int64(input)/8, "")
	r.step(store.SpanAdmit, "", t.us(60, 250), "")
	r.step(store.SpanPrepare, "", t.us(20, 100), "")
	sp := speeds["bge-m3"]
	took := time.Duration(t.jitter(float64(sp.ttft), 0.3)) +
		time.Duration(float64(input)/sp.prefillPerMS)*time.Millisecond
	if t.fails(r.e.Alias, at) {
		r.step(store.SpanUpstream, r.e.Alias, took.Microseconds(), fmt.Sprintf("answered %d", sp.status))
		return t.add(r.done(sp.status, sp.failureReason))
	}
	r.step(store.SpanUpstream, r.e.Alias, took.Microseconds(), "answered 200")
	r.step(store.SpanRespond, r.e.Alias, t.us(100, 900), "")
	r.e.Status = 200
	r.e.Latency = time.Duration(r.at) * time.Microsecond
	r.e.TTFT = r.e.Latency
	r.e.InputTokens = input
	r.e.CostMicros = t.models[r.e.Alias].Cost(policy.Tokens{Input: input})
	r.e.Scopes = t.scopes(r.e)
	return t.add(r.e)
}

// toolCall is a coding agent calling a tool through the gateway's MCP proxy.
func (t *traffic) toolCall(a actor, at time.Time, session string) store.Event {
	tools := map[string][]string{
		"github":    {"search_code", "get_file_contents", "list_pull_requests", "get_pull_request", "create_pull_request", "get_issue"},
		"atlassian": {"getJiraIssue", "searchJiraIssuesUsingJql", "getConfluencePage", "addCommentToJiraIssue"},
	}
	server := "github"
	if t.chance(0.35) {
		server = "atlassian"
	}
	outcome, result := store.ToolOK, int(t.jitter(6000, 0.9))
	if t.chance(0.006) {
		outcome, result = store.ToolFailed, 120
	}
	return t.add(store.Event{
		TS: at, OrgID: t.org, ProjectID: a.projectID, UserID: a.userID, KeyID: a.keyID,
		Client: a.client, SessionKey: session,
		Latency: time.Duration(t.jitter(450, 0.6)) * time.Millisecond,
		Tool: &store.ToolCall{
			Server: server, Tool: tools[server][t.rnd.IntN(len(tools[server]))],
			Outcome: outcome, ArgBytes: 60 + t.rnd.IntN(400), ResultBytes: result,
		},
	})
}

// scopes are the budget windows a request's cost counts against.
func (t *traffic) scopes(e store.Event) []policy.Scope {
	scopes := []policy.Scope{
		{Type: policy.ScopeOrg, ID: e.OrgID, Period: policy.PeriodMonth},
		{Type: policy.ScopeProject, ID: e.ProjectID, Period: policy.PeriodMonth},
		{Type: policy.ScopeKey, ID: e.KeyID, Period: policy.PeriodMonth},
	}
	if e.KeyID == t.dailyKey {
		scopes = append(scopes, policy.Scope{Type: policy.ScopeKey, ID: e.KeyID, Period: policy.PeriodDay})
	}
	return scopes
}

func (t *traffic) add(e store.Event) store.Event {
	t.events = append(t.events, e)
	return e
}

// officeHours weighs the hours from 8 to 19: busiest mid-morning and
// mid-afternoon, quiet over lunch.
var officeHours = []float64{3, 8, 10, 9, 3, 6, 10, 10, 8, 5, 2, 1}

// workTime is a moment in office hours.
func (t *traffic) workTime(day time.Time) time.Time {
	h := 8 + t.pickIndex(officeHours)
	return t.clock(day, h, h+1)
}

// clock is a moment between two hours of day.
func (t *traffic) clock(day time.Time, from, to int) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), from, 0, 0, 0, t.zone).
		Add(time.Duration(t.rnd.Int64N(int64(to-from) * int64(time.Hour))))
}

func (t *traffic) pick(ws []weighted) string {
	weights := make([]float64, len(ws))
	for i, w := range ws {
		weights[i] = w.weight
	}
	return ws[t.pickIndex(weights)].alias
}

// pickIndex picks an index, each as likely as its weight.
func (t *traffic) pickIndex(weights []float64) int {
	var sum float64
	for _, w := range weights {
		sum += w
	}
	x := t.rnd.Float64() * sum
	for i, w := range weights {
		if x -= w; x < 0 {
			return i
		}
	}
	return len(weights) - 1
}

func (t *traffic) chance(p float64) bool { return t.rnd.Float64() < p }

// jitter is m, spread the way latencies and token counts are: mostly near it,
// now and then far above.
func (t *traffic) jitter(m, sigma float64) float64 {
	return m * math.Exp(t.rnd.NormFloat64()*sigma)
}

func (t *traffic) us(from, to int64) int64 { return from + t.rnd.Int64N(to-from+1) }

// poisson is how many of something with mean lambda happen.
func (t *traffic) poisson(lambda float64) int {
	n, p, limit := 0, 1.0, math.Exp(-lambda)
	for {
		if p *= t.rnd.Float64(); p <= limit {
			return n
		}
		n++
	}
}

// sessionKey is a session's key as the gateway derives it: a hash of the
// session the client named, or of its opening prompt. Random bytes look the
// same.
func (t *traffic) sessionKey(stated bool) string {
	b := make([]byte, 12)
	binary.LittleEndian.PutUint64(b, t.rnd.Uint64())
	binary.LittleEndian.PutUint32(b[8:], t.rnd.Uint32())
	prefix := "p"
	if stated {
		prefix = "c"
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

var idEncoding = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

// requestID is an id like internal/id makes, for a request at.
func requestID(at time.Time, rnd *rand.Rand) string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], uint64(at.UnixMilli())<<16)
	binary.BigEndian.PutUint64(b[8:], rnd.Uint64())
	binary.BigEndian.PutUint16(b[6:8], uint16(rnd.Uint32()))
	return "req_" + idEncoding.EncodeToString(b[:])
}
