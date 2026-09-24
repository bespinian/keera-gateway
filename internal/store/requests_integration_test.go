package store

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// The failure log is what the dashboard's count is opened up into, so what it
// has to carry is the three things the count cannot say: which model, whose
// key, and what the client was told.
func TestFailuresCarryWhatTheClientWasTold(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC().Truncate(time.Second)

	if err := st.WriteEvents(ctx, []Event{
		{TS: now.Add(-3 * time.Hour), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-code", Status: 200, CostMicros: 500},
		{TS: now.Add(-2 * time.Hour), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-code", Status: 500, Error: "CUDA out of memory",
			Latency: 1200 * time.Millisecond, Stream: true},
		{TS: now.Add(-time.Hour), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-fast", Status: 429, Error: "your team is over its rate limit"},
		// Answered, and then cut short: the 200 the client was sent stands.
		{TS: now.Add(-30 * time.Minute), OrgID: f.orgID, KeyID: f.keyID,
			Alias: "keera-code", Status: 200, Error: "the stream ended before the model was done"},
	}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	failed, err := st.Failures(ctx, FailureQuery{OrgID: f.orgID, Kind: KindFailed})
	if err != nil {
		t.Fatalf("Failures: %v", err)
	}
	if len(failed) != 1 {
		t.Fatalf("%d failures, want only the backend error", len(failed))
	}
	got := failed[0]
	if got.Alias != "keera-code" || got.KeyID != f.keyID || got.TeamID != f.teamID {
		t.Errorf("row = %+v, want the model, the key and the team it happened to", got)
	}
	if got.Error != "CUDA out of memory" {
		t.Errorf("Error = %q, want the backend's own wording", got.Error)
	}
	if got.LatencyMS != 1200 || !got.Stream {
		t.Errorf("row = %+v, want how long it took and that it was a stream", got)
	}

	refused, err := st.Failures(ctx, FailureQuery{OrgID: f.orgID, Kind: KindRefused})
	if err != nil {
		t.Fatalf("Failures: %v", err)
	}
	if len(refused) != 1 || refused[0].Status != 429 {
		t.Errorf("refusals = %+v, want only the rate limit", refused)
	}

	cut, err := st.Failures(ctx, FailureQuery{OrgID: f.orgID, Kind: KindInterrupted})
	if err != nil {
		t.Fatalf("Failures: %v", err)
	}
	if len(cut) != 1 || cut[0].Status != 200 {
		t.Errorf("interrupted = %+v, want the stream that stopped, still carrying its 200", cut)
	}

	all, err := st.Failures(ctx, FailureQuery{OrgID: f.orgID})
	if err != nil {
		t.Fatalf("Failures: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("%d rows, want the three that did not deliver and not the one that did", len(all))
	}
	// Newest first: an investigation starts at what just happened.
	if !all[0].TS.After(all[len(all)-1].TS) {
		t.Errorf("rows are not newest first: %v then %v", all[0].TS, all[len(all)-1].TS)
	}
}

// Narrowing is how a reader gets from "something is failing" to one model or
// one key, and a window with none of it must not report another tenant's.
func TestFailuresNarrowAndStayInsideOneTenant(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	if _, err := st.CreateOrg(ctx, "org_2", "Another Bank"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)

	events := []Event{
		{TS: now.Add(-time.Hour), OrgID: f.orgID, KeyID: f.keyID, Alias: "keera-code",
			Status: 502, Error: "connection refused"},
		{TS: now.Add(-time.Hour), OrgID: f.orgID, KeyID: "key_2", Alias: "keera-fast",
			Status: 500, Error: "internal server error"},
		{TS: now.Add(-40 * 24 * time.Hour), OrgID: f.orgID, KeyID: f.keyID,
			Alias: "keera-code", Status: 500, Error: "long ago"},
		{TS: now.Add(-time.Hour), OrgID: "org_2", KeyID: "key_9", Alias: "keera-code",
			Status: 500, Error: "someone else's problem"},
	}
	if err := st.WriteEvents(ctx, events); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	window := FailureQuery{OrgID: f.orgID, From: now.AddDate(0, 0, -7), To: now.Add(time.Minute)}

	byAlias := window
	byAlias.Alias = "keera-code"
	rows, err := st.Failures(ctx, byAlias)
	if err != nil {
		t.Fatalf("Failures: %v", err)
	}
	if len(rows) != 1 || rows[0].Error != "connection refused" {
		t.Errorf("rows = %+v, want this tenant's keera-code failure inside the window", rows)
	}

	byKey := window
	byKey.KeyID = "key_2"
	if rows, err = st.Failures(ctx, byKey); err != nil {
		t.Fatalf("Failures: %v", err)
	}
	if len(rows) != 1 || rows[0].Alias != "keera-fast" {
		t.Errorf("rows = %+v, want only what key_2 did", rows)
	}

	byStatus := window
	byStatus.Status = 502
	if rows, err = st.Failures(ctx, byStatus); err != nil {
		t.Fatalf("Failures: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != 502 {
		t.Errorf("rows = %+v, want only the 502", rows)
	}

	// Paging backwards on the id cursor, which is stable while the log is still
	// being written to.
	page, err := st.Failures(ctx, FailureQuery{OrgID: f.orgID, Limit: 1})
	if err != nil {
		t.Fatalf("Failures: %v", err)
	}
	if len(page) != 1 {
		t.Fatalf("%d rows, want the one asked for", len(page))
	}
	next, err := st.Failures(ctx, FailureQuery{OrgID: f.orgID, Limit: 1, Before: page[0].ID})
	if err != nil {
		t.Fatalf("Failures: %v", err)
	}
	if len(next) != 1 || next[0].ID >= page[0].ID {
		t.Errorf("the cursor did not move backwards: %+v then %+v", page, next)
	}
}

// The facets are what the screen offers to narrow by, and the counts are what
// make them worth offering: one model failing four hundred times and four
// hundred models failing once are the same number on the dashboard.
func TestFailureFiltersCountTheWholeWindow(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC().Truncate(time.Second)

	events := []Event{
		{TS: now, OrgID: f.orgID, KeyID: f.keyID, Alias: "keera-code", Status: 500, Error: "boom"},
		{TS: now, OrgID: f.orgID, KeyID: f.keyID, Alias: "keera-code", Status: 500, Error: "boom"},
		{TS: now, OrgID: f.orgID, KeyID: "key_2", Alias: "keera-fast", Status: 502, Error: "gone"},
		// A refusal, which the failed kind must not count.
		{TS: now, OrgID: f.orgID, KeyID: f.keyID, Alias: "keera-code", Status: 429, Error: "slow down"},
		// Served, which no kind counts.
		{TS: now, OrgID: f.orgID, KeyID: f.keyID, Alias: "keera-code", Status: 200},
	}
	if err := st.WriteEvents(ctx, events); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	facets, err := st.FailureFilters(ctx, FailureQuery{OrgID: f.orgID, Kind: KindFailed})
	if err != nil {
		t.Fatalf("FailureFilters: %v", err)
	}
	if facets.Total != 3 {
		t.Errorf("Total = %d, want the three failures and neither the refusal nor the "+
			"request that was served", facets.Total)
	}
	// Ordered by how much of the trouble each one is: the worst offender first.
	if len(facets.Models) != 2 || facets.Models[0].Value != "keera-code" ||
		facets.Models[0].Count != 2 {
		t.Errorf("Models = %+v, want keera-code with two, first", facets.Models)
	}
	if len(facets.Keys) != 2 || facets.Keys[0].Value != f.keyID || facets.Keys[0].Count != 2 {
		t.Errorf("Keys = %+v, want the key that produced most of them, first", facets.Keys)
	}
	if len(facets.Statuses) != 2 {
		t.Errorf("Statuses = %+v, want the 500 and the 502", facets.Statuses)
	}

	// The facets describe the window and the kind, not the narrowing already
	// applied: a screen showing only what it is already filtered to offers no
	// way back out.
	narrowed, err := st.FailureFilters(ctx,
		FailureQuery{OrgID: f.orgID, Kind: KindFailed, Alias: "keera-fast"})
	if err != nil {
		t.Fatalf("FailureFilters: %v", err)
	}
	if narrowed.Total != facets.Total || len(narrowed.Models) != len(facets.Models) {
		t.Errorf("facets = %+v, want them unchanged by the alias narrowing", narrowed)
	}
}

// The column holds somebody else's free text, so the bound is enforced where it
// is written rather than trusted to whoever wrote the message.
func TestAnEnormousBackendMessageIsBounded(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)

	huge := strings.Repeat("ä", 4000) // multi-byte, to catch a cut mid-rune
	if err := st.WriteEvents(ctx, []Event{{
		TS: time.Now(), OrgID: f.orgID, KeyID: f.keyID, Alias: "keera-code",
		Status: 500, Error: huge,
	}}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}
	rows, err := st.Failures(ctx, FailureQuery{OrgID: f.orgID})
	if err != nil {
		t.Fatalf("Failures: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d rows, want 1", len(rows))
	}
	if len(rows[0].Error) > maxErrorBytes+len("…") {
		t.Errorf("stored %d bytes, want it bounded at %d", len(rows[0].Error), maxErrorBytes)
	}
	if !utf8.ValidString(rows[0].Error) {
		t.Errorf("the message was cut in the middle of a rune: %q", rows[0].Error)
	}
	if !strings.HasSuffix(rows[0].Error, "…") {
		t.Errorf("a truncated message does not say so: %q", rows[0].Error[len(rows[0].Error)-8:])
	}
}

// The entity screens exist because a share of the organisation's total is not
// an answer about one team. What has to hold is that the narrowed report is the
// same report: this team's requests, tokens, spend and latency, and none of
// anybody else's.
func TestOverviewNarrowsToOneEntity(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC().Truncate(time.Second)

	if _, err := st.CreateTeam(ctx, "team_2", f.orgID, "Data Science"); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if _, err := st.CreateKey(ctx, KeyInfo{
		ID: "key_2", OrgID: f.orgID, TeamID: "team_2", Alias: "a notebook", Prefix: "keera_sk_two",
	}, []byte("hash-of-the-second-key-32-bytes!")); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if err := st.WriteEvents(ctx, []Event{
		{TS: now.Add(-2 * time.Hour), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-code", InputTokens: 100, OutputTokens: 20,
			CostMicros: 500, Status: 200, TTFT: 200 * time.Millisecond},
		{TS: now.Add(-time.Hour), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-fast", InputTokens: 10, OutputTokens: 5,
			CostMicros: 100, Status: 200, TTFT: 50 * time.Millisecond},
		{TS: now.Add(-time.Hour), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-code", Status: 502, Error: "no backend answered"},
		// Another team, another key, the same window: none of it belongs in the
		// first team's report.
		{TS: now.Add(-time.Hour), OrgID: f.orgID, TeamID: "team_2", KeyID: "key_2",
			Alias: "keera-code", InputTokens: 900, OutputTokens: 90,
			CostMicros: 9000, Status: 200, TTFT: 400 * time.Millisecond},
	}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}
	from, to := now.Add(-6*time.Hour), now.Add(time.Hour)

	whole, err := st.Overview(ctx, f.orgID, from, to, Scope{})
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if whole.Requests != 4 || whole.CostMicros != 9600 {
		t.Errorf("unscoped = %d requests / %d micros, want 4 / 9600",
			whole.Requests, whole.CostMicros)
	}
	// The dashboard does not pay for a breakdown it does not draw.
	if whole.TopKeys != nil {
		t.Errorf("TopKeys = %+v on an unscoped overview, want none", whole.TopKeys)
	}

	team, err := st.Overview(ctx, f.orgID, from, to, Scope{TeamID: f.teamID})
	if err != nil {
		t.Fatalf("Overview by team: %v", err)
	}
	if team.Requests != 3 {
		t.Errorf("Requests = %d, want the team's 3 - refused and failed included",
			team.Requests)
	}
	if team.CostMicros != 600 {
		t.Errorf("CostMicros = %d, want 600 and not the other team's spend", team.CostMicros)
	}
	if team.Failed != 1 {
		t.Errorf("Failed = %d, want 1", team.Failed)
	}
	if team.TTFTP95MS > 300 {
		t.Errorf("TTFTP95MS = %d, want the team's own samples, not the slow one next door",
			team.TTFTP95MS)
	}
	// A scoped screen breaks its traffic down by key, because "which of them is
	// doing this" is the next question.
	if len(team.TopKeys) != 1 || team.TopKeys[0].Group != f.keyID {
		t.Errorf("TopKeys = %+v, want only this team's key", team.TopKeys)
	}
	if len(team.TopModels) != 2 {
		t.Errorf("TopModels = %+v, want both models the team used", team.TopModels)
	}
	var charted int64
	for _, p := range team.Series {
		charted += p.Requests
	}
	if charted != 3 {
		t.Errorf("the chart plots %d requests, want the same 3 the totals count", charted)
	}

	model, err := st.Overview(ctx, f.orgID, from, to, Scope{Alias: "keera-code"})
	if err != nil {
		t.Fatalf("Overview by model: %v", err)
	}
	if model.Requests != 3 || model.CostMicros != 9500 {
		t.Errorf("by model = %d requests / %d micros, want 3 / 9500",
			model.Requests, model.CostMicros)
	}
	if len(model.TopTeams) != 2 {
		t.Errorf("TopTeams = %+v, want both teams that called this model", model.TopTeams)
	}

	key, err := st.Overview(ctx, f.orgID, from, to, Scope{KeyID: "key_2"})
	if err != nil {
		t.Fatalf("Overview by key: %v", err)
	}
	if key.Requests != 1 || key.CostMicros != 9000 {
		t.Errorf("by key = %d requests / %d micros, want 1 / 9000",
			key.Requests, key.CostMicros)
	}
}

// The request log is the half of the event log the failure log throws away.
// "My agent's calls never arrived" is answered by the served rows, and a screen
// that only holds failures cannot answer it.
// The breakdown of one request's latency survives the round trip, and a
// request that has none is told apart from a request whose steps were all of
// zero length. The second is what a row written by an older gateway looks like,
// and the panel draws nothing at all rather than a chart of nothing.
func TestRequestsCarryWhereTheirTimeWent(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC().Truncate(time.Second)

	steps := []Span{
		{Name: SpanAuth, At: 0, For: 120},
		{Name: SpanReceive, At: 130, For: 2400},
		{Name: SpanAdmit, At: 2540, For: 95},
		{Name: SpanFilter, Of: "redact", At: 2640, For: 310000, Note: "rewrote"},
		{Name: SpanUpstream, Of: "keera-code", At: 312700, For: 890000,
			Note: "answered 200"},
	}
	if err := st.WriteEvents(ctx, []Event{
		{TS: now.Add(-time.Hour), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-code", Status: 200, Latency: 1203 * time.Millisecond,
			Spans: steps},
		{TS: now, OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-code", Status: 200, Latency: time.Second},
	}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	rows, err := st.Requests(ctx, RequestQuery{OrgID: f.orgID})
	if err != nil {
		t.Fatalf("Requests: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2", len(rows))
	}
	if rows[0].Spans != nil {
		t.Errorf("a request that recorded no steps came back with %v; nothing and "+
			"nowhere have to stay different things", rows[0].Spans)
	}
	if !slices.Equal(rows[1].Spans, steps) {
		t.Errorf("the steps came back as %+v, want %+v", rows[1].Spans, steps)
	}
}

func TestRequestsCarryWhatWasServedAndWhatWasNot(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC().Truncate(time.Second)

	if err := st.WriteEvents(ctx, []Event{
		{TS: now.Add(-4 * time.Hour), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-code", InputTokens: 1000, OutputTokens: 200,
			CostMicros: 2500, Status: 200, Latency: 4 * time.Second,
			TTFT: 300 * time.Millisecond, Stream: true},
		{TS: now.Add(-3 * time.Hour), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-code", Status: 500, Error: "CUDA out of memory"},
		{TS: now.Add(-2 * time.Hour), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-fast", Status: 429, Error: "over the rate limit"},
		{TS: now.Add(-time.Hour), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-code", Status: 200, Error: "the stream ended early",
			Canceled: true, Estimated: true, OutputTokens: 12},
	}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	all, err := st.Requests(ctx, RequestQuery{OrgID: f.orgID})
	if err != nil {
		t.Fatalf("Requests: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("%d rows, want every request including the served one", len(all))
	}
	// Newest first, the same way every log here reads.
	if !all[0].TS.After(all[len(all)-1].TS) {
		t.Errorf("rows are not newest-first: %v then %v", all[0].TS, all[len(all)-1].TS)
	}

	served, err := st.Requests(ctx, RequestQuery{OrgID: f.orgID, Outcome: OutcomeOK})
	if err != nil {
		t.Fatalf("Requests(ok): %v", err)
	}
	if len(served) != 1 {
		t.Fatalf("%d served, want 1", len(served))
	}
	got := served[0]
	if got.InputTokens != 1000 || got.OutputTokens != 200 || got.CostMicros != 2500 {
		t.Errorf("tokens and cost = %d/%d/%d, want 1000/200/2500",
			got.InputTokens, got.OutputTokens, got.CostMicros)
	}
	if got.TTFTMS != 300 || got.LatencyMS != 4000 {
		t.Errorf("ttft/latency = %d/%d ms, want 300/4000 - a model that has gone slow "+
			"is only visible in the first of them", got.TTFTMS, got.LatencyMS)
	}
	if !got.Stream {
		t.Error("the row lost that it was streamed")
	}

	// An interruption is a 200 that did not finish, which is neither a failure
	// nor a refusal and has to stay tellable apart from both.
	cut, err := st.Requests(ctx, RequestQuery{OrgID: f.orgID, Outcome: OutcomeInterrupted})
	if err != nil {
		t.Fatalf("Requests(interrupted): %v", err)
	}
	if len(cut) != 1 || !cut[0].Canceled || !cut[0].Estimated {
		t.Errorf("interrupted = %+v, want the one cancelled row, marked estimated", cut)
	}

	// Scoped to a model, and paged on the id rather than an offset.
	fast, err := st.Requests(ctx, RequestQuery{OrgID: f.orgID, Scope: Scope{Alias: "keera-fast"}})
	if err != nil {
		t.Fatalf("Requests(alias): %v", err)
	}
	if len(fast) != 1 || fast[0].Status != 429 {
		t.Errorf("by alias = %+v, want only the refused keera-fast call", fast)
	}
	page, err := st.Requests(ctx, RequestQuery{OrgID: f.orgID, Limit: 2})
	if err != nil {
		t.Fatalf("Requests(limit): %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("%d rows on a page of 2", len(page))
	}
	next, err := st.Requests(ctx, RequestQuery{OrgID: f.orgID, Limit: 2, Before: page[1].ID})
	if err != nil {
		t.Fatalf("Requests(before): %v", err)
	}
	if len(next) != 2 || next[0].ID >= page[1].ID {
		t.Errorf("the second page = %+v, want the two older rows", next)
	}
}

// The log read forwards rather than backwards: what a panel watching it asks
// for is everything past the newest row it has already been shown.
func TestRequestsAfterACursorIsWhatALiveReaderSees(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC().Truncate(time.Second)

	// Empty: a reader with nothing on screen has to be startable, and starting
	// them at zero is what makes the difference between watching and replaying.
	first, err := st.LatestEventID(ctx)
	if err != nil {
		t.Fatalf("LatestEventID: %v", err)
	}
	if first != 0 {
		t.Errorf("LatestEventID = %d on an empty log, want 0", first)
	}

	if err := st.WriteEvents(ctx, []Event{
		{TS: now.Add(-2 * time.Minute), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-code", Status: 200},
		{TS: now.Add(-time.Minute), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-code", Status: 500, Error: "CUDA out of memory"},
	}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	seen, err := st.LatestEventID(ctx)
	if err != nil {
		t.Fatalf("LatestEventID: %v", err)
	}
	if seen == 0 {
		t.Fatal("LatestEventID = 0 with two rows written")
	}
	// Caught up: a reader holding the newest row is sent nothing, rather than
	// being sent that row again every time the log is written to.
	none, err := st.Requests(ctx, RequestQuery{OrgID: f.orgID, After: seen})
	if err != nil {
		t.Fatalf("Requests(after): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("%d rows past the newest one, want none: %+v", len(none), none)
	}

	if err := st.WriteEvents(ctx, []Event{
		{TS: now, OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-fast", Status: 429, Error: "over the rate limit"},
	}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}
	fresh, err := st.Requests(ctx, RequestQuery{OrgID: f.orgID, After: seen})
	if err != nil {
		t.Fatalf("Requests(after): %v", err)
	}
	if len(fresh) != 1 || fresh[0].Alias != "keera-fast" {
		t.Fatalf("past the cursor = %+v, want only the newest request", fresh)
	}

	// The cursor narrows with everything else rather than instead of it: a
	// reader watching the failures is not sent the served rows in between.
	served, err := st.Requests(ctx, RequestQuery{
		OrgID: f.orgID, After: seen, Outcome: OutcomeOK,
	})
	if err != nil {
		t.Fatalf("Requests(after, ok): %v", err)
	}
	if len(served) != 0 {
		t.Errorf("a reader watching served requests was sent %+v", served)
	}
	// And both ends of the cursor still meet in the middle, so that paging
	// backwards and watching forwards cannot disagree about the same row.
	older, err := st.Requests(ctx, RequestQuery{
		OrgID: f.orgID, After: 0, Before: fresh[0].ID,
	})
	if err != nil {
		t.Fatalf("Requests(before): %v", err)
	}
	if len(older) != 2 {
		t.Errorf("%d rows before the newest, want the two written first", len(older))
	}
}

// Writing the log announces itself, which is what lets a panel show a request
// as it is recorded rather than when its reader next reloads.
func TestWriteEventsAnnouncesThatTheLogGrew(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)

	conn, err := st.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+EventsChannel); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}

	if err := st.WriteEvents(ctx, []Event{
		{TS: time.Now(), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-code", Status: 200},
	}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	n, err := conn.Conn().WaitForNotification(waitCtx)
	if err != nil {
		t.Fatalf("WaitForNotification: %v", err)
	}
	if n.Channel != EventsChannel {
		t.Errorf("notification arrived on %q, want %q - a policy channel carrying "+
			"these would have every replica drop its cache several times a second",
			n.Channel, EventsChannel)
	}
}

// The counts are what the log leads with, because "four hundred requests, two
// refused" and "four hundred requests, three hundred refused" are the same
// first page of a table and completely different afternoons.
func TestOutcomesCountTheWindowNotThePage(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC().Truncate(time.Second)

	events := []Event{
		{TS: now.Add(-time.Hour), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-code", Status: 500, Error: "backend fell over"},
		{TS: now.Add(-time.Hour), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-code", Status: 402, Error: "budget spent"},
		{TS: now.Add(-time.Hour), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-code", Status: 200, Error: "cut short"},
		// Outside the window, and so outside every count.
		{TS: now.AddDate(0, 0, -10), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-code", Status: 200},
	}
	for range 3 {
		events = append(events, Event{TS: now.Add(-time.Hour), OrgID: f.orgID,
			TeamID: f.teamID, KeyID: f.keyID, Alias: "keera-code", Status: 200})
	}
	if err := st.WriteEvents(ctx, events); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	q := RequestQuery{
		OrgID: f.orgID, Scope: Scope{TeamID: f.teamID},
		From: now.Add(-6 * time.Hour), To: now.Add(time.Hour),
		// The outcome the reader is looking at must not narrow the counts: a
		// filter that only offers the filter already applied is no filter.
		Outcome: OutcomeFailed, Limit: 1,
	}
	c, err := st.Outcomes(ctx, q)
	if err != nil {
		t.Fatalf("Outcomes: %v", err)
	}
	if c.Total != 6 {
		t.Errorf("Total = %d, want the 6 inside the window", c.Total)
	}
	if c.OK != 3 || c.Failed != 1 || c.Refused != 1 || c.Interrupted != 1 {
		t.Errorf("counts = %+v, want 3 served, 1 failed, 1 refused, 1 interrupted", c)
	}

	// A status is not an outcome, so it does narrow them. The total beside
	// "All" is what the reader compares against the rows on the screen, and a
	// filter the rows obey and the total does not is a total nobody can use.
	byStatus := q
	byStatus.Status = 402
	only, err := st.Outcomes(ctx, byStatus)
	if err != nil {
		t.Fatalf("Outcomes(status): %v", err)
	}
	if only.Total != 1 || only.Refused != 1 || only.OK != 0 {
		t.Errorf("counts under status 402 = %+v, want the one refused row and no others", only)
	}

	// Another team's traffic is not in this team's counts.
	if _, err := st.CreateTeam(ctx, "team_2", f.orgID, "Data Science"); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if err := st.WriteEvents(ctx, []Event{{TS: now.Add(-time.Hour), OrgID: f.orgID,
		TeamID: "team_2", Alias: "keera-code", Status: 500, Error: "not ours"}}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}
	again, err := st.Outcomes(ctx, q)
	if err != nil {
		t.Fatalf("Outcomes: %v", err)
	}
	if again.Failed != 1 {
		t.Errorf("Failed = %d after another team failed, want this team's 1", again.Failed)
	}

	// The live stream counts the whole window up to its cursor, then only the
	// rows past it.
	latest, err := st.LatestEventID(ctx)
	if err != nil {
		t.Fatalf("LatestEventID: %v", err)
	}
	upTo := q
	upTo.Before = latest + 1
	if c, err := st.Outcomes(ctx, upTo); err != nil || c != again {
		t.Errorf("counts up to the latest row = %+v, %v; want all of them, %+v", c, err, again)
	}
	past := q
	past.After = latest
	if c, err := st.Outcomes(ctx, past); err != nil || c.Total != 0 {
		t.Errorf("counts past the latest row = %+v, %v; want none", c, err)
	}
}

// A class of statuses is one filter, because it is one question. "Did anything
// fail this afternoon" is asked about all of 5XX at once, and the reader asking
// it does not know which of 500, 502 and 504 their backends answer with.
func TestRequestsNarrowToAClassOfStatuses(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC().Truncate(time.Second)

	if err := st.WriteEvents(ctx, []Event{
		{TS: now.Add(-4 * time.Hour), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-code", Status: 200},
		{TS: now.Add(-3 * time.Hour), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-code", Status: 402, Error: "budget spent"},
		{TS: now.Add(-2 * time.Hour), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-code", Status: 429, Error: "over the rate limit"},
		{TS: now.Add(-time.Hour), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-code", Status: 503, Error: "nothing serving"},
	}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	client, err := st.Requests(ctx, RequestQuery{OrgID: f.orgID, StatusClass: 4})
	if err != nil {
		t.Fatalf("Requests(4xx): %v", err)
	}
	if len(client) != 2 {
		t.Fatalf("%d rows for 4XX, want the refused two", len(client))
	}
	for _, r := range client {
		if r.Status < 400 || r.Status > 499 {
			t.Errorf("status %d came back under 4XX", r.Status)
		}
	}

	server, err := st.Requests(ctx, RequestQuery{OrgID: f.orgID, StatusClass: 5})
	if err != nil {
		t.Fatalf("Requests(5xx): %v", err)
	}
	if len(server) != 1 || server[0].Status != 503 {
		t.Errorf("5XX = %+v, want only the 503", server)
	}

	// The counts obey the class for the same reason they obey an exact status:
	// the total they carry is the one written beside "All" on the screen, and a
	// total the rows below it disagree with is a total nobody can use.
	c, err := st.Outcomes(ctx, RequestQuery{OrgID: f.orgID, StatusClass: 4})
	if err != nil {
		t.Fatalf("Outcomes(4xx): %v", err)
	}
	if c.Total != 2 || c.Refused != 2 || c.OK != 0 || c.Failed != 0 {
		t.Errorf("counts under 4XX = %+v, want the two refused rows and no others", c)
	}
}

// The facets are the vocabulary of the filters, and they are read from the log
// rather than from the catalogue: a deployment with two hundred keys has four
// that made a request this week, and offering the other hundred and ninety-six
// is offering a list somebody has to search to find the row that matters.
func TestRequestFiltersOfferOnlyWhatOccurred(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC().Truncate(time.Second)

	if _, err := st.UpsertUser(ctx, "user_1", f.orgID, "dev@example.ch", "", "member"); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	if err := st.WriteEvents(ctx, []Event{
		{TS: now.Add(-time.Hour), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			UserID: "user_1", Alias: "keera-code", Status: 200},
		{TS: now.Add(-time.Hour), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			UserID: "user_1", Alias: "keera-code", Status: 500, Error: "backend fell over"},
		{TS: now.Add(-time.Hour), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-fast", Status: 500, Error: "backend fell over"},
		// Outside the window, so outside every facet: a model nobody has called
		// this week must not be offered as a way to narrow this week.
		{TS: now.AddDate(0, 0, -10), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-retired", Status: 200},
	}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	q := RequestQuery{
		OrgID: f.orgID,
		From:  now.Add(-6 * time.Hour), To: now.Add(time.Hour),
		// What has already been narrowed to must not narrow the facets, or the
		// screen ends up offering only the filter it is already showing.
		Scope: Scope{Alias: "keera-code"},
	}
	all, err := st.RequestFilters(ctx, q)
	if err != nil {
		t.Fatalf("RequestFilters: %v", err)
	}
	if len(all.Models) != 2 {
		t.Errorf("models = %+v, want the two called inside the window and not the "+
			"retired one, and not narrowed to the alias already selected", all.Models)
	}
	if len(all.Models) > 0 && (all.Models[0].Value != "keera-code" || all.Models[0].Count != 2) {
		t.Errorf("models[0] = %+v, want keera-code with 2 - the busiest first, so that "+
			"the one that matters is the one at the top", all.Models[0])
	}
	if len(all.Keys) != 1 || all.Keys[0].Count != 3 {
		t.Errorf("keys = %+v, want the one key with all 3 rows", all.Keys)
	}
	if len(all.Teams) != 1 || all.Teams[0].Value != f.teamID {
		t.Errorf("teams = %+v, want the one team", all.Teams)
	}
	// A row with no person on it is nobody's, not a blank entry in the list.
	if len(all.Users) != 1 || all.Users[0].Value != "user_1" || all.Users[0].Count != 2 {
		t.Errorf("users = %+v, want user_1 with its 2 rows and no empty value", all.Users)
	}
	if len(all.Statuses) != 2 {
		t.Errorf("statuses = %+v, want the 200 and the 500", all.Statuses)
	}

	// The outcome being read does narrow them: which models are failing is the
	// question, and a model that never failed is not an answer to it.
	failed, err := st.RequestFilters(ctx, RequestQuery{
		OrgID: f.orgID, From: q.From, To: q.To, Outcome: OutcomeFailed,
	})
	if err != nil {
		t.Fatalf("RequestFilters(failed): %v", err)
	}
	if len(failed.Models) != 2 {
		t.Errorf("models under failed = %+v, want the two that failed", failed.Models)
	}
	if len(failed.Statuses) != 1 || failed.Statuses[0].Value != "500" {
		t.Errorf("statuses under failed = %+v, want only the 500", failed.Statuses)
	}
	if len(failed.Users) != 1 || failed.Users[0].Count != 1 {
		t.Errorf("users under failed = %+v, want the one failed row that had a person "+
			"on it", failed.Users)
	}
}

// The cached share of a prompt is charged at its own rate, so a row that lost
// it is a row whose cost cannot be arrived at from the counts beside it -
// which is the state somebody reconciling against a provider's invoice is
// trying to get out of.
func TestRequestsCarryTheCachedShareOfThePrompt(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC().Truncate(time.Second)

	if err := st.WriteEvents(ctx, []Event{
		{TS: now.Add(-time.Minute), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-code", Status: 200, InputTokens: 1000, CachedInputTokens: 900,
			OutputTokens: 500, CostMicros: 2190},
		// A model served inside your own infrastructure caches nothing, which
		// has to come back as none cached rather than as absent.
		{TS: now, OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-code", Status: 200, InputTokens: 1000, OutputTokens: 500,
			CostMicros: 3000},
	}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	rows, err := st.Requests(ctx, RequestQuery{OrgID: f.orgID})
	if err != nil {
		t.Fatalf("Requests: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2", len(rows))
	}
	if rows[0].CachedInputTokens != 0 {
		t.Errorf("a request that cached nothing came back with %d cached",
			rows[0].CachedInputTokens)
	}
	if rows[1].InputTokens != 1000 || rows[1].CachedInputTokens != 900 {
		t.Errorf("tokens = %d in, %d cached; want the whole prompt 1000 and the "+
			"900 of it the provider had already read",
			rows[1].InputTokens, rows[1].CachedInputTokens)
	}
}
