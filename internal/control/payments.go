package control

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/id"
	"github.com/bespinian/keera-gateway/internal/payment"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/registry"
	"github.com/bespinian/keera-gateway/internal/store"
)

// Payments is how organisations pay for the deployment's provider keys in
// advance, by card through PostFinance Checkout. See docs/billing.md.
type Payments struct {
	Client *payment.Client
	// VATBP is the VAT added to every payment, in basis points.
	VATBP int64
}

// The bounds of one payment, in CHF micros. The upper catches a typo before a
// card does.
const (
	minTopUp = 1_000_000
	maxTopUp = 10_000_000_000
)

// The transaction fee on every card payment, for what the payment costs: CHF
// 0.18 plus 1.55% of the credit, the percentage at least CHF 0.20. It is paid
// on top and not credited.
const (
	feeFixed   = 180_000
	feeBP      = 155
	feeMinPart = 200_000
)

// payments answers for a route that needs payments, and says so on a
// deployment without them.
func (s *Server) payments(w http.ResponseWriter) *Payments {
	if s.opts.Payments == nil {
		conflict(w, "payments_off",
			"this deployment takes no payments: nobody pays in advance. See docs/billing.md")
	}
	return s.opts.Payments
}

// creditAccount is an organisation's credit, its settings and its latest
// payments, as the panel shows them.
func (s *Server) creditAccount(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if !s.requireAdmin(w, p) {
		return
	}
	orgID, ok := s.queryOrg(w, r, p)
	if !ok {
		return
	}
	pay := s.opts.Payments
	if pay == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"payments": false})
		return
	}
	// Someone back from the payment page sees the payment now, not when the
	// webhook arrives. PostFinance gets a few seconds; after that the page
	// shows the payment as pending.
	settleCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	s.settleRecent(settleCtx, orgID)
	cancel()
	a, err := s.st.CreditAccount(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	list, err := s.st.ListPayments(r.Context(), orgID, 20)
	if err != nil {
		s.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"payments":         true,
		"vat_percent":      payment.Percent(pay.VATBP),
		"fee_fixed_micros": feeFixed,
		"fee_bp":           feeBP,
		"fee_min_micros":   feeMinPart,
		"min_topup_micros": minTopUp,
		"max_topup_micros": maxTopUp,
		"account":          a,
		"recent":           list,
	})
}

// settleRecent looks up the pending payments an organisation started in the
// last hour, at most a few.
func (s *Server) settleRecent(ctx context.Context, orgID string) {
	list, err := s.st.PendingPaymentsOf(ctx, orgID, time.Now().Add(-time.Hour), 5)
	if err != nil {
		return
	}
	for _, pay := range list {
		if pay.TransactionID != 0 {
			if err := s.settle(ctx, pay); err != nil {
				s.log.Warn("reading a payment from PostFinance failed", "payment", pay.ID, "error", err)
			}
		}
	}
}

// updateCreditAccount sets the automatic top-up, and for operators whether
// the organisation is billed by invoice.
func (s *Server) updateCreditAccount(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if s.payments(w) == nil {
		return
	}
	orgID, ok := s.adminOrg(w, r, p)
	if !ok {
		return
	}
	var in struct {
		TopUpBelowMicros *int64 `json:"topup_below_micros"`
		TopUpMicros      *int64 `json:"topup_micros"`
		Invoiced         *bool  `json:"invoiced"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Invoiced != nil && !p.Unrestricted() {
		forbid(w, "only an operator can bill an organisation by invoice")
		return
	}
	if _, err := s.st.ScopeName(r.Context(), policy.ScopeOrg, orgID); err != nil {
		s.fail(w, err)
		return
	}
	if (in.TopUpBelowMicros == nil) != (in.TopUpMicros == nil) {
		badRequest(w, "set 'topup_below_micros' and 'topup_micros' together; both 0 turns "+
			"the automatic top-up off")
		return
	}
	if in.TopUpMicros != nil {
		below, amount := *in.TopUpBelowMicros, *in.TopUpMicros
		switch {
		case below == 0 && amount == 0:
		case below <= 0 || below > maxTopUp:
			badRequest(w, "'topup_below_micros' is the balance that starts a top-up; give one "+
				"above CHF 0 and up to CHF "+policy.FormatMicros(maxTopUp))
			return
		default:
			if msg := checkAmount(amount); msg != "" {
				badRequest(w, "'topup_micros': "+msg)
				return
			}
			// Otherwise one top-up would not lift the balance over the
			// threshold, and the next would follow ten minutes later.
			if amount < below {
				badRequest(w, "'topup_micros' must be at least 'topup_below_micros', so one "+
					"top-up lifts the credit above the threshold")
				return
			}
		}
	}
	a, err := s.st.UpdateCredit(r.Context(), orgID, store.CreditSettings{
		TopUpBelowMicros: in.TopUpBelowMicros, TopUpMicros: in.TopUpMicros, Invoiced: in.Invoiced,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, orgID, "billing.account.update", "org", orgID, in)
	if in.Invoiced != nil {
		s.refreshCredit(r.Context())
	}
	httpx.WriteJSON(w, http.StatusOK, a)
}

// checkAmount says what is wrong with an amount to pay, or nothing.
func checkAmount(micros int64) string {
	switch {
	case micros < minTopUp || micros > maxTopUp:
		return "pay from CHF " + policy.FormatMicros(minTopUp) + " to CHF " +
			policy.FormatMicros(maxTopUp)
	case micros%10_000 != 0:
		return "give whole centimes"
	}
	return ""
}

// forgetCard removes the saved card, here and at PostFinance. The automatic
// top-up stops with it.
func (s *Server) forgetCard(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	pay := s.payments(w)
	if pay == nil {
		return
	}
	orgID, ok := s.adminOrg(w, r, p)
	if !ok {
		return
	}
	token, err := s.st.ForgetCard(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if token != 0 {
		s.deleteToken(r.Context(), pay, token)
	}
	s.auditf(r, p, orgID, "billing.card.remove", "org", orgID, nil)
	w.WriteHeader(http.StatusNoContent)
}

// deleteToken asks PostFinance to forget a card. Keera has already: if this
// fails, the token is never charged again, and stays in the back office.
func (s *Server) deleteToken(ctx context.Context, pay *Payments, token int64) {
	if err := pay.Client.DeleteToken(ctx, token); err != nil {
		s.log.Warn("PostFinance did not forget a saved card", "token", token, "error", err)
	}
}

// createTopUp starts a payment, and answers the payment page to send the
// person to. The credit is added once PostFinance says it is paid.
func (s *Server) createTopUp(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	pay := s.payments(w)
	if pay == nil {
		return
	}
	orgID, ok := s.adminOrg(w, r, p)
	if !ok {
		return
	}
	var in struct {
		AmountMicros int64 `json:"amount_micros"`
		SaveCard     bool  `json:"save_card"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if msg := checkAmount(in.AmountMicros); msg != "" {
		badRequest(w, "'amount_micros': "+msg)
		return
	}
	pm := pay.newPayment(orgID, store.PaymentTopUp, in.AmountMicros, p.Actor())
	pm.SaveCard = in.SaveCard
	if err := s.st.CreatePayment(r.Context(), pm); err != nil {
		s.fail(w, err)
		return
	}
	t := transactionFor(pm, p.Email, pay.VATBP)
	back := s.opts.PublicURL + "/billing?payment=" + pm.ID
	t.SuccessURL, t.FailedURL = back, back
	if in.SaveCard {
		t.TokenizationMode = payment.SaveCard
	}
	tx, err := s.startTransaction(r.Context(), &pm, t)
	var page string
	if err == nil {
		page, err = pay.Client.PaymentPageURL(r.Context(), tx.ID)
	}
	if err != nil {
		s.log.Error("starting a payment failed", "payment", pm.ID, "error", err)
		_ = s.settleFailed(r.Context(), pm, "PostFinance Checkout could not start the payment", false)
		httpx.WriteError(w, http.StatusBadGateway, "server_error", "payment_unavailable",
			"PostFinance Checkout could not start the payment; nothing was charged. Try again in a moment")
		return
	}
	s.auditf(r, p, orgID, "billing.topup", "payment", pm.ID, map[string]any{
		"micros": pm.Micros, "fee_micros": pm.FeeMicros, "save_card": pm.SaveCard,
	})
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"payment": pm, "url": page})
}

// newPayment is a pending payment of micros, with its VAT and fee.
func (pay *Payments) newPayment(orgID string, kind store.PaymentKind, micros int64,
	actor string) store.Payment {
	return store.Payment{
		ID: id.New("pay"), OrgID: orgID, Kind: kind, Micros: micros,
		VATMicros: vatOn(micros, pay.VATBP), FeeMicros: feeOn(micros),
		Actor: actor, State: store.PaymentPending,
	}
}

// startTransaction creates the PostFinance transaction t for pm, and ties
// the two together.
func (s *Server) startTransaction(ctx context.Context, pm *store.Payment,
	t payment.NewTransaction) (payment.Transaction, error) {
	tx, err := s.opts.Payments.Client.Create(ctx, t)
	if err != nil {
		return tx, err
	}
	pm.TransactionID = tx.ID
	return tx, s.st.SetTransaction(ctx, pm.ID, tx.ID)
}

// vatOn is the VAT on an amount, to the centime.
func vatOn(micros, bp int64) int64 {
	return int64(math.Round(float64(micros)*float64(bp)/10_000/10_000)) * 10_000
}

// feeOn is the transaction fee on an amount, to the centime.
func feeOn(micros int64) int64 {
	part := max(micros*feeBP/10_000, feeMinPart)
	return feeFixed + (part+5_000)/10_000*10_000
}

// transactionFor is the PostFinance transaction that pays for a payment.
func transactionFor(pm store.Payment, email string, vatBP int64) payment.NewTransaction {
	item := payment.LineItem{
		Name: "Keera credit", Quantity: 1, Type: "PRODUCT",
		AmountIncludingTax: payment.Amount(pm.Micros + pm.VATMicros),
		UniqueID:           "credit", SKU: "keera-credit",
	}
	if vatBP > 0 {
		item.Taxes = []payment.Tax{{Rate: payment.Percent(vatBP), Title: "VAT"}}
	}
	items := []payment.LineItem{item}
	// A payment started before the fee has none.
	if pm.FeeMicros > 0 {
		items = append(items, payment.LineItem{
			Name: "Transaction fee", Quantity: 1, Type: "FEE",
			AmountIncludingTax: payment.Amount(pm.FeeMicros), UniqueID: "fee",
		})
	}
	return payment.NewTransaction{
		Currency: policy.Currency, LineItems: items,
		CustomerID: pm.OrgID, CustomerEmailAddress: email, MerchantReference: pm.ID,
		CompletionBehavior: "COMPLETE_IMMEDIATELY",
		MetaData:           map[string]string{"org_id": pm.OrgID, "payment_id": pm.ID},
	}
}

// listPayments lists payments, newest first: an organisation's, or for an
// operator every one's.
func (s *Server) listPayments(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if !s.requireAdmin(w, p) {
		return
	}
	q := r.URL.Query()
	orgID, ok := s.scopeOrg(w, p, q.Get("org_id"))
	if !ok {
		return
	}
	limit, _ := page(q)
	list, err := s.st.ListPayments(r.Context(), orgID, limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": list})
}

// grantCredit adds credit to an organisation without a payment, or takes
// some away: a trial, a refund, money that came by bank transfer.
func (s *Server) grantCredit(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if s.payments(w) == nil {
		return
	}
	if !p.Unrestricted() {
		forbid(w, "only an operator can grant credit")
		return
	}
	var in struct {
		OrgID        string `json:"org_id"`
		AmountMicros int64  `json:"amount_micros"`
		Note         string `json:"note"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	in.Note = strings.TrimSpace(in.Note)
	switch {
	case in.OrgID == "":
		badRequest(w, "send the 'org_id' of the organisation to credit")
		return
	case in.AmountMicros == 0 || in.AmountMicros < -maxTopUp || in.AmountMicros > maxTopUp:
		badRequest(w, "'amount_micros' is the credit to add, or below 0 to take away, up to CHF "+
			policy.FormatMicros(maxTopUp))
		return
	case in.Note == "":
		badRequest(w, "a 'note' is required: it says why, in the organisation's payments")
		return
	}
	if _, err := s.st.ScopeName(r.Context(), policy.ScopeOrg, in.OrgID); err != nil {
		s.fail(w, err)
		return
	}
	g, err := s.st.Grant(r.Context(), store.Payment{
		ID: id.New("pay"), OrgID: in.OrgID, Micros: in.AmountMicros, Actor: p.Actor(), Note: in.Note,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, in.OrgID, "billing.grant", "payment", g.ID, in)
	s.refreshCredit(r.Context())
	httpx.WriteJSON(w, http.StatusCreated, g)
}

// postFinanceWebhook is told by PostFinance that a transaction changed. The
// call is not trusted: it only says which transaction to read back over the
// authenticated API, and only one of a pending or failed payment of Keera's
// is read: a payment given up on can still be paid late.
func (s *Server) postFinanceWebhook(w http.ResponseWriter, r *http.Request) {
	pay := s.opts.Payments
	if pay == nil {
		http.NotFound(w, r)
		return
	}
	var in struct {
		EntityID int64  `json:"entityId"`
		Entity   string `json:"listenerEntityTechnicalName"`
		SpaceID  int64  `json:"spaceId"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		badRequest(w, "this is not a PostFinance Checkout webhook")
		return
	}
	// Anything else is acknowledged, so PostFinance does not keep sending it.
	if in.Entity != "Transaction" || in.SpaceID != pay.Client.SpaceID() {
		w.WriteHeader(http.StatusOK)
		return
	}
	pm, err := s.st.PaymentByTransaction(r.Context(), in.EntityID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && pm.State == store.PaymentPaid) {
		w.WriteHeader(http.StatusOK)
		return
	}
	if err == nil {
		err = s.settle(r.Context(), pm)
	}
	if err != nil {
		// PostFinance tries again later.
		s.log.Warn("a PostFinance webhook could not be handled", "transaction", in.EntityID, "error", err)
		httpx.WriteError(w, http.StatusServiceUnavailable, "server_error", "", "try again later")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// settle reads a payment's transaction, and ends the payment if the
// transaction has ended. A failed one is read too: one given up on can still
// be paid.
func (s *Server) settle(ctx context.Context, pm store.Payment) error {
	if pm.State == store.PaymentPaid || pm.TransactionID == 0 {
		return nil
	}
	tx, err := s.opts.Payments.Client.Get(ctx, pm.TransactionID)
	if err != nil {
		return err
	}
	return s.settleWith(ctx, pm, tx)
}

// settleWith ends a payment as its transaction says. A transaction still
// under way leaves it pending.
func (s *Server) settleWith(ctx context.Context, pm store.Payment, tx payment.Transaction) error {
	pay := s.opts.Payments
	var how store.Settlement
	switch {
	case tx.Paid() && !paidInFull(pm, tx):
		// Keera made the transaction, so this should never happen; if it
		// does, an operator has to look before any credit is added.
		s.log.Error("a payment's transaction is for another amount; no credit was added",
			"payment", pm.ID, "transaction", tx.ID, "currency", tx.Currency,
			"amount", tx.AuthorizationAmount)
		how.Note = "the amount paid does not match; ask the operator of this deployment"
	case tx.Paid():
		how.Paid = true
		if pm.SaveCard && tx.TokenID() == 0 {
			// Paid with a method PostFinance cannot charge again by itself.
			how.Note = "paid, but this payment method cannot be saved for automatic top-ups"
		}
		if pm.SaveCard && tx.TokenID() != 0 {
			card, err := pay.Client.Card(ctx, tx.TokenID())
			if err != nil {
				s.log.Warn("reading a saved card failed", "token", tx.TokenID(), "error", err)
			}
			how.Token = tx.TokenID()
			how.Card = &store.SavedCard{Label: cmp.Or(card.Label, "Saved card")}
			if !card.Expires.IsZero() {
				how.Card.Expires = &card.Expires
			}
		}
	case tx.Failed():
		how.Note = cmp.Or(tx.UserFailureMessage, "the payment did not go through")
	default:
		return nil
	}
	done, err := s.st.Settle(ctx, pm.ID, how)
	if err != nil || !done.Ended {
		return err
	}
	settled := done.Payment
	if done.OldToken != 0 {
		s.deleteToken(ctx, pay, done.OldToken)
	}
	s.refreshCredit(ctx)
	if done.Lifted {
		// Lifting a signed-up organisation's limit changes what its models
		// may reach. Any other payment changes only the balance.
		s.announce(ctx, registry.AnyChange)
	}
	s.auditPayment(ctx, settled)
	return nil
}

// auditPayment records how a payment ended, however it was settled.
func (s *Server) auditPayment(ctx context.Context, settled store.Payment) {
	if err := s.st.Audit(ctx, "payments", settled.OrgID, "billing.payment."+string(settled.State),
		"payment", settled.ID, map[string]any{"micros": settled.Micros, "note": settled.Note}); err != nil {
		s.log.Warn("writing audit entry failed", "error", err, "action", "billing.payment")
	}
}

// paidInFull reports whether a transaction is for what the payment asked.
func paidInFull(pm store.Payment, tx payment.Transaction) bool {
	cents := int64(math.Round(tx.AuthorizationAmount * 100))
	return tx.Currency == policy.Currency && cents == (pm.Micros+pm.VATMicros+pm.FeeMicros)/10_000
}

// refreshCredit lets this replica's gateway see a balance that changed. The
// others see it within KEERA_SPEND_REFRESH.
func (s *Server) refreshCredit(ctx context.Context) {
	if err := s.reg.RefreshCredit(ctx); err != nil {
		s.log.Warn("reading the credit failed", "error", err)
	}
}

// paymentsEvery is how often the automatic top-ups and the pending payments
// are looked at.
const paymentsEvery = 30 * time.Second

// RunPayments charges the saved cards of accounts that ran low, and settles
// payments whose webhook never came, until ctx ends. Every replica runs it,
// but they take turns, so each round one replica looks. A payment is still
// credited once, whether the webhook or this check gets there first.
func (s *Server) RunPayments(ctx context.Context) {
	if s.opts.Payments == nil {
		return
	}
	t := time.NewTicker(paymentsEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// One replica at a time, so a payment is not asked after twice.
			if ok, err := s.st.TakeTurn(ctx, "payments", paymentsEvery); !ok {
				if err != nil && ctx.Err() == nil {
					s.log.Warn("asking whether to look at payments failed", "error", err)
				}
				continue
			}
			s.topUpDue(ctx)
			s.settlePending(ctx, time.Now())
		}
	}
}

// topUpDue starts the automatic top-ups that are due.
func (s *Server) topUpDue(ctx context.Context) {
	due, err := s.st.DueTopUps(ctx, 20)
	if err != nil {
		s.warnUnlessDone(ctx, "reading the due top-ups failed", "error", err)
		return
	}
	for _, a := range due {
		s.autoTopUp(ctx, a)
	}
}

// autoTopUp charges an account's saved card its top-up.
func (s *Server) autoTopUp(ctx context.Context, a store.CreditAccount) {
	pay := s.opts.Payments
	pm := pay.newPayment(a.OrgID, store.PaymentAuto, a.TopUpMicros, "automatic top-up")
	switch err := s.st.CreatePayment(ctx, pm); {
	case errors.Is(err, store.ErrTopUpUnderway):
		return
	case err != nil:
		s.log.Warn("starting an automatic top-up failed", "org_id", a.OrgID, "error", err)
		return
	}
	t := transactionFor(pm, "", pay.VATBP)
	t.Token, t.CustomersPresence = a.CardToken, "NOT_PRESENT"
	tx, err := s.startTransaction(ctx, &pm, t)
	if err != nil {
		s.log.Warn("an automatic top-up could not start; it is tried again", "org_id", a.OrgID,
			"error", err)
		_ = s.settleFailed(ctx, pm, "PostFinance Checkout could not be reached", true)
		return
	}
	charged, err := pay.Client.Charge(ctx, tx.ID)
	if refused, ok := errors.AsType[*payment.Error](err); ok && refused.Status/100 == 4 {
		// PostFinance refused to charge the card at all, so nothing was
		// charged, and trying again would be refused again.
		s.log.Warn("PostFinance refused an automatic top-up", "org_id", a.OrgID, "error", err)
		if err := s.settleFailed(ctx, pm, "PostFinance refused to charge the saved card", false); err != nil {
			s.log.Warn("settling an automatic top-up failed", "payment", pm.ID, "error", err)
		}
		return
	}
	if err != nil {
		// The charge may have gone through all the same: the transaction is
		// read again until it ends.
		s.log.Warn("an automatic top-up has no answer yet", "org_id", a.OrgID, "error", err)
		return
	}
	if err := s.settleWith(ctx, pm, charged); err != nil {
		s.log.Warn("settling an automatic top-up failed", "payment", pm.ID, "error", err)
	}
}

// How long a payment may stay pending. One never tied to a transaction was
// interrupted. A top-up is looked up for a day; after that only its webhook
// settles it, so one paid late is still credited. An automatic top-up still
// pending after a day stops, and says so, because it blocks the next.
const (
	settleAfter = time.Minute
	unstarted   = 10 * time.Minute
	lookUpFor   = 24 * time.Hour
)

// settlePending reads back the payments still pending: their webhook may
// have been lost.
func (s *Server) settlePending(ctx context.Context, now time.Time) {
	list, err := s.st.PendingPayments(ctx, now.Add(-lookUpFor), now.Add(-settleAfter), 50)
	if err != nil {
		s.warnUnlessDone(ctx, "reading the pending payments failed", "error", err)
		return
	}
	for _, pm := range list {
		age := now.Sub(pm.TS)
		if pm.TransactionID == 0 {
			if age > unstarted {
				_ = s.settleFailed(ctx, pm, "the payment was interrupted before it started", true)
			}
			continue
		}
		tx, err := s.opts.Payments.Client.Get(ctx, pm.TransactionID)
		if err != nil {
			s.log.Warn("reading a payment from PostFinance failed", "payment", pm.ID, "error", err)
			continue
		}
		if !tx.Paid() && !tx.Failed() && age > lookUpFor {
			s.log.Error("an automatic top-up has not ended after a day; no other is tried "+
				"until an administrator acts", "payment", pm.ID, "transaction", tx.ID, "state", tx.State)
			if err := s.settleFailed(ctx, pm, "it did not end within a day; look up "+
				"transaction "+strconv.FormatInt(tx.ID, 10)+" in PostFinance Checkout", false); err != nil {
				s.log.Warn("settling a payment failed", "payment", pm.ID, "error", err)
			}
			continue
		}
		if err := s.settleWith(ctx, pm, tx); err != nil {
			s.log.Warn("settling a payment failed", "payment", pm.ID, "error", err)
		}
	}
}

// settleFailed ends a payment as failed. A failed automatic top-up stops the
// next until someone acts, unless retry says the card was not at fault. The
// balance stays as it was, so the gateway has nothing to read again.
func (s *Server) settleFailed(ctx context.Context, pm store.Payment, note string, retry bool) error {
	done, err := s.st.Settle(ctx, pm.ID, store.Settlement{Note: note, Retry: retry})
	if err == nil && done.Ended {
		s.auditPayment(ctx, done.Payment)
	}
	return err
}
