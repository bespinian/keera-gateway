package control

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/connect"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// csvExportRows caps a CSV export. An export is the whole filtered window,
// not the page on screen, because it is read somewhere else.
const csvExportRows = 5000

// reportScope resolves the organisation and time window every report takes.
func (s *Server) reportScope(w http.ResponseWriter, r *http.Request,
	p *authn.Principal,
) (orgID string, from, to time.Time, ok bool) {
	q := r.URL.Query()
	orgID, ok = s.scopeOrg(w, p, q.Get("org_id"))
	if !ok {
		return "", from, to, false
	}
	from, to, ok = queryWindow(w, q)
	if !ok {
		return "", from, to, false
	}
	return orgID, from, to, true
}

// entityScope resolves the one team, key or person a report is narrowed to,
// and answers 404 for one in another organisation. Without this, another
// tenant's key id would reveal their spend.
//
// A model alias is not checked: the catalogue is shared, and the rows are
// still only this organisation's.
func (s *Server) entityScope(w http.ResponseWriter, r *http.Request,
	orgID string,
) (store.Scope, bool) {
	q := r.URL.Query()
	sc := store.Scope{
		TeamID: q.Get("team_id"),
		KeyID:  q.Get("key_id"),
		UserID: q.Get("user_id"),
		Alias:  q.Get("alias"),
	}
	// An operator looking across every tenant may read all of them.
	if orgID == "" {
		return sc, true
	}
	ctx := r.Context()
	if sc.TeamID != "" {
		owner, err := s.st.TeamOrg(ctx, sc.TeamID)
		if !s.inOrg(w, orgID, owner, err) {
			return sc, false
		}
	}
	if sc.KeyID != "" {
		owner, err := s.st.KeyOrg(ctx, sc.KeyID)
		if !s.inOrg(w, orgID, owner, err) {
			return sc, false
		}
	}
	if sc.UserID != "" {
		user, err := s.st.UserByID(ctx, sc.UserID)
		if !s.inOrg(w, orgID, user.OrgID, err) {
			return sc, false
		}
	}
	return sc, true
}

// inOrg checks the result of an owner lookup, answering 404 for an owner that
// is not orgID.
func (s *Server) inOrg(w http.ResponseWriter, orgID, owner string, err error) bool {
	if err != nil {
		s.fail(w, err)
		return false
	}
	if owner != orgID {
		s.fail(w, store.ErrNotFound)
		return false
	}
	return true
}

// overview is the dashboard in one round trip.
//
// Narrowed by the query, it is also one team's, key's or model's own screen.
// Answering both here keeps a team's own page equal to its bar on the
// dashboard.
func (s *Server) overview(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, from, to, ok := s.reportScope(w, r, p)
	if !ok {
		return
	}
	sc, ok := s.entityScope(w, r, orgID)
	if !ok {
		return
	}
	o, err := s.st.Overview(r.Context(), orgID, from, to, sc)
	if err != nil {
		s.fail(w, err)
		return
	}
	names, err := s.st.TeamNames(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := map[string]any{
		"currency":   s.opts.Currency,
		"team_names": names,
		"overview":   o,
	}
	// A narrowed overview breaks its traffic down by key, so it needs the key
	// names too.
	if !sc.Empty() {
		keys, err := s.st.KeyAliases(r.Context(), orgID)
		if err != nil {
			s.fail(w, err)
			return
		}
		out["key_aliases"] = keys
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// requests is the event log: one row per request, whatever happened to it,
// narrowed to the entity whose screen is asking.
//
// Administrator-only, like the failure log: the rows name other people's keys
// and carry messages from the inference plane. Members see their own traffic
// on My access.
func (s *Server) requests(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if !p.CanAdminOrg(p.OrgID) {
		s.forbid(w, "only an administrator can read the request log")
		return
	}
	orgID, from, to, ok := s.reportScope(w, r, p)
	if !ok {
		return
	}
	sc, ok := s.entityScope(w, r, orgID)
	if !ok {
		return
	}
	q := r.URL.Query()
	rq := store.RequestQuery{
		OrgID:   orgID,
		Scope:   sc,
		From:    from,
		To:      to,
		Outcome: store.Outcome(q.Get("outcome")),
	}
	rq.Status, rq.StatusClass = parseStatus(q.Get("status"))
	rq.Limit, _ = strconv.Atoi(q.Get("limit"))
	rq.Before, _ = strconv.ParseInt(q.Get("before"), 10, 64)

	names, err := s.groupNames(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	asCSV := q.Get("format") == "csv"
	if asCSV {
		rq.Before, rq.Limit = 0, csvExportRows
	}
	rows, err := s.st.Requests(r.Context(), rq)
	if err != nil {
		s.fail(w, err)
		return
	}
	if asCSV {
		s.requestsCSV(w, rows, names)
		return
	}
	out := map[string]any{
		"from": from, "to": to, "data": rows,
		"currency": s.opts.Currency,
	}
	names.addTo(out)
	// Only the first page carries the counts: they do not change while paging,
	// and they cost a second query.
	if rq.Before == 0 {
		counts, err := s.st.Outcomes(r.Context(), rq)
		if err != nil {
			s.fail(w, err)
			return
		}
		out["outcomes"] = counts

		// The filter choices, only when asked for. An entity's own screen is
		// already narrowed and draws no such filters.
		if httpx.Flag(q, "facets") {
			facets, err := s.st.RequestFilters(r.Context(), rq)
			if err != nil {
				s.fail(w, err)
				return
			}
			out["filters"] = facets
		}
	}
	if len(rows) > 0 {
		out["next_before"] = rows[len(rows)-1].ID
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// parseStatus reads the log's status filter: one exact status ("402") or a
// whole class ("5xx"). Anything else filters nothing, because a filter nobody
// can see they applied is worse than none.
func parseStatus(v string) (exact, class int) {
	if v == "" {
		return 0, 0
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n, 0
	}
	if c, ok := strings.CutSuffix(strings.ToLower(v), "xx"); ok && len(c) == 1 &&
		c[0] >= '1' && c[0] <= '5' {
		return 0, int(c[0] - '0')
	}
	return 0, 0
}

func (s *Server) requestsCSV(w http.ResponseWriter, rows []store.Request, names groupLabels) {
	cw := beginCSV(w, "keera-requests-"+time.Now().Format("2006-01-02")+".csv")
	_ = cw.Write([]string{
		"timestamp", "model", "status", "error", "key", "key_id",
		"team", "user", "input_tokens", "output_tokens",
		"cost_" + strings.ToLower(s.opts.Currency),
		"latency_ms", "ttft_ms", "stream", "estimated", "canceled",
	})
	for _, q := range rows {
		_ = cw.Write([]string{
			q.TS.Format(time.RFC3339), q.Alias, strconv.Itoa(q.Status), q.Error,
			names.label("key", q.KeyID), q.KeyID,
			names.label("team", q.TeamID), names.label("user", q.UserID),
			strconv.FormatInt(q.InputTokens, 10), strconv.FormatInt(q.OutputTokens, 10),
			policy.FormatMicros(q.CostMicros),
			strconv.FormatInt(q.LatencyMS, 10), strconv.FormatInt(q.TTFTMS, 10),
			strconv.FormatBool(q.Stream), strconv.FormatBool(q.Estimated),
			strconv.FormatBool(q.Canceled),
		})
	}
	cw.Flush()
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, from, to, ok := s.reportScope(w, r, p)
	if !ok {
		return
	}
	groupBy := r.URL.Query().Get("group_by")
	// An unknown grouping is refused. It used to fall back to the default
	// while the caller's string was echoed back, even into the CSV filename.
	if groupBy != "" && !store.ValidGroupBy(groupBy) {
		badRequest(w, "'group_by' must be one of "+strings.Join(store.GroupBys(), ", "))
		return
	}
	buckets, err := s.st.Usage(r.Context(), store.UsageQuery{
		OrgID: orgID, From: from, To: to, GroupBy: groupBy,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	names, err := s.groupNames(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if r.URL.Query().Get("format") == "csv" {
		s.usageCSV(w, orgID, groupBy, from, to, buckets, names)
		return
	}
	out := map[string]any{
		"from": from, "to": to, "currency": s.opts.Currency,
		"data": buckets,
	}
	names.addTo(out)
	// Client names come from the rows, not the database, so they are built
	// only for the client grouping.
	if groupBy == "client" {
		labels := make(map[string]string, len(buckets))
		for _, b := range buckets {
			if b.Group != "" {
				labels[b.Group] = connect.ClientLabel(b.Group)
			}
		}
		out["client_names"] = labels
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// groupLabels maps the ids a report groups by to the names people use. All
// three are sent every time: they are small, and readers switch groupings
// often.
type groupLabels struct {
	teams, keys, users map[string]string
}

func (s *Server) groupNames(ctx context.Context, orgID string) (groupLabels, error) {
	var (
		g   groupLabels
		err error
	)
	if g.teams, err = s.st.TeamNames(ctx, orgID); err != nil {
		return g, err
	}
	if g.keys, err = s.st.KeyAliases(ctx, orgID); err != nil {
		return g, err
	}
	if g.users, err = s.st.UserEmails(ctx, orgID); err != nil {
		return g, err
	}
	return g, nil
}

// addTo puts the labels into a report's response.
func (g groupLabels) addTo(out map[string]any) {
	out["team_names"] = g.teams
	out["key_aliases"] = g.keys
	out["user_names"] = g.users
}

func (g groupLabels) label(groupBy, id string) string {
	if id == "" {
		return "(none)"
	}
	var m map[string]string
	switch groupBy {
	case "team":
		m = g.teams
	case "key":
		m = g.keys
	case "user":
		m = g.users
	case "client":
		// A client is not a table row; its name comes from the client
		// catalogue.
		return connect.ClientLabel(id)
	default:
		return id
	}
	if name, ok := m[id]; ok && name != "" {
		return name
	}
	return id
}

// usageCSV writes the report as a spreadsheet, which is where finance
// reconciles a chargeback.
func (s *Server) usageCSV(w http.ResponseWriter, orgID, groupBy string, from, to time.Time,
	buckets []store.UsageBucket, names groupLabels,
) {
	if groupBy == "" {
		groupBy = "model"
	}
	filename := fmt.Sprintf("keera-usage-%s-%s.csv", groupBy, to.Format("2006-01-02"))
	cw := beginCSV(w, filename)
	_ = cw.Write([]string{
		groupBy, groupBy + "_id", "requests", "input_tokens",
		"output_tokens", "cost_" + strings.ToLower(s.opts.Currency), "org_id", "from", "to",
	})
	for _, b := range buckets {
		_ = cw.Write([]string{
			names.label(groupBy, b.Group), b.Group,
			strconv.FormatInt(b.Requests, 10),
			strconv.FormatInt(b.InputTokens, 10),
			strconv.FormatInt(b.OutputTokens, 10),
			policy.FormatMicros(b.CostMicros),
			orgID, from.Format(time.RFC3339), to.Format(time.RFC3339),
		})
	}
	cw.Flush()
}

// failures opens up the dashboard's failure count: one row per request that
// did not deliver, with the message the client got. It tells a model that is
// down from a budget that ran out from a field the backend does not support.
//
// Administrator-only, like the audit log: rows name other people's keys, and
// an upstream error can name a backend.
func (s *Server) failures(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if !p.CanAdminOrg(p.OrgID) {
		s.forbid(w, "only an administrator can read the failure log")
		return
	}
	orgID, from, to, ok := s.reportScope(w, r, p)
	if !ok {
		return
	}
	q := r.URL.Query()
	fq := store.FailureQuery{
		OrgID:  orgID,
		From:   from,
		To:     to,
		Alias:  q.Get("alias"),
		KeyID:  q.Get("key_id"),
		TeamID: q.Get("team_id"),
		Kind:   store.FailureKind(q.Get("kind")),
	}
	fq.Status, _ = strconv.Atoi(q.Get("status"))
	fq.Limit, _ = strconv.Atoi(q.Get("limit"))
	fq.Before, _ = strconv.ParseInt(q.Get("before"), 10, 64)

	names, err := s.groupNames(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	asCSV := q.Get("format") == "csv"
	if asCSV {
		fq.Before, fq.Limit = 0, csvExportRows
	}
	rows, err := s.st.Failures(r.Context(), fq)
	if err != nil {
		s.fail(w, err)
		return
	}
	if asCSV {
		s.failuresCSV(w, rows, names)
		return
	}
	out := map[string]any{"from": from, "to": to, "data": rows}
	names.addTo(out)
	// Only the first page carries the filter choices: they do not change
	// while paging, and they cost a second query.
	if fq.Before == 0 {
		facets, err := s.st.FailureFilters(r.Context(), fq)
		if err != nil {
			s.fail(w, err)
			return
		}
		out["filters"] = facets
	}
	if len(rows) > 0 {
		out["next_before"] = rows[len(rows)-1].ID
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) failuresCSV(w http.ResponseWriter, rows []store.Failure, names groupLabels) {
	cw := beginCSV(w, "keera-failures-"+time.Now().Format("2006-01-02")+".csv")
	_ = cw.Write([]string{
		"timestamp", "model", "status", "error", "key", "key_id",
		"team", "user", "latency_ms", "stream", "canceled",
	})
	for _, f := range rows {
		_ = cw.Write([]string{
			f.TS.Format(time.RFC3339), f.Alias, strconv.Itoa(f.Status), f.Error,
			names.label("key", f.KeyID), f.KeyID,
			names.label("team", f.TeamID), names.label("user", f.UserID),
			strconv.FormatInt(f.LatencyMS, 10),
			strconv.FormatBool(f.Stream), strconv.FormatBool(f.Canceled),
		})
	}
	cw.Flush()
}

func (s *Server) spend(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	q := r.URL.Query()
	scope := policy.ScopeType(q.Get("scope_type"))
	scopeID := q.Get("scope_id")
	if !s.requireScopeRead(w, r, p, scope, scopeID) {
		return
	}
	period := policy.Period(q.Get("period"))
	if !period.Valid() {
		period = policy.PeriodMonth
	}
	now := time.Now()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"scope_type":   scope,
		"scope_id":     scopeID,
		"period":       period,
		"period_start": period.Start(now),
		"currency":     s.opts.Currency,
		"micros":       s.reg.Budgets().Spent(scope, scopeID, period, now),
	})
}

// audit is administrator-only, because it names people and what they
// changed. An operator reads every tenant's, anybody else only their own.
//
// It can be filtered and paged, because it is the product's compliance
// record and has to answer real questions.
func (s *Server) audit(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if !p.CanAdminOrg(p.OrgID) {
		s.forbid(w, "only an administrator can read the audit log")
		return
	}
	q := r.URL.Query()
	orgID, ok := s.scopeOrg(w, p, q.Get("org_id"))
	if !ok {
		return
	}
	aq := store.AuditQuery{OrgID: orgID, Actor: q.Get("actor"), Action: q.Get("action")}
	if q.Get("since") != "" || q.Get("from") != "" || q.Get("to") != "" {
		if aq.From, aq.To, ok = queryWindow(w, q); !ok {
			return
		}
	}
	aq.Limit, _ = strconv.Atoi(q.Get("limit"))
	aq.Before, _ = strconv.ParseInt(q.Get("before"), 10, 64)

	// Half an audit trail is worse than none, so an export is never a page.
	asCSV := q.Get("format") == "csv"
	if asCSV {
		aq.Before, aq.Limit = 0, csvExportRows
	}
	entries, err := s.st.ListAudit(r.Context(), aq)
	if err != nil {
		s.fail(w, err)
		return
	}
	if asCSV {
		s.auditCSV(w, entries)
		return
	}
	out := map[string]any{"data": entries}
	// Only the first page carries the filter choices: they do not change
	// while paging, and they cost a second query.
	if aq.Before == 0 {
		facets, err := s.st.AuditFilters(r.Context(), orgID)
		if err != nil {
			s.fail(w, err)
			return
		}
		out["filters"] = facets
	}
	if len(entries) > 0 {
		out["next_before"] = entries[len(entries)-1].ID
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) auditCSV(w http.ResponseWriter, entries []store.AuditEntry) {
	cw := beginCSV(w, "keera-audit-"+time.Now().Format("2006-01-02")+".csv")
	_ = cw.Write([]string{"timestamp", "actor", "action", "target_type", "target_id", "detail"})
	for _, e := range entries {
		_ = cw.Write([]string{
			e.TS.Format(time.RFC3339), e.Actor, e.Action, e.TargetType, e.TargetID,
			string(e.Detail),
		})
	}
	cw.Flush()
}

// beginCSV writes the headers that make a browser save the response as a file.
func beginCSV(w http.ResponseWriter, filename string) *csvWriter {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	// FormatMediaType quotes the filename, so a quote in it cannot add a
	// parameter. It returns "" for a name it cannot encode; then a plain name
	// is used.
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": filename})
	if disposition == "" {
		disposition = `attachment; filename="keera-export.csv"`
	}
	w.Header().Set("Content-Disposition", disposition)
	w.WriteHeader(http.StatusOK)
	return &csvWriter{csv.NewWriter(w)}
}

// csvWriter is encoding/csv, except that no field can be read as a formula by
// the spreadsheet that opens it.
//
// The exports carry names and audit detail that users wrote. A team called
// `=HYPERLINK(...)` would run as a formula in Excel, and quoting does not
// prevent that.
type csvWriter struct{ *csv.Writer }

// Write escapes every field in place, then writes the row.
func (c *csvWriter) Write(row []string) error {
	for i, field := range row {
		row[i] = csvSafe(field)
	}
	return c.Writer.Write(row)
}

// csvSafe prefixes a field a spreadsheet would evaluate with an apostrophe,
// which Excel, LibreOffice and Sheets all read as "this is text".
//
// A leading sign followed by a number is left alone, so cost columns still
// add up.
func csvSafe(field string) string {
	if field == "" {
		return field
	}
	switch field[0] {
	case '=', '@', '\t', '\r':
	case '+', '-':
		if _, err := strconv.ParseFloat(field, 64); err == nil {
			return field
		}
	default:
		return field
	}
	return "'" + field
}

// setup says what a new deployment already has, so the panel can show which
// first-run step comes next.
func (s *Server) setup(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.scopeOrg(w, p, r.URL.Query().Get("org_id"))
	if !ok {
		return
	}
	st, err := s.st.SetupState(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, st)
}

// timeRange resolves a window from either a pair of timestamps or a duration.
// The default is the last 30 days, the window of a monthly invoice and budget.
func timeRange(fromStr, toStr, since string) (from, to time.Time, err error) {
	to = time.Now()
	from = to.AddDate(0, 0, -30)
	if since != "" {
		d, perr := time.ParseDuration(since)
		if perr != nil || d <= 0 {
			return from, to, errors.New("'since' must be a positive duration such as 24h")
		}
		from = to.Add(-d)
	}
	if fromStr != "" {
		if from, err = time.Parse(time.RFC3339, fromStr); err != nil {
			return from, to, errors.New("'from' must be an RFC3339 timestamp")
		}
	}
	if toStr != "" {
		if to, err = time.Parse(time.RFC3339, toStr); err != nil {
			return from, to, errors.New("'to' must be an RFC3339 timestamp")
		}
	}
	if !from.Before(to) {
		return from, to, errors.New("'from' must be before 'to'")
	}
	return from, to, nil
}
