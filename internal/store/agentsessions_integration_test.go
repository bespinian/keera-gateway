package store

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSessionsGroupRequestsIntoTheTasksTheyWereMadeFor(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC().Truncate(time.Second)
	conversation := DerivedSessionKey("aaaaaaaaaaaaaaaa")

	// One task: four served calls a minute apart, and a fifth the budget
	// refused.
	events := make([]Event, 0, 6)
	for i := range 4 {
		events = append(events, Event{
			TS:    now.Add(-3*time.Hour + time.Duration(i)*time.Minute),
			OrgID: f.orgID, ProjectID: f.projectID, KeyID: f.keyID, Alias: "keera-code",
			SessionKey: conversation, InputTokens: 1000, OutputTokens: 100,
			CostMicros: 500, Status: 200, Latency: 2 * time.Second,
			TTFT: 400 * time.Millisecond, Stream: true,
		})
	}
	events = append(events, Event{
		TS:    now.Add(-3*time.Hour + 4*time.Minute),
		OrgID: f.orgID, ProjectID: f.projectID, KeyID: f.keyID, Alias: "keera-code",
		SessionKey: conversation, Status: 402, Error: "your project has spent its month budget",
	})
	// The same conversation two hours later, with the same key: a new task.
	events = append(events, Event{
		TS: now.Add(-time.Hour), OrgID: f.orgID, ProjectID: f.projectID, KeyID: f.keyID,
		Alias: "keera-speed", SessionKey: conversation, CostMicros: 250,
		InputTokens: 40, OutputTokens: 8, Status: 200, Latency: time.Second,
		TTFT: 200 * time.Millisecond,
	})
	// A request in no conversation, such as an embedding, is no session.
	events = append(events, Event{
		TS: now.Add(-90 * time.Minute), OrgID: f.orgID, ProjectID: f.projectID,
		KeyID: f.keyID, Alias: "keera-embed", Status: 200, CostMicros: 10,
	})
	if err := st.WriteEvents(ctx, events); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	q := AgentSessionQuery{Gap: DefaultSessionGap, OrgID: f.orgID, From: now.Add(-24 * time.Hour), To: now.Add(time.Minute)}
	sessions, err := st.AgentSessions(ctx, q)
	if err != nil {
		t.Fatalf("AgentSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("%d sessions, want 2 - one task each side of the idle gap, and "+
			"nothing for the request that had no conversation", len(sessions))
	}

	// Newest first.
	recent, task := sessions[0], sessions[1]
	if !recent.StartedAt.After(task.StartedAt) {
		t.Fatalf("sessions are not newest-first: %v then %v", recent.StartedAt, task.StartedAt)
	}
	if recent.Requests != 1 || task.Requests != 5 {
		t.Errorf("requests = %d and %d, want 1 and 5", recent.Requests, task.Requests)
	}
	if task.OK != 4 || task.Refused != 1 || task.Failed != 0 || task.Interrupted != 0 {
		t.Errorf("outcomes = %+v, want four served and one refused", task)
	}
	if task.CostMicros != 2000 || task.InputTokens != 4000 || task.OutputTokens != 400 {
		t.Errorf("cost/tokens = %d/%d/%d, want 2000/4000/400 - the refused call "+
			"cost nothing and must not have added any",
			task.CostMicros, task.InputTokens, task.OutputTokens)
	}
	if task.LastStatus != 402 || !strings.Contains(task.LastError, "budget") {
		t.Errorf("how it ended = %d %q, want the refusal the last call was given",
			task.LastStatus, task.LastError)
	}
	if task.Failed+task.Refused+task.Interrupted == 0 {
		t.Error("a task that ended on a refusal does not read as unhappy")
	}
	if len(task.Models) != 1 || task.Models[0] != "keera-code" {
		t.Errorf("models = %v, want just keera-code", task.Models)
	}
	if task.Stated {
		t.Error("a key derived from the opening prompt must not read as one the client stated")
	}
	// The duration runs to the end of the last call, not its start.
	if got := task.Duration(); got != 4*time.Minute {
		t.Errorf("duration = %s, want 4m", got)
	}
	if task.TTFTMedianMS != 400 {
		t.Errorf("median first token = %d ms, want 400 - the refused call waited "+
			"for no token and must not be in the median", task.TTFTMedianMS)
	}

	// A session is named by the request that opened it.
	rows, err := st.Requests(ctx, RequestQuery{OrgID: f.orgID})
	if err != nil {
		t.Fatalf("Requests: %v", err)
	}
	var opening int64
	for _, r := range rows {
		if r.CreatedAt.Equal(now.Add(-3 * time.Hour)) {
			opening = r.ID
		}
	}
	if opening == 0 {
		t.Fatal("the opening request is not in the log")
	}
	if task.ID != opening {
		t.Errorf("session id = %d, want %d - the id of the request that started it",
			task.ID, opening)
	}
}

func TestAStatedSessionKeyIsMarkedAsOne(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC()
	if err := st.WriteEvents(ctx, []Event{{
		TS: now.Add(-time.Minute), OrgID: f.orgID, KeyID: f.keyID, Alias: "keera-code",
		SessionKey: statedSessionKey("bbbbbbbbbbbbbbbb"), Status: 200, CostMicros: 100,
	}}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}
	sessions, err := st.AgentSessions(ctx, AgentSessionQuery{Gap: DefaultSessionGap, OrgID: f.orgID})
	if err != nil {
		t.Fatalf("AgentSessions: %v", err)
	}
	if len(sessions) != 1 || !sessions[0].Stated {
		t.Errorf("sessions = %+v, want one marked as named by the client", sessions)
	}
}

// A model filter must not cut a task where the agent switched models.
func TestNarrowingToAModelKeepsTheSessionWhole(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC().Truncate(time.Second)
	conversation := DerivedSessionKey("cccccccccccccccc")

	// One task that switched to the hosted model in the middle and back.
	for i, alias := range []string{"keera-code", "keera-frontier", "keera-code"} {
		if err := st.WriteEvents(ctx, []Event{{
			TS:    now.Add(-time.Hour + time.Duration(i)*time.Minute),
			OrgID: f.orgID, ProjectID: f.projectID, KeyID: f.keyID, Alias: alias,
			SessionKey: conversation, Status: 200, CostMicros: 100, Latency: time.Second,
		}}); err != nil {
			t.Fatalf("WriteEvents: %v", err)
		}
	}

	hosted, err := st.AgentSessions(ctx, AgentSessionQuery{
		Gap: DefaultSessionGap, OrgID: f.orgID, Alias: "keera-frontier",
		From: now.Add(-24 * time.Hour), To: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("AgentSessions(alias): %v", err)
	}
	if len(hosted) != 1 {
		t.Fatalf("%d sessions used the hosted model, want 1", len(hosted))
	}
	if hosted[0].Requests != 3 || hosted[0].CostMicros != 300 {
		t.Errorf("the session that touched the hosted model = %d calls costing %d, "+
			"want the whole task: 3 calls costing 300 - what it cost is what the "+
			"task cost, not what the one hosted call inside it cost",
			hosted[0].Requests, hosted[0].CostMicros)
	}
	if len(hosted[0].Models) != 2 {
		t.Errorf("models = %v, want both of them", hosted[0].Models)
	}

	none, err := st.AgentSessions(ctx, AgentSessionQuery{Gap: DefaultSessionGap, OrgID: f.orgID, Alias: "keera-speed"})
	if err != nil {
		t.Fatalf("AgentSessions(unused alias): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("%d sessions for a model nothing used", len(none))
	}
}

func TestASessionIsFoundFromAnyRequestInIt(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC().Truncate(time.Second)
	conversation := DerivedSessionKey("dddddddddddddddd")

	for i := range 3 {
		if err := st.WriteEvents(ctx, []Event{{
			TS:    now.Add(-2*time.Hour + time.Duration(i)*time.Minute),
			OrgID: f.orgID, ProjectID: f.projectID, KeyID: f.keyID, Alias: "keera-code",
			SessionKey: conversation, Status: 200, CostMicros: 100, Latency: time.Second,
		}}); err != nil {
			t.Fatalf("WriteEvents: %v", err)
		}
	}
	// A later task on the same conversation, which must not be included.
	if err := st.WriteEvents(ctx, []Event{{
		TS: now, OrgID: f.orgID, ProjectID: f.projectID, KeyID: f.keyID, Alias: "keera-code",
		SessionKey: conversation, Status: 200, CostMicros: 100,
	}, {
		// And a request with no conversation.
		TS: now, OrgID: f.orgID, KeyID: f.keyID, Alias: "keera-embed", Status: 200,
	}}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	rows, err := st.Requests(ctx, RequestQuery{OrgID: f.orgID})
	if err != nil {
		t.Fatalf("Requests: %v", err)
	}
	var middle, loose int64
	for _, r := range rows {
		if r.CreatedAt.Equal(now.Add(-2*time.Hour + time.Minute)) {
			middle = r.ID
		}
		if r.SessionKey == "" {
			loose = r.ID
		}
	}
	if middle == 0 || loose == 0 {
		t.Fatal("the rows this test needs are not in the log")
	}

	session, requests, err := st.AgentSessionAt(ctx, f.orgID, middle, DefaultSessionGap)
	if err != nil {
		t.Fatalf("AgentSessionAt(the middle request): %v", err)
	}
	if session.Requests != 3 || len(requests) != 3 {
		t.Fatalf("session = %d calls with %d rows, want 3 and 3 - the later task on "+
			"the same conversation is a different session", session.Requests, len(requests))
	}
	// Oldest first, unlike every other log.
	if !requests[0].CreatedAt.Before(requests[2].CreatedAt) {
		t.Errorf("the requests are not oldest-first: %v then %v", requests[0].CreatedAt, requests[2].CreatedAt)
	}
	if session.ID != requests[0].ID {
		t.Errorf("session id = %d, want the opening request's %d", session.ID, requests[0].ID)
	}
	// The summary and the rows are two queries that must agree.
	var summed int64
	for _, r := range requests {
		summed += r.CostMicros
	}
	if summed != session.CostMicros {
		t.Errorf("the summary says %d and its rows add up to %d", session.CostMicros, summed)
	}

	// The opening request finds the same session as the middle one did.
	opening, _, err := st.AgentSessionAt(ctx, f.orgID, requests[0].ID, DefaultSessionGap)
	if err != nil {
		t.Fatalf("AgentSessionAt(the opening request): %v", err)
	}
	if opening.ID != session.ID {
		t.Errorf("the opening request found session %d, the middle one found %d",
			opening.ID, session.ID)
	}

	// A request in no conversation is in no session: ErrNotFound.
	if _, _, err := st.AgentSessionAt(ctx, f.orgID, loose, DefaultSessionGap); !errors.Is(err, ErrNotFound) {
		t.Errorf("a request with no conversation gave %v, want ErrNotFound", err)
	}
	// Nor is another tenant's request.
	if _, _, err := st.AgentSessionAt(ctx, "org_other", middle, DefaultSessionGap); !errors.Is(err, ErrNotFound) {
		t.Errorf("another tenant's session gave %v, want ErrNotFound", err)
	}
}

// A task straddling the window's start is reported whole.
func TestASessionStraddlingTheWindowIsReportedWhole(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC().Truncate(time.Second)
	conversation := DerivedSessionKey("eeeeeeeeeeeeeeee")
	from := now.Add(-time.Hour)

	// Four calls five minutes apart, two on each side of the window's start.
	for i := range 4 {
		if err := st.WriteEvents(ctx, []Event{{
			TS:    from.Add(time.Duration(i-2) * 5 * time.Minute),
			OrgID: f.orgID, KeyID: f.keyID, Alias: "keera-code",
			SessionKey: conversation, Status: 200, CostMicros: 100, Latency: time.Second,
		}}); err != nil {
			t.Fatalf("WriteEvents: %v", err)
		}
	}
	// A task that finished well before the window.
	if err := st.WriteEvents(ctx, []Event{{
		TS: from.Add(-3 * time.Hour), OrgID: f.orgID, KeyID: f.keyID, Alias: "keera-code",
		SessionKey: conversation, Status: 200, CostMicros: 999,
	}}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	sessions, err := st.AgentSessions(ctx, AgentSessionQuery{
		Gap: DefaultSessionGap, OrgID: f.orgID, From: from, To: now,
	})
	if err != nil {
		t.Fatalf("AgentSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("%d sessions, want 1 - the one running when the window opened, and "+
			"not the one that had already finished", len(sessions))
	}
	if sessions[0].Requests != 4 || sessions[0].CostMicros != 400 {
		t.Errorf("the straddling session = %d calls costing %d, want all 4 costing 400",
			sessions[0].Requests, sessions[0].CostMicros)
	}
	if !sessions[0].StartedAt.Before(from) {
		t.Errorf("it starts at %v, which is not before the window it was read in", sessions[0].StartedAt)
	}
}

// Sessions rank by cost, tokens, requests and duration.
func TestSessionsRankByWhatMakesOneWorthOpening(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC().Truncate(time.Second)

	// Three tasks: an expensive short one, a long cheap one, and an ordinary one.
	write := func(key string, at time.Time, calls int, micros int64, spacing time.Duration) {
		for i := range calls {
			if err := st.WriteEvents(ctx, []Event{{
				TS: at.Add(time.Duration(i) * spacing), OrgID: f.orgID, KeyID: f.keyID,
				Alias: "keera-code", SessionKey: DerivedSessionKey(key), Status: 200,
				CostMicros: micros, InputTokens: 100, Latency: time.Second,
			}}); err != nil {
				t.Fatalf("WriteEvents: %v", err)
			}
		}
	}
	write("expensive0000000", now.Add(-5*time.Hour), 2, 5000, time.Minute)
	write("runaway000000000", now.Add(-4*time.Hour), 40, 50, time.Minute)
	write("ordinary00000000", now.Add(-2*time.Hour), 6, 200, time.Minute)

	window := AgentSessionQuery{Gap: DefaultSessionGap, OrgID: f.orgID, From: now.Add(-24 * time.Hour), To: now.Add(time.Hour)}
	for _, tc := range []struct {
		sort Sort
		want string
	}{
		{SortCost, "expensive0000000"},
		{SortTokens, "runaway000000000"},
		{SortRequests, "runaway000000000"},
		{SortDuration, "runaway000000000"},
		{SortRecent, "ordinary00000000"},
	} {
		q := window
		q.Sort = tc.sort
		got, err := st.AgentSessions(ctx, q)
		if err != nil {
			t.Fatalf("AgentSessions(%q): %v", tc.sort, err)
		}
		if len(got) != 3 {
			t.Fatalf("%d sessions under %q, want 3", len(got), tc.sort)
		}
		if got[0].SessionKey != DerivedSessionKey(tc.want) {
			t.Errorf("%q puts %q first, want %q", tc.sort, got[0].SessionKey, tc.want)
		}
	}

	// The totals cover the window, not the page, and use medians.
	page := window
	page.Limit = 1
	totals, err := st.AgentSessionSummary(ctx, page)
	if err != nil {
		t.Fatalf("AgentSessionSummary: %v", err)
	}
	if totals.Sessions != 3 || totals.Requests != 48 {
		t.Errorf("totals = %d sessions and %d calls, want 3 and 48 over the whole "+
			"window even though the page holds one", totals.Sessions, totals.Requests)
	}
	if totals.CostMicros != 2*5000+40*50+6*200 {
		t.Errorf("total cost = %d, want %d", totals.CostMicros, 2*5000+40*50+6*200)
	}
	if totals.RequestsMedian != 6 {
		t.Errorf("median calls per task = %d, want 6 - the mean would be 16 and "+
			"would describe none of these three tasks", totals.RequestsMedian)
	}
	if totals.LongestRequests != 40 {
		t.Errorf("longest = %d calls, want 40", totals.LongestRequests)
	}
	if totals.Unhappy != 0 {
		t.Errorf("%d unhappy sessions, want none", totals.Unhappy)
	}
}

// A session holding a failure counts as failed, and only one where everything
// was served counts as served. The counts and filter choices cover the window.
func TestSessionsNarrowByOutcomeAndOfferWhatOccurred(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC().Truncate(time.Second)

	write := func(key, user, alias string, statuses ...int) {
		for i, status := range statuses {
			if err := st.WriteEvents(ctx, []Event{{
				TS: now.Add(-3*time.Hour + time.Duration(i)*time.Minute), OrgID: f.orgID,
				ProjectID: f.projectID, KeyID: f.keyID, UserID: user, Alias: alias,
				SessionKey: DerivedSessionKey(key), Status: status, CostMicros: 10,
			}}); err != nil {
				t.Fatalf("WriteEvents: %v", err)
			}
		}
	}
	write("clean00000000000", "user_1", "keera-code", 200, 200)
	write("failing000000000", "user_1", "keera-code", 200, 500, 200)
	write("refused000000000", "user_2", "keera-fast", 402)

	window := AgentSessionQuery{
		Gap: DefaultSessionGap, OrgID: f.orgID,
		From: now.Add(-24 * time.Hour), To: now.Add(time.Hour),
	}
	for _, tc := range []struct {
		outcome Outcome
		want    int
	}{
		{OutcomeAny, 3},
		{OutcomeOK, 1},
		{OutcomeFailed, 1},
		{OutcomeRefused, 1},
		{OutcomeInterrupted, 0},
		{OutcomeUnhappy, 2},
	} {
		q := window
		q.Outcome = tc.outcome
		got, err := st.AgentSessions(ctx, q)
		if err != nil {
			t.Fatalf("AgentSessions(%q): %v", tc.outcome, err)
		}
		if len(got) != tc.want {
			t.Errorf("%d sessions under %q, want %d", len(got), tc.outcome, tc.want)
		}
	}

	// The totals follow the chosen outcome; the outcome counts do not.
	failed := window
	failed.Outcome = OutcomeFailed
	totals, err := st.AgentSessionSummary(ctx, failed)
	if err != nil {
		t.Fatalf("AgentSessionSummary: %v", err)
	}
	if totals.Sessions != 1 || totals.Requests != 3 {
		t.Errorf("totals under failed = %d sessions, %d calls, want 1 and 3",
			totals.Sessions, totals.Requests)
	}
	want := RequestOutcomes{Total: 3, OK: 1, Failed: 1, Refused: 1}
	if totals.Outcomes != want {
		t.Errorf("outcome counts = %+v, want %+v", totals.Outcomes, want)
	}

	// Picking one model still offers the other.
	picked := window
	picked.Alias = "keera-code"
	facets, err := st.AgentSessionFilters(ctx, picked)
	if err != nil {
		t.Fatalf("AgentSessionFilters: %v", err)
	}
	if len(facets.Models) != 2 || facets.Models[0] != (FacetCount{"keera-code", 2}) {
		t.Errorf("models = %+v, want keera-code with 2 sessions, then keera-fast",
			facets.Models)
	}
	if len(facets.Users) != 2 || len(facets.Keys) != 1 || facets.Keys[0].Count != 3 {
		t.Errorf("facets = %+v, want both people and the one key with 3 sessions", facets)
	}
	// A session's status is its last, so one that recovered counts under 200.
	wantStatuses := []FacetCount{{"200", 2}, {"402", 1}}
	if !slices.Equal(facets.Statuses, wantStatuses) {
		t.Errorf("statuses = %+v, want %+v", facets.Statuses, wantStatuses)
	}
	for _, tc := range []struct {
		status, class, want int
	}{
		{402, 0, 1},
		{0, 2, 2},
		{0, 5, 0},
	} {
		q := window
		q.Status, q.StatusClass = tc.status, tc.class
		got, err := st.AgentSessions(ctx, q)
		if err != nil {
			t.Fatalf("AgentSessions(status %d, class %d): %v", tc.status, tc.class, err)
		}
		if len(got) != tc.want {
			t.Errorf("%d sessions ended with status %d / class %d, want %d",
				len(got), tc.status, tc.class, tc.want)
		}
	}

	// A member's choices are their own.
	mine := window
	mine.Holder = "user_2"
	facets, err = st.AgentSessionFilters(ctx, mine)
	if err != nil {
		t.Fatalf("AgentSessionFilters(holder): %v", err)
	}
	if len(facets.Users) != 1 || facets.Users[0].Value != "user_2" {
		t.Errorf("a member is offered %+v, want only themselves", facets.Users)
	}
	mine.UserID = "user_1"
	got, err := st.AgentSessions(ctx, mine)
	if err != nil {
		t.Fatalf("AgentSessions(holder): %v", err)
	}
	if len(got) != 1 || got[0].UserID != "user_2" {
		t.Errorf("a member reads %+v, want only their own session", got)
	}
}

func TestSessionsStayInsideOneTenantAndNarrowToOneEntity(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC().Truncate(time.Second)

	if err := st.WriteEvents(ctx, []Event{{
		TS: now.Add(-time.Hour), OrgID: f.orgID, ProjectID: f.projectID, KeyID: f.keyID,
		UserID: "user_1", Alias: "keera-code", SessionKey: DerivedSessionKey("ours000000000000"),
		Status: 200, CostMicros: 100,
	}, {
		TS: now.Add(-time.Hour), OrgID: "org_other", ProjectID: "project_other",
		KeyID: "key_other", Alias: "keera-code", SessionKey: DerivedSessionKey("theirs0000000000"),
		Status: 500, Error: "CUDA out of memory",
	}}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	ours, err := st.AgentSessions(ctx, AgentSessionQuery{Gap: DefaultSessionGap, OrgID: f.orgID})
	if err != nil {
		t.Fatalf("AgentSessions: %v", err)
	}
	if len(ours) != 1 || ours[0].SessionKey != DerivedSessionKey("ours000000000000") {
		t.Fatalf("sessions = %+v, want only this tenant's", ours)
	}

	for _, tc := range []struct {
		name string
		q    AgentSessionQuery
		want int
	}{
		{"this project", AgentSessionQuery{Gap: DefaultSessionGap, OrgID: f.orgID, ProjectID: f.projectID}, 1},
		{"another project", AgentSessionQuery{Gap: DefaultSessionGap, OrgID: f.orgID, ProjectID: "project_other"}, 0},
		{"this key", AgentSessionQuery{Gap: DefaultSessionGap, OrgID: f.orgID, KeyID: f.keyID}, 1},
		{"this person", AgentSessionQuery{Gap: DefaultSessionGap, OrgID: f.orgID, UserID: "user_1"}, 1},
		{"one conversation", AgentSessionQuery{Gap: DefaultSessionGap, OrgID: f.orgID, SessionKey: DerivedSessionKey("ours000000000000")}, 1},
		{"only the unhappy ones", AgentSessionQuery{Gap: DefaultSessionGap, OrgID: f.orgID, Outcome: OutcomeUnhappy}, 0},
	} {
		got, err := st.AgentSessions(ctx, tc.q)
		if err != nil {
			t.Fatalf("AgentSessions(%s): %v", tc.name, err)
		}
		if len(got) != tc.want {
			t.Errorf("%s = %d sessions, want %d", tc.name, len(got), tc.want)
		}
	}

	// An operator reads across tenants.
	all, err := st.AgentSessions(ctx, AgentSessionQuery{Gap: DefaultSessionGap})
	if err != nil {
		t.Fatalf("AgentSessions(every tenant): %v", err)
	}
	if len(all) != 2 {
		t.Errorf("%d sessions across every tenant, want 2", len(all))
	}
	failing, err := st.AgentSessions(ctx, AgentSessionQuery{Gap: DefaultSessionGap, Outcome: OutcomeUnhappy})
	if err != nil {
		t.Fatalf("AgentSessions(unhappy): %v", err)
	}
	if len(failing) != 1 || failing[0].Failed != 1 {
		t.Errorf("unhappy sessions = %+v, want the one that failed", failing)
	}
}

// The gap is not stored, so changing it re-cuts sessions already recorded.
func TestTheIdleGapIsAppliedAtReadTime(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC().Truncate(time.Second)
	conversation := DerivedSessionKey("ffffffffffffffff")

	// Two calls ten minutes apart: one task under a wide gap, two under a
	// narrow one.
	for i := range 2 {
		if err := st.WriteEvents(ctx, []Event{{
			TS:    now.Add(-time.Hour + time.Duration(i)*10*time.Minute),
			OrgID: f.orgID, KeyID: f.keyID, Alias: "keera-code",
			SessionKey: conversation, Status: 200, CostMicros: 100,
		}}); err != nil {
			t.Fatalf("WriteEvents: %v", err)
		}
	}

	for _, tc := range []struct {
		gap  time.Duration
		want int
	}{
		{30 * time.Minute, 1},
		{5 * time.Minute, 2},
	} {
		got, err := st.AgentSessions(ctx, AgentSessionQuery{OrgID: f.orgID, Gap: tc.gap})
		if err != nil {
			t.Fatalf("AgentSessions(gap %s): %v", tc.gap, err)
		}
		if len(got) != tc.want {
			t.Errorf("a gap of %s cuts these two calls into %d sessions, want %d",
				tc.gap, len(got), tc.want)
		}
	}
}
