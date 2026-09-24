package control

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// The session reports: the request log grouped into the tasks the requests
// were made for. "This task cost 1.20, made 41 calls and took 11 minutes" is
// what a budget talk needs, and it shows an agent going round in circles.
//
// Administrator-only, like the request log: the rows name other people's keys
// and carry text from the inference plane.

// sessionSorts maps the orderings this API offers to the store's, so a
// caller's string never reaches the query.
//
// Paging works only under the default order: a cursor from one order would
// drop rows in another, and whoever sorts by cost wants the top of the list.
var sessionSorts = map[string]store.AgentSessionSort{
	"":         store.SortRecent,
	"recent":   store.SortRecent,
	"cost":     store.SortCost,
	"requests": store.SortRequests,
	"duration": store.SortDuration,
}

// sessionQuery reads the narrowing every session report takes.
func (s *Server) sessionQuery(w http.ResponseWriter, r *http.Request,
	p *authn.Principal) (store.AgentSessionQuery, bool) {
	var q store.AgentSessionQuery
	if !p.CanAdminOrg(p.OrgID) {
		s.forbid(w, "only an administrator can read the session log")
		return q, false
	}
	orgID, from, to, ok := s.reportScope(w, r, p)
	if !ok {
		return q, false
	}
	sc, ok := s.entityScope(w, r, orgID)
	if !ok {
		return q, false
	}
	v := r.URL.Query()
	sort, known := sessionSorts[v.Get("sort")]
	if !known {
		badRequest(w, "sort must be one of recent, cost, requests or duration")
		return q, false
	}
	q = store.AgentSessionQuery{
		OrgID:   orgID,
		TeamID:  sc.TeamID,
		KeyID:   sc.KeyID,
		UserID:  sc.UserID,
		Alias:   sc.Alias,
		Key:     v.Get("key"),
		Unhappy: httpx.Flag(v, "unhappy"),
		From:    from,
		To:      to,
		Gap:     s.sessionGap(),
		Sort:    sort,
	}
	q.Limit, _ = strconv.Atoi(v.Get("limit"))
	if sort == store.SortRecent {
		q.Before, _ = strconv.ParseInt(v.Get("before"), 10, 64)
	}
	return q, true
}

// sessionGap is the idle time after which the next request starts a new task.
func (s *Server) sessionGap() time.Duration {
	if s.opts.SessionGap > 0 {
		return s.opts.SessionGap
	}
	return store.DefaultSessionGap
}

// sessions lists the tasks in one window, with totals for the whole window.
// The median next to them makes a runaway session stand out, where a mean
// would hide it.
func (s *Server) sessions(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	q, ok := s.sessionQuery(w, r, p)
	if !ok {
		return
	}
	names, err := s.groupNames(r.Context(), q.OrgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	asCSV := r.URL.Query().Get("format") == "csv"
	if asCSV {
		q.Before, q.Limit = 0, csvExportRows
	}
	rows, err := s.st.AgentSessions(r.Context(), q)
	if err != nil {
		s.fail(w, err)
		return
	}
	if asCSV {
		s.sessionsCSV(w, rows, names)
		return
	}
	out := map[string]any{
		"from": q.From, "to": q.To, "data": rows,
		"currency": s.opts.Currency,
		// The gap that produced this grouping. Without it a reader cannot
		// tell tasks cut at ten minutes from tasks cut at an hour.
		"gap_seconds": int64(q.Gap.Seconds()),
	}
	names.addTo(out)
	// Only the first page carries the totals: they do not change while
	// paging, and they cost a second pass over the window.
	if q.Before == 0 {
		totals, err := s.st.AgentSessionSummary(r.Context(), q)
		if err != nil {
			s.fail(w, err)
			return
		}
		out["totals"] = totals
	}
	if len(rows) > 0 && q.Sort == store.SortRecent {
		out["next_before"] = rows[len(rows)-1].ID
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// session is one task from start to end: its totals and every request in it,
// in order.
//
// The id in the path may be any request in the session, because readers come
// from a row in the request log.
func (s *Server) session(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if !p.CanAdminOrg(p.OrgID) {
		s.forbid(w, "only an administrator can read the session log")
		return
	}
	orgID, ok := s.scopeOrg(w, p, r.URL.Query().Get("org_id"))
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badRequest(w, "the session is named by the id of one of its requests")
		return
	}
	gap := s.sessionGap()
	session, requests, err := s.st.AgentSessionAt(r.Context(), orgID, id, gap)
	if err != nil {
		s.fail(w, err)
		return
	}
	// An operator reading across tenants gets every tenant's names, as in
	// every other report.
	names, err := s.groupNames(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	// What the agent did between its requests. See store.SessionToolCalls for
	// how the calls are matched.
	tools, err := s.st.SessionToolCalls(r.Context(), orgID, session)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := map[string]any{
		"session":     session,
		"requests":    requests,
		"tool_calls":  tools,
		"currency":    s.opts.Currency,
		"gap_seconds": int64(gap.Seconds()),
	}
	names.addTo(out)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// sessionsCSV writes the window as a spreadsheet, one row per task. The
// columns are for dividing spend by tasks and finding tasks that were not
// worth their cost, so they include how each task ended.
func (s *Server) sessionsCSV(w http.ResponseWriter, rows []store.AgentSession,
	names groupLabels) {
	cw := beginCSV(w, "keera-sessions-"+time.Now().Format("2006-01-02")+".csv")
	_ = cw.Write([]string{"started", "ended", "minutes", "requests", "ok", "failed",
		"refused", "interrupted", "models", "key", "key_id", "team", "user",
		"input_tokens", "output_tokens", "cost_" + strings.ToLower(s.opts.Currency),
		"last_status", "last_error", "session_id", "named_by_client"})
	for _, a := range rows {
		_ = cw.Write([]string{
			a.StartedAt.Format(time.RFC3339), a.EndedAt.Format(time.RFC3339),
			strconv.FormatFloat(a.Duration().Minutes(), 'f', 1, 64),
			strconv.FormatInt(a.Requests, 10), strconv.FormatInt(a.OK, 10),
			strconv.FormatInt(a.Failed, 10), strconv.FormatInt(a.Refused, 10),
			strconv.FormatInt(a.Interrupted, 10),
			strings.Join(a.Models, " "),
			names.label("key", a.KeyID), a.KeyID,
			names.label("team", a.TeamID), names.label("user", a.UserID),
			strconv.FormatInt(a.InputTokens, 10), strconv.FormatInt(a.OutputTokens, 10),
			policy.FormatMicros(a.CostMicros),
			strconv.Itoa(a.LastStatus), a.LastError,
			strconv.FormatInt(a.ID, 10), strconv.FormatBool(a.Stated),
		})
	}
	cw.Flush()
}
