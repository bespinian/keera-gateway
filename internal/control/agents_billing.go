package control

import (
	"fmt"
	"net/http"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// Keera Agents bills the machines its agents and workspaces run on through
// the gateway. Each organisation then has one credit, one bill and one set of
// receipts for both. Keera Agents meters and prices the machines itself, and
// bills nothing without prices; the gateway records its lines and says whether
// an organisation may run machines. So the service key is the only switch
// here. See https://docs.keera.ch/manage/billing/.

// machineRefusal is why an organisation may not run machines, or nil.
type machineRefusal struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// refuseMachines decides whether an organisation may run machines now. It
// follows the gateway's own rules for a billed call: a recurring charge no
// card can pay stops everything; with payments, credit decides; without
// them, an organisation that signed itself up waits, as it does for the
// deployment's provider keys.
func refuseMachines(c store.CreditRow, limited, payments bool, now time.Time) *machineRefusal {
	switch {
	case !c.ChargeDueAt.IsZero() && !c.ChargeDueAt.After(now):
		return &machineRefusal{
			Code: "charge_due",
			Message: "your organisation has a recurring charge due since " +
				c.ChargeDueAt.UTC().Format(time.DateOnly) + " that no saved card can pay; an " +
				"administrator can pay it and save a card in Keera Gateway under Billing",
		}
	case payments && !c.Invoiced && c.BalanceMicros <= 0:
		return &machineRefusal{
			Code: "credit_exhausted",
			Message: "your organisation has used up its credit (balance " +
				policy.FormatMoney(c.BalanceMicros) + "); an administrator can add credit " +
				"in Keera Gateway under Billing",
		}
	case !payments && limited:
		return &machineRefusal{
			Code: "org_limited",
			Message: "your organisation signed itself up and cannot run machines yet; " +
				"an operator of Keera Gateway can lift the limit",
		}
	}
	return nil
}

// serviceBilling says whether an organisation may run machines now. Keera
// Agents asks before it starts or resumes one.
func (s *Server) serviceBilling(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if !requireService(w, p) {
		return
	}
	org, ok := s.serviceOrg(w, r, r.URL.Query().Get("org_id"))
	if !ok {
		return
	}
	payments := s.opts.Payments != nil
	c, err := s.st.CreditRowOf(r.Context(), org.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	balance := c.BalanceMicros
	if !payments {
		// Without payments there is no credit to speak of.
		balance = 0
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"payments": payments, "balance_micros": balance, "invoiced": c.Invoiced,
		"refusal": refuseMachines(c, org.Limited, payments, time.Now()),
	})
}

const (
	maxServiceLines = 1000
	// maxLineMicros is CHF 1 million, far above any line. It keeps a bug in
	// Keera Agents from emptying an organisation's credit in one go.
	maxLineMicros = 1_000_000_000_000
	// A line may come from a clock a little ahead, and from a backlog Keera
	// Agents could not send while the gateway was away, but not from long
	// ago: that month's bill is likely paid.
	lineClockSkew = 5 * time.Minute
	maxLineAge    = 40 * 24 * time.Hour
)

// checkServiceLines says what is wrong with the first bad line, or "".
func checkServiceLines(lines []store.ServiceLine, now time.Time) string {
	if len(lines) == 0 || len(lines) > maxServiceLines {
		return fmt.Sprintf("send from 1 to %d lines in 'lines'", maxServiceLines)
	}
	for i, l := range lines {
		why := ""
		switch {
		case l.ID == "" || len(l.ID) > 200:
			why = "'id' must be 1 to 200 characters"
		case l.OrgID == "":
			why = "name the organisation in 'org_id'"
		case l.Item == "" || len(l.Item) > 64:
			why = "'item' must be 1 to 64 characters"
		case len(l.Unit) > 32:
			why = "'unit' can be at most 32 characters"
		case l.QuantityMicros < 0:
			why = "'quantity_micros' cannot be negative"
		case l.Micros < 0 || l.Micros > maxLineMicros:
			why = fmt.Sprintf("'micros' must be from 0 to %d", maxLineMicros)
		case l.TS.IsZero():
			why = "send the line's time in 'ts'"
		case l.TS.After(now.Add(lineClockSkew)):
			why = "'ts' is in the future"
		case l.TS.Before(now.Add(-maxLineAge)):
			why = fmt.Sprintf("'ts' is more than %d days ago", maxLineAge/(24*time.Hour))
		}
		if why != "" {
			return fmt.Sprintf("line %d: %s", i, why)
		}
	}
	return ""
}

// serviceBillingLines records lines Keera Agents bills. A line it sends again,
// because it did not hear back, is recorded and charged once.
//
// Whether the organisation still exists is not checked: billing rows outlive
// organisations, and Keera Agents knows only organisations that existed.
func (s *Server) serviceBillingLines(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if !requireService(w, p) {
		return
	}
	var in struct {
		Lines []store.ServiceLine `json:"lines"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if why := checkServiceLines(in.Lines, time.Now()); why != "" {
		badRequest(w, why)
		return
	}
	added, err := s.st.AddServiceLines(r.Context(), store.ServiceAgents, in.Lines, s.opts.Payments != nil)
	if err != nil {
		s.fail(w, err)
		return
	}
	duplicates := len(in.Lines) - added
	s.log.Debug("recorded Keera Agents' billing lines", "added", added, "duplicates", duplicates)
	httpx.WriteJSON(w, http.StatusOK, map[string]int{"added": added, "duplicates": duplicates})
}
