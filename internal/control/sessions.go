package control

import (
	"net/http"
	"slices"
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
// A member sees only the sessions of their own keys, like the request log.

// sessionQuery reads the narrowing every session report takes.
func (s *Server) sessionQuery(w http.ResponseWriter, r *http.Request,
	p *authn.Principal) (store.AgentSessionQuery, bool) {
	var q store.AgentSessionQuery
	holder, ok := s.trafficHolder(w, p, r.URL.Query().Get("user_id"))
	if !ok {
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
	sort, ok := parseSort(w, v, store.SessionSorts())
	if !ok {
		return q, false
	}
	q = store.AgentSessionQuery{
		OrgID:       orgID,
		ReportScope: sc,
		Holder:      holder,
		Key:         v.Get("key"),
		Outcome:     store.Outcome(v.Get("outcome")),
		From:        from,
		To:          to,
		Gap:         s.opts.SessionGap,
		Sort:        sort,
	}
	// Older keera commands ask for the sessions with problems this way.
	if q.Outcome == store.OutcomeAny && httpx.Flag(v, "unhappy") {
		q.Outcome = store.OutcomeUnhappy
	}
	var before int64
	q.Limit, before = page(v)
	if sort == store.SortRecent {
		q.Before = before
	}
	return q, true
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

		// The filter choices, only when asked for, as on the request log.
		if httpx.Flag(r.URL.Query(), "facets") {
			facets, err := s.st.AgentSessionFilters(r.Context(), q)
			if err != nil {
				s.fail(w, err)
				return
			}
			out["filters"] = facets
		}
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
	holder, ok := s.trafficHolder(w, p, "")
	if !ok {
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
	session, requests, err := s.st.AgentSessionAt(r.Context(), orgID, id, s.opts.SessionGap)
	if err != nil {
		s.fail(w, err)
		return
	}
	// A session is one key's, but a key can change hands. A member sees it
	// only if every request in it was theirs; anything else is not found, as
	// another tenant's would be.
	if holder != "" && slices.ContainsFunc(requests, func(q store.Request) bool {
		return q.UserID != holder
	}) {
		s.fail(w, store.ErrNotFound)
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
		"gap_seconds": int64(s.opts.SessionGap.Seconds()),
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
		"refused", "interrupted", "models", "key", "key_id", "project", "user",
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
			names.label("project", a.ProjectID), names.label("user", a.UserID),
			strconv.FormatInt(a.InputTokens, 10), strconv.FormatInt(a.OutputTokens, 10),
			policy.FormatMicros(a.CostMicros),
			strconv.Itoa(a.LastStatus), a.LastError,
			strconv.FormatInt(a.ID, 10), strconv.FormatBool(a.Stated),
		})
	}
	cw.Flush()
}
