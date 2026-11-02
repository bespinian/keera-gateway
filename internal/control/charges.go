package control

import (
	"context"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/id"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// The most a recurring charge may cost each period. A yearly fee may be more
// than a top-up, but this still catches a typo before a card does.
const maxCharge = 100_000_000_000

// listCharges lists recurring charges: an organisation's, or for an operator
// every one's.
func (s *Server) listCharges(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if s.payments(w) == nil || !s.requireAdmin(w, p) {
		return
	}
	orgID, ok := s.scopeOrg(w, p, r.URL.Query().Get("org_id"))
	if !ok {
		return
	}
	list, err := s.st.ListCharges(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": list})
}

// createCharge adds a recurring charge. Only an operator may: it is what the
// organisation agreed to pay the deployment, not something it chooses.
func (s *Server) createCharge(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if s.payments(w) == nil {
		return
	}
	if !p.Unrestricted() {
		forbid(w, "only an operator can add a recurring charge")
		return
	}
	var in struct {
		OrgID        string `json:"org_id"`
		Name         string `json:"name"`
		AmountMicros int64  `json:"amount_micros"`
		Every        string `json:"every"`
		StartsOn     string `json:"starts_on"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	today := time.Now().UTC().Format(time.DateOnly)
	if in.StartsOn == "" {
		in.StartsOn = today
	}
	start, err := time.Parse(time.DateOnly, in.StartsOn)
	switch {
	case in.OrgID == "":
		needOrg(w, "send the 'org_id' of the organisation to charge")
		return
	case in.Name == "" || utf8.RuneCountInString(in.Name) > 100:
		badRequest(w, "'name' says what the charge pays for, on the payment page and the "+
			"receipt; give one of up to 100 characters")
		return
	case in.AmountMicros < minTopUp || in.AmountMicros > maxCharge:
		badRequest(w, "'amount_micros' is the price of each period without VAT; charge from "+
			policy.FormatPrice(minTopUp)+" to "+policy.FormatPrice(maxCharge))
		return
	case in.AmountMicros%10_000 != 0:
		badRequest(w, "'amount_micros': give whole centimes")
		return
	case in.Every != "month" && in.Every != "year":
		badRequest(w, "'every' is 'month' or 'year'")
		return
	case err != nil:
		badRequest(w, "'starts_on' is the day the first period starts, such as "+today)
		return
	// Otherwise every period since would be charged at once.
	case in.StartsOn < today:
		badRequest(w, "'starts_on' cannot be in the past; the earliest is today, "+today)
		return
	}
	in.StartsOn = start.Format(time.DateOnly)
	if _, err := s.st.ScopeName(r.Context(), policy.ScopeOrg, in.OrgID); err != nil {
		s.fail(w, err)
		return
	}
	c, err := s.st.CreateCharge(r.Context(), store.RecurringCharge{
		ID: id.New("charge"), OrgID: in.OrgID, Name: in.Name, AmountMicros: in.AmountMicros,
		Every: in.Every, StartsOn: in.StartsOn, Actor: p.Actor(),
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, in.OrgID, "billing.charge.create", "charge", c.ID, in)
	// Without a saved card, the organisation is refused from the first day.
	s.refreshCredit(r.Context())
	httpx.WriteJSON(w, http.StatusCreated, c)
}

// deleteCharge ends a recurring charge. What was paid stays paid; a period
// not paid by then is no longer due.
func (s *Server) deleteCharge(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if s.payments(w) == nil {
		return
	}
	if !p.Unrestricted() {
		forbid(w, "only an operator can remove a recurring charge")
		return
	}
	c, err := s.st.DeleteCharge(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, c.OrgID, "billing.charge.delete", "charge", c.ID, nil)
	s.refreshCredit(r.Context())
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": c.ID, "deleted": true})
}

// payCharge pays a recurring charge's first unpaid period on the payment page,
// and saves the card for the periods after. It is how an organisation without
// a card, or whose card was declined, saves one.
func (s *Server) payCharge(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	pay := s.payments(w)
	if pay == nil || !s.requireAdmin(w, p) {
		return
	}
	c, err := s.st.Charge(r.Context(), r.PathValue("id"))
	if err == nil && !p.CanAdminOrg(c.OrgID) {
		// 404, not 403, so charge ids cannot be probed.
		err = store.ErrNotFound
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	a, err := s.st.CreditAccount(r.Context(), c.OrgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	// With a card that works, the next period is charged to it when it
	// starts. Paying ahead would only pay it early.
	if a.Card != nil && c.Error == "" && time.Now().Before(c.DueAt) {
		conflict(w, "nothing_due", "nothing is due yet: the saved card is charged on "+
			c.DueAt.Format(time.DateOnly))
		return
	}
	pm := pay.newPayment(c.OrgID, store.PaymentCharge, c.AmountMicros, p.Actor())
	pm.SaveCard = true
	pm.ChargeID, pm.Item, pm.PeriodStart, pm.PeriodEnd = c.ID, c.Name, &c.DueAt, &c.DueUntil
	s.payOnPage(w, r, p, pm, "billing.charge.pay")
}

// chargesDue charges the saved cards of recurring charges whose period has
// started.
func (s *Server) chargesDue(ctx context.Context) {
	due, err := s.st.DueCharges(ctx, 20)
	if err != nil {
		s.warnUnlessDone(ctx, "reading the due recurring charges failed", "error", err)
		return
	}
	for _, c := range due {
		s.chargeSavedCard(ctx, c)
	}
}

// chargeSavedCard charges one period of a recurring charge to the saved card.
func (s *Server) chargeSavedCard(ctx context.Context, c store.DueCharge) {
	pm := s.opts.Payments.newPayment(c.OrgID, store.PaymentCharge, c.AmountMicros, "recurring charge")
	pm.ChargeID, pm.Item, pm.PeriodStart, pm.PeriodEnd = c.ID, c.Name, &c.DueAt, &c.DueUntil
	s.chargeCard(ctx, pm, c.CardToken)
}

// lineItemName is how a charge's period reads on the payment page and the
// receipt. The period's end is the last day it covers.
func lineItemName(pm store.Payment) string {
	if pm.PeriodStart == nil || pm.PeriodEnd == nil {
		return pm.Item
	}
	return pm.Item + ", " + pm.PeriodStart.UTC().Format(time.DateOnly) + " to " +
		pm.PeriodEnd.UTC().AddDate(0, 0, -1).Format(time.DateOnly)
}
