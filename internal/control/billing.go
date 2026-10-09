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

// billingLine is one row of the monthly bill. What the provider charges, and
// so the margin, is shown to operators only.
type billingLine struct {
	store.BillingRow
	ProviderMicros *int64 `json:"provider_micros,omitempty"`
	MarginMicros   *int64 `json:"margin_micros,omitempty"`
}

// billingTotal is the bill summed per currency. Nothing is converted.
type billingTotal struct {
	Currency       string `json:"currency"`
	Calls          int64  `json:"calls"`
	Micros         int64  `json:"micros"`
	ProviderMicros *int64 `json:"provider_micros,omitempty"`
	MarginMicros   *int64 `json:"margin_micros,omitempty"`
}

// billing is one month of use of the deployment's own provider keys, per
// organisation and provider model, at list price. See docs/billing.md.
//
// Administrators read their own organisation's. Operators read any, or all at
// once, and also see what the providers charge.
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
	operator := p.Unrestricted()
	lines := make([]billingLine, 0, len(rows))
	var totals []billingTotal
	for _, row := range rows {
		line := billingLine{BillingRow: row}
		if operator {
			line.ProviderMicros, line.MarginMicros = costAndMargin(row.Micros, row.ProviderMicros)
		}
		lines = append(lines, line)
		totals = addTotal(totals, row, operator)
	}
	month := from.Format("2006-01")
	if q.Get("format") == "csv" {
		billingCSV(w, month, lines, operator)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"month": month, "from": from, "to": to, "data": lines, "totals": totals,
	})
}

func costAndMargin(micros, providerMicros int64) (*int64, *int64) {
	margin := micros - providerMicros
	return &providerMicros, &margin
}

// addTotal adds one row to the total of its currency.
func addTotal(totals []billingTotal, row store.BillingRow, operator bool) []billingTotal {
	i := 0
	for i < len(totals) && totals[i].Currency != row.Currency {
		i++
	}
	if i == len(totals) {
		totals = append(totals, billingTotal{Currency: row.Currency})
	}
	t := &totals[i]
	t.Calls += row.Calls
	t.Micros += row.Micros
	if operator {
		var cost int64
		if t.ProviderMicros != nil {
			cost = *t.ProviderMicros
		}
		t.ProviderMicros, t.MarginMicros = costAndMargin(t.Micros, cost+row.ProviderMicros)
	}
	return totals
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

func billingCSV(w http.ResponseWriter, month string, lines []billingLine, operator bool) {
	cw := beginCSV(w, "keera-billing-"+month+".csv")
	head := []string{"month", "org_id", "org_name", "provider", "model", "currency", "calls",
		"input_tokens", "cached_input_tokens", "cache_write_tokens", "output_tokens", "amount"}
	if operator {
		head = append(head, "provider_cost", "margin")
	}
	_ = cw.Write(head)
	for _, l := range lines {
		rec := []string{month, l.OrgID, l.OrgName, l.Provider, l.BackendModel, l.Currency,
			strconv.FormatInt(l.Calls, 10),
			strconv.FormatInt(l.InputTokens, 10),
			strconv.FormatInt(l.CachedInputTokens, 10),
			strconv.FormatInt(l.CacheWriteTokens, 10),
			strconv.FormatInt(l.OutputTokens, 10),
			policy.FormatMicros(l.Micros)}
		if operator {
			rec = append(rec, policy.FormatMicros(*l.ProviderMicros), policy.FormatMicros(*l.MarginMicros))
		}
		_ = cw.Write(rec)
	}
	cw.Flush()
}
