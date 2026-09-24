package control

import (
	"net/http"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/store"
)

// Filters and routers are the two hooks an organisation puts in front of a
// request. Their routes share the helpers here.

// writeHookList answers a filter or router list. With ?stats it adds what each
// one did in the window. Only the panel asks for that, so the CLI and the
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

// scopeList names the guardrails that still use a filter or router, for the
// refusal to delete it.
func scopeList(users []store.FilterScope) string {
	labels := make([]string, 0, len(users))
	for _, u := range users {
		labels = append(labels, string(u.ScopeType)+" "+u.Name)
	}
	return strings.Join(labels, ", ")
}

// deleted finishes the delete of a filter, router or model: it writes the
// audit entry, tells the gateways and answers. kind is "filter", "router" or
// "model".
func (s *Server) deleted(w http.ResponseWriter, r *http.Request, p *authn.Principal,
	orgID, kind, alias string,
) {
	s.auditf(r, p, orgID, kind+".delete", kind, alias, nil)
	s.changed(r)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"alias": alias, "deleted": true})
}
