package control

import (
	"cmp"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"slices"
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

// reportOrg is reportScope for a report that needs exactly one organisation.
func (s *Server) reportOrg(w http.ResponseWriter, r *http.Request,
	p *authn.Principal,
) (orgID string, from, to time.Time, ok bool) {
	orgID, from, to, ok = s.reportScope(w, r, p)
	if ok && orgID == "" {
		needOrg(w, orgRequired)
		return "", from, to, false
	}
	return orgID, from, to, ok
}

// page reads ?limit and ?before. Absent is zero: the store's default and the
// newest rows. A value that is not a number is refused, like a bad window.
func page(w http.ResponseWriter, q url.Values) (limit int, before int64, ok bool) {
	if limit, ok = pageLimit(w, q); !ok {
		return 0, 0, false
	}
	if v := q.Get("before"); v != "" {
		var err error
		if before, err = strconv.ParseInt(v, 10, 64); err != nil {
			badRequest(w, "'before' must be the id the previous page ended on")
			return 0, 0, false
		}
	}
	return limit, before, true
}

func pageLimit(w http.ResponseWriter, q url.Values) (int, bool) {
	v := q.Get("limit")
	if v == "" {
		return 0, true
	}
	limit, err := strconv.Atoi(v)
	if err != nil || limit < 0 {
		badRequest(w, "'limit' must be a whole number")
		return 0, false
	}
	return limit, true
}

// entityScope resolves the one project, key or person a report is narrowed to,
// and answers 404 for one in another organisation, so another tenant's key id
// cannot reveal their spend. A model alias is not checked: the rows are only
// this organisation's, so it reveals nothing. Across every organisation, which
// only an operator sees, it sums every model of that alias.
func (s *Server) entityScope(w http.ResponseWriter, r *http.Request,
	orgID string,
) (store.ReportScope, bool) {
	q := r.URL.Query()
	sc := store.ReportScope{
		ProjectID: q.Get("project_id"),
		KeyID:     q.Get("key_id"),
		UserID:    q.Get("user_id"),
		Alias:     q.Get("alias"),
	}
	// An operator looking across every tenant may read all of them.
	if orgID == "" {
		return sc, true
	}
	if sc.ProjectID != "" && !s.requireProjectInOrg(w, r, sc.ProjectID, orgID) {
		return sc, false
	}
	ctx := r.Context()
	if sc.KeyID != "" {
		o, err := s.st.KeyOwnerOf(ctx, sc.KeyID)
		if !s.inOrg(w, orgID, o.OrgID, err) {
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

// overview is the dashboard in one round trip. Narrowed by the query, it is
// also one project's, key's or model's own screen, which keeps that screen
// equal to its bar on the dashboard.
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
	names, err := s.st.ProjectNames(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := map[string]any{
		"project_names": names,
		"overview":      o,
	}
	// A narrowed overview breaks its traffic down by key, so it needs the key
	// names too.
	if !sc.Empty() {
		keys, err := s.st.KeyNames(r.Context(), orgID)
		if err != nil {
			s.fail(w, err)
			return
		}
		out["key_names"] = keys
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// requests is the event log: one row per request, whatever happened to it,
// narrowed to the entity whose screen is asking.
//
// An administrator reads the whole organisation's. A member reads only the
// requests made with their own keys: the rows carry messages from the
// inference plane, which are nobody else's business.
func (s *Server) requests(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	rq, from, to, ok := s.requestQuery(w, r, p)
	if !ok {
		return
	}
	orgID := rq.OrgID
	q := r.URL.Query()
	rq.From, rq.To = from, to
	// An id names a request whenever it was made. Someone holding one rarely
	// knows the day, and the period picked for browsing would hide it.
	if rq.RequestID != "" {
		rq.From, rq.To = time.Time{}, time.Time{}
	}
	if rq.Sort, ok = parseSort(w, q, store.RequestSorts()); !ok {
		return
	}
	if rq.Limit, rq.Before, ok = page(w, q); !ok {
		return
	}
	if rq.Sort != store.SortRecent {
		rq.Before = 0
	}

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
	if len(rows) > 0 && rq.Sort == store.SortRecent {
		out["next_before"] = rows[len(rows)-1].ID
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// parseSort reads a log's ranking, one of offered. "recent" and nothing at
// all are the default. One it does not offer is refused rather than ignored.
//
// Paging works only under the default order: a cursor from one order would
// drop rows in another, and whoever ranks by cost wants the top of the list.
func parseSort(w http.ResponseWriter, v url.Values, offered []store.Sort) (store.Sort, bool) {
	sort := store.Sort(v.Get("sort"))
	if sort == "recent" {
		sort = store.SortRecent
	}
	if slices.Contains(offered, sort) {
		return sort, true
	}
	names := make([]string, len(offered))
	for i, o := range offered {
		names[i] = cmp.Or(string(o), "recent")
	}
	badRequest(w, "sort must be one of "+strings.Join(names, ", "))
	return "", false
}

// requestQuery is what the request log, its stream and the session reports
// share: whose requests the caller may read, the organisation and entity
// narrowed to, and the outcome and status filters. The window is returned
// apart, because the stream has none. False means it already answered.
func (s *Server) requestQuery(w http.ResponseWriter, r *http.Request, p *authn.Principal) (
	rq store.RequestQuery, from, to time.Time, ok bool,
) {
	holder, ok := s.trafficHolder(w, p, r.URL.Query().Get("user_id"))
	if !ok {
		return rq, from, to, false
	}
	orgID, from, to, ok := s.reportScope(w, r, p)
	if !ok {
		return rq, from, to, false
	}
	sc, ok := s.entityScope(w, r, orgID)
	if !ok {
		return rq, from, to, false
	}
	q := r.URL.Query()
	outcome := store.Outcome(q.Get("outcome"))
	// Refused rather than read as every request, so a typo does not list
	// successes under a heading that says failures.
	if !store.ValidOutcome(outcome) {
		badRequest(w, "'outcome' must be one of "+strings.Join(store.Outcomes(), ", "))
		return rq, from, to, false
	}
	rq = store.RequestQuery{OrgID: orgID, ReportScope: sc, Holder: holder,
		Outcome:   outcome,
		RequestID: strings.TrimSpace(q.Get("request_id"))}
	rq.Status, rq.StatusClass = parseStatus(q.Get("status"))
	return rq, from, to, true
}

// trafficHolder is whose requests p may read one by one: empty for an
// administrator, who reads everyone's in the organisation, and the member
// themselves for a member, who reads those made with their own keys.
//
// A member who names someone else is refused rather than shown their own, so
// a request never does something other than what it says. It comes before
// any read, so a member cannot probe who exists.
func (s *Server) trafficHolder(w http.ResponseWriter, p *authn.Principal,
	named string) (string, bool) {
	if p.CanAdminOrg(p.OrgID) {
		return "", true
	}
	if p.UserID == "" || (named != "" && named != p.UserID) {
		forbid(w, "a member can only see the requests made with their own keys")
		return "", false
	}
	return p.UserID, true
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
		"project", "user", "input_tokens", "output_tokens",
		"cost",
		"latency_ms", "ttft_ms", "stream", "estimated", "canceled", "note",
	})
	for _, q := range rows {
		_ = cw.Write([]string{
			q.CreatedAt.Format(time.RFC3339), q.Alias, strconv.Itoa(q.Status), q.Error,
			names.label("key", q.KeyID), q.KeyID,
			names.label("project", q.ProjectID), names.label("user", q.UserID),
			strconv.FormatInt(q.InputTokens, 10), strconv.FormatInt(q.OutputTokens, 10),
			policy.FormatMicros(q.CostMicros),
			strconv.FormatInt(q.LatencyMS, 10), strconv.FormatInt(q.TTFTMS, 10),
			strconv.FormatBool(q.Stream), strconv.FormatBool(q.Estimated),
			strconv.FormatBool(q.Canceled), q.Note,
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
	// Refused, because a fallback would still echo the caller's string, even
	// into the CSV filename.
	if groupBy != "" && !store.ValidGroupBy(groupBy) {
		badRequest(w, "'group_by' must be one of "+strings.Join(store.GroupBys(), ", "))
		return
	}
	sc, ok := s.entityScope(w, r, orgID)
	if !ok {
		return
	}
	// Over more than two days the report counts whole days, so it says so.
	from, to, _ = store.DayWindow(from, to)
	uq := store.UsageQuery{
		OrgID: orgID, ReportScope: sc, From: from, To: to, GroupBy: groupBy,
	}
	buckets, err := s.st.Usage(r.Context(), uq)
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
	chart, err := s.st.UsageChart(r.Context(), uq)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := map[string]any{
		"from": from, "to": to, "data": buckets, "chart": chart,
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
	projects, keys, users map[string]string
}

func (s *Server) groupNames(ctx context.Context, orgID string) (groupLabels, error) {
	var (
		g   groupLabels
		err error
	)
	if g.projects, err = s.st.ProjectNames(ctx, orgID); err != nil {
		return g, err
	}
	if g.keys, err = s.st.KeyNames(ctx, orgID); err != nil {
		return g, err
	}
	if g.users, err = s.st.UserEmails(ctx, orgID); err != nil {
		return g, err
	}
	return g, nil
}

func (g groupLabels) addTo(out map[string]any) {
	out["project_names"] = g.projects
	out["key_names"] = g.keys
	out["user_names"] = g.users
}

func (g groupLabels) label(groupBy, id string) string {
	if id == "" {
		return "(none)"
	}
	var m map[string]string
	switch groupBy {
	case "project":
		m = g.projects
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
		groupBy = store.DefaultGroupBy
	}
	filename := fmt.Sprintf("keera-usage-%s-%s.csv", groupBy, to.Format("2006-01-02"))
	cw := beginCSV(w, filename)
	_ = cw.Write([]string{
		groupBy, groupBy + "_id", "requests", "input_tokens",
		"output_tokens", "cost", "org_id", "from", "to",
	})
	for _, b := range buckets {
		_ = cw.Write([]string{
			names.label(groupBy, b.Group), b.Group,
			strconv.FormatInt(b.Requests, 10),
			strconv.FormatInt(b.InputTokens, 10),
			strconv.FormatInt(b.OutputTokens, 10),
			policy.FormatMicros(b.CostMicros),
			cmp.Or(b.OrgID, orgID), from.Format(time.RFC3339), to.Format(time.RFC3339),
		})
	}
	cw.Flush()
}

// audit is administrator-only, because it names people and what they
// changed. An operator reads every tenant's, anybody else only their own. It
// filters and pages, because it is the compliance record.
func (s *Server) audit(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if !s.requireAdmin(w, p) {
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
	if aq.Limit, aq.Before, ok = page(w, q); !ok {
		return
	}

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
			e.CreatedAt.Format(time.RFC3339), e.Actor, e.Action, e.TargetType, e.TargetID,
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

// csvWriter is encoding/csv, except that no field can be read as a formula.
// Exports carry names and audit detail users wrote: a project called
// `=HYPERLINK(...)` would run in Excel, and quoting does not prevent that.
type csvWriter struct{ *csv.Writer }

// Write escapes every field in place, then writes the row.
func (c *csvWriter) Write(row []string) error {
	for i, field := range row {
		row[i] = csvSafe(field)
	}
	return c.Writer.Write(row)
}

// csvSafe prefixes a field a spreadsheet would evaluate with an apostrophe,
// which Excel, LibreOffice and Sheets all read as "this is text". A sign
// followed by a number is left alone, so cost columns still add up.
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
