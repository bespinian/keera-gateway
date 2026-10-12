package control

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// billingTotal is the whole bill.
type billingTotal struct {
	Calls  int64 `json:"calls"`
	Micros int64 `json:"micros"`
}

// billing is one month of use of the deployment's own provider keys, per
// organisation and provider model, at list price. See docs/billing.md.
//
// Administrators read their own organisation's. Operators read any, or all at
// once.
func (s *Server) billing(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if !s.requireAdmin(w, p) {
		return
	}
	q := r.URL.Query()
	orgID, ok := s.scopeOrg(w, p, q.Get("org_id"))
	if !ok {
		return
	}
	from, err := parseMonth(q.Get("month"), time.Now())
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	to := policy.PeriodMonth.Next(from)
	rows, err := s.st.Billing(r.Context(), orgID, from, to)
	if err != nil {
		s.fail(w, err)
		return
	}
	var total billingTotal
	for _, row := range rows {
		total.Calls += row.Calls
		total.Micros += row.Micros
	}
	month := from.Format("2006-01")
	// What came out of prepaid credit only means something on a deployment
	// that takes payments.
	payments := s.opts.Payments != nil
	if q.Get("format") == "csv" {
		billingCSV(w, month, rows, payments)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"month": month, "from": from, "to": to, "data": rows, "total": total,
		"payments": payments,
	})
}

// parseMonth reads a month written as YYYY-MM, in UTC like the budgets.
// Empty is the current one.
func parseMonth(raw string, now time.Time) (time.Time, error) {
	if raw == "" {
		return policy.PeriodMonth.Start(now), nil
	}
	t, err := time.Parse("2006-01", raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("'month' is %q; write it as YYYY-MM, such as 2026-09", raw)
	}
	return t, nil
}

func billingCSV(w http.ResponseWriter, month string, lines []store.BillingRow, payments bool) {
	cw := beginCSV(w, "keera-billing-"+month+".csv")
	head := []string{"month", "org_id", "org_name", "provider", "model", "calls",
		"input_tokens", "cached_input_tokens", "cache_write_tokens", "output_tokens", "amount"}
	if payments {
		head = append(head, "from_credit")
	}
	_ = cw.Write(head)
	for _, l := range lines {
		rec := []string{month, l.OrgID, l.OrgName, l.Provider, l.BackendModel,
			strconv.FormatInt(l.Calls, 10),
			strconv.FormatInt(l.InputTokens, 10),
			strconv.FormatInt(l.CachedInputTokens, 10),
			strconv.FormatInt(l.CacheWriteTokens, 10),
			strconv.FormatInt(l.OutputTokens, 10),
			policy.FormatMicros(l.Micros)}
		if payments {
			rec = append(rec, policy.FormatMicros(l.CreditMicros))
		}
		_ = cw.Write(rec)
	}
	cw.Flush()
}
