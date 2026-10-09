package control

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/store"
)

// Filters and routers are the two hooks an organisation puts in front of a
// request. Their routes share the helpers here, and so do the routes of
// models and MCP servers.

// writeHookList answers a filter, router or model list. With ?stats it adds
// what each one did in the window. Only the panel asks for that, so the CLI and the
// gateway do not pay for an aggregate over the log.
func (s *Server) writeHookList(w http.ResponseWriter, r *http.Request, data any,
	stats func(from, to time.Time) (any, error),
) {
	out := map[string]any{"data": data}
	if q := r.URL.Query(); httpx.Flag(q, "stats") {
		from, to, ok := queryWindow(w, q)
		if !ok {
			return
		}
		st, err := stats(from, to)
		if err != nil {
			s.fail(w, err)
			return
		}
		out["stats"], out["from"], out["to"] = st, from, to
		out["currency"] = s.opts.Currency
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// hookReport reads one filter's or router's own screen: its report over the
// window and, under kind, the filter or router itself. A deleted one keeps its
// traffic, so the report is served either way and kind is left out rather than
// answering 404. It returns false once it has answered with an error.
func hookReport[R, H any](s *Server, w http.ResponseWriter, r *http.Request, p *authn.Principal, kind string,
	report func(ctx context.Context, orgID, alias string, from, to time.Time) (R, error),
	hook func(ctx context.Context, orgID, alias string) (H, error),
) (out map[string]any, orgID string, ok bool) {
	orgID, ok = s.queryOrg(w, r, p)
	if !ok {
		return nil, "", false
	}
	from, to, ok := queryWindow(w, r.URL.Query())
	if !ok {
		return nil, "", false
	}
	alias := r.PathValue("alias")
	rep, err := report(r.Context(), orgID, alias, from, to)
	if err != nil {
		s.fail(w, err)
		return nil, "", false
	}
	out = map[string]any{"report": rep, "currency": s.opts.Currency}
	h, err := hook(r.Context(), orgID, alias)
	switch {
	case err == nil:
		out[kind] = h
	case !errors.Is(err, store.ErrNotFound):
		s.fail(w, err)
		return nil, "", false
	}
	return out, orgID, true
}

// scopeList names the guardrails that still use a filter, router or MCP
// server, for the refusal to delete it.
func scopeList(users []store.GuardrailRef) string {
	labels := make([]string, 0, len(users))
	for _, u := range users {
		labels = append(labels, string(u.ScopeType)+" "+u.Name)
	}
	return strings.Join(labels, ", ")
}

// deleteUnused deletes a filter, router or MCP server that no guardrail uses.
// One still in use is refused with a 409 naming those guardrails; inUse says
// why, given the alias and that list. kind names which, as deleted takes it.
func (s *Server) deleteUnused(w http.ResponseWriter, r *http.Request, p *authn.Principal, kind string,
	users func(ctx context.Context, orgID, alias string) ([]store.GuardrailRef, error),
	remove func(ctx context.Context, orgID, alias string) error,
	inUse func(alias, users string) string,
) {
	orgID, ok := s.adminOrg(w, r, p)
	if !ok {
		return
	}
	alias := r.PathValue("alias")
	refs, err := users(r.Context(), orgID, alias)
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(refs) > 0 {
		httpx.WriteError(w, http.StatusConflict, "invalid_request_error", kind+"_in_use",
			inUse(alias, scopeList(refs)))
		return
	}
	if err := remove(r.Context(), orgID, alias); err != nil {
		s.fail(w, err)
		return
	}
	s.deleted(w, r, p, orgID, kind, alias)
}

// deleted finishes the delete of a filter, router, model or MCP server: it
// writes the audit entry, tells the gateways and answers. kind names which.
func (s *Server) deleted(w http.ResponseWriter, r *http.Request, p *authn.Principal,
	orgID, kind, alias string,
) {
	s.auditf(r, p, orgID, kind+".delete", kind, alias, nil)
	s.changed(r)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"alias": alias, "deleted": true})
}
