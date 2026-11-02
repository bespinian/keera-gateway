package control

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/payment"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// addCharge has an operator charge org_1 CHF 500 every month from today.
func addCharge(t *testing.T, s *Server) store.RecurringCharge {
	t.Helper()
	w := invoke(s.createCharge, operator(), http.MethodPost, "/v1/billing/charges",
		`{"org_id":"org_1","name":"Dedicated model","amount_micros":500000000,"every":"month"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("charge: %d %s", w.Code, w.Body)
	}
	var c store.RecurringCharge
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func today() time.Time {
	return time.Now().UTC().Truncate(24 * time.Hour)
}

func TestARecurringChargeIsChargedToTheSavedCardEachPeriod(t *testing.T) {
	st, ctx := routerStore(t)
	pf := newFakePostFinance(t)
	s, reg := paymentServer(ctx, t, st, pf)
	topUp(t, s, pf)
	c := addCharge(t, s)
	if !c.DueAt.Equal(today()) || !c.DueUntil.Equal(today().AddDate(0, 1, 0)) {
		t.Errorf("charge = %+v, want the first month due from today", c)
	}

	s.chargesDue(ctx)
	id, sent := pf.last()
	// CHF 500 plus 8.1% VAT, and a fee of CHF 0.18 plus 1.55%.
	period := "Dedicated model, " + today().Format(time.DateOnly) + " to " +
		today().AddDate(0, 1, -1).Format(time.DateOnly)
	if sent.Token != 7 || sent.CustomersPresence != "NOT_PRESENT" || len(sent.LineItems) != 2 ||
		sent.LineItems[0].Name != period || string(sent.LineItems[0].AmountIncludingTax) != "540.50" ||
		string(sent.LineItems[1].AmountIncludingTax) != "7.93" {
		t.Errorf("transaction %d = %+v", id, sent)
	}
	// A fee, not credit.
	if a, _ := st.CreditAccount(ctx, "org_1"); a.BalanceMicros != 100_000_000 {
		t.Errorf("balance = %d, want CHF 100.00 as before", a.BalanceMicros)
	}
	paid, _ := st.PaymentByTransaction(ctx, id)
	if paid.Kind != store.PaymentCharge || paid.State != store.PaymentPaid ||
		paid.ChargeID != c.ID || !paid.PeriodStart.Equal(c.DueAt) {
		t.Errorf("payment = %+v", paid)
	}
	after, _ := st.Charge(ctx, c.ID)
	if after.PaidPeriods != 1 || !after.DueAt.Equal(c.DueUntil) {
		t.Errorf("charge = %+v, want the next month due", after)
	}

	// The next month is not charged before it starts.
	s.chargesDue(ctx)
	if again, _ := pf.last(); again != id {
		t.Error("charged again before the next period")
	}
	if err := reg.Budgets().ChargesPaid("org_1", time.Now()); err != nil {
		t.Errorf("refused with every charge paid: %v", err)
	}
}

// Without a saved card, a charge due stops the organisation until an
// administrator pays it on the payment page, which saves the card.
func TestADueChargeWithoutACardStopsTheOrganisationUntilItIsPaid(t *testing.T) {
	st, ctx := routerStore(t)
	pf := newFakePostFinance(t)
	s, reg := paymentServer(ctx, t, st, pf)
	c := addCharge(t, s)
	if _, ok := errors.AsType[*policy.ErrChargeDue](reg.Budgets().ChargesPaid("org_1", time.Now())); !ok {
		t.Fatal("let in with a charge due and no card")
	}
	// The panel's banner reads it from the setup counts.
	if st, _ := st.SetupState(ctx, "org_1"); st.ChargeDueAt == nil || !st.ChargeDueAt.Equal(c.DueAt) {
		t.Errorf("setup = %+v, want the charge due", st)
	}
	s.chargesDue(ctx)
	if n, _ := pf.last(); n != 100 {
		t.Fatal("charged without a card")
	}

	w := invoke(s.payCharge, admin("org_1"), http.MethodPost, "/v1/billing/charges/"+c.ID+"/pay", "")
	if w.Code != http.StatusCreated {
		t.Fatalf("pay: %d %s", w.Code, w.Body)
	}
	id, sent := pf.last()
	if sent.TokenizationMode != payment.SaveCard || sent.Token != 0 ||
		!strings.HasPrefix(sent.LineItems[0].Name, "Dedicated model, ") {
		t.Errorf("transaction = %+v", sent)
	}
	pf.pay(id, 9)
	if w := webhook(s, transactionChanged(id)); w.Code != http.StatusOK {
		t.Fatalf("webhook: %d %s", w.Code, w.Body)
	}
	if err := reg.Budgets().ChargesPaid("org_1", time.Now()); err != nil {
		t.Errorf("refused after paying: %v", err)
	}
	if st, _ := st.SetupState(ctx, "org_1"); st.ChargeDueAt != nil {
		t.Errorf("setup = %+v, want nothing due", st)
	}
	a, _ := st.CreditAccount(ctx, "org_1")
	if a.Card == nil || a.BalanceMicros != 0 {
		t.Errorf("account = %+v, want the card saved and no credit", a)
	}
	// The card stays while there are charges to pay with it.
	if w := invoke(s.forgetCard, admin("org_1"), http.MethodDelete, "/v1/billing/account/card", ""); w.Code != http.StatusConflict {
		t.Errorf("forget: %d %s", w.Code, w.Body)
	}
	// The month is paid, so nothing more is due, and paying ahead is refused.
	if w := invoke(s.payCharge, admin("org_1"), http.MethodPost, "/v1/billing/charges/"+c.ID+"/pay", ""); w.Code != http.StatusConflict {
		t.Errorf("pay ahead: %d %s", w.Code, w.Body)
	}

	// Removing the charge lets the card go.
	if w := invoke(s.deleteCharge, operator(), http.MethodDelete, "/v1/billing/charges/"+c.ID, ""); w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if w := invoke(s.forgetCard, admin("org_1"), http.MethodDelete, "/v1/billing/account/card", ""); w.Code != http.StatusOK {
		t.Errorf("forget after the charge ended: %d %s", w.Code, w.Body)
	}
}

// A declined card stops the organisation and is not tried again, until a
// payment on the payment page works.
func TestADeclinedChargeStopsTheOrganisationUntilSomeonePays(t *testing.T) {
	st, ctx := routerStore(t)
	pf := newFakePostFinance(t)
	s, reg := paymentServer(ctx, t, st, pf)
	topUp(t, s, pf)
	pf.mu.Lock()
	pf.charged = payment.Transaction{State: "DECLINE", UserFailureMessage: "The card was declined."}
	pf.mu.Unlock()
	c := addCharge(t, s)
	s.chargesDue(ctx)
	if got, _ := st.Charge(ctx, c.ID); got.Error != "The card was declined." || got.PaidPeriods != 0 {
		t.Errorf("charge = %+v", got)
	}
	if err := reg.Budgets().ChargesPaid("org_1", time.Now()); err == nil {
		t.Error("let in with a declined charge")
	}
	if _, err := st.Pool().Exec(ctx, `UPDATE payments SET ts = ts - interval '11 minutes'`); err != nil {
		t.Fatal(err)
	}
	before, _ := pf.last()
	s.chargesDue(ctx)
	if after, _ := pf.last(); after != before {
		t.Error("a declined card was charged again")
	}

	w := invoke(s.payCharge, admin("org_1"), http.MethodPost, "/v1/billing/charges/"+c.ID+"/pay", "")
	if w.Code != http.StatusCreated {
		t.Fatalf("pay: %d %s", w.Code, w.Body)
	}
	id, _ := pf.last()
	pf.pay(id, 11)
	webhook(s, transactionChanged(id))
	if got, _ := st.Charge(ctx, c.ID); got.Error != "" || got.PaidPeriods != 1 {
		t.Errorf("charge = %+v, want it paid and the card tried again next time", got)
	}
	if err := reg.Budgets().ChargesPaid("org_1", time.Now()); err != nil {
		t.Errorf("refused after paying: %v", err)
	}
	if len(pf.deleted) != 1 || pf.deleted[0] != 7 {
		t.Errorf("the declined card was not replaced: %v", pf.deleted)
	}
}

func TestWhoMayDoWhatWithCharges(t *testing.T) {
	st, ctx := routerStore(t)
	pf := newFakePostFinance(t)
	s, _ := paymentServer(ctx, t, st, pf)
	topUp(t, s, pf)
	c := addCharge(t, s)
	s.chargesDue(ctx)
	tomorrow := time.Now().UTC().AddDate(0, 0, 1).Format(time.DateOnly)
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format(time.DateOnly)
	body := func(fields string) string {
		return `{"org_id":"org_1","name":"Support","amount_micros":100000000,` + fields + `}`
	}

	tests := []struct {
		name string
		w    func() int
		want int
	}{
		{"an admin cannot add one", func() int {
			return invoke(s.createCharge, admin("org_1"), http.MethodPost, "/v1/billing/charges",
				body(`"every":"month"`)).Code
		}, http.StatusForbidden},
		{"an admin cannot remove one", func() int {
			return invoke(s.deleteCharge, admin("org_1"), http.MethodDelete, "/v1/billing/charges/"+c.ID, "").Code
		}, http.StatusForbidden},
		{"a member cannot list them", func() int {
			return invoke(s.listCharges, member("org_1"), http.MethodGet, "/v1/billing/charges", "").Code
		}, http.StatusForbidden},
		{"an admin lists their own", func() int {
			return invoke(s.listCharges, admin("org_1"), http.MethodGet, "/v1/billing/charges", "").Code
		}, http.StatusOK},
		{"another organisation's admin cannot pay one", func() int {
			return invoke(s.payCharge, admin("org_2"), http.MethodPost, "/v1/billing/charges/"+c.ID+"/pay", "").Code
		}, http.StatusNotFound},
		{"paid by a working card already", func() int {
			return invoke(s.payCharge, admin("org_1"), http.MethodPost, "/v1/billing/charges/"+c.ID+"/pay", "").Code
		}, http.StatusConflict},
		{"every week", func() int {
			return invoke(s.createCharge, operator(), http.MethodPost, "/v1/billing/charges",
				body(`"every":"week"`)).Code
		}, http.StatusBadRequest},
		{"from yesterday", func() int {
			return invoke(s.createCharge, operator(), http.MethodPost, "/v1/billing/charges",
				body(`"every":"year","starts_on":"`+yesterday+`"`)).Code
		}, http.StatusBadRequest},
		{"no organisation", func() int {
			return invoke(s.createCharge, operator(), http.MethodPost, "/v1/billing/charges",
				strings.Replace(body(`"every":"year"`), "org_1", "org_nope", 1)).Code
		}, http.StatusNotFound},
		{"from tomorrow", func() int {
			return invoke(s.createCharge, operator(), http.MethodPost, "/v1/billing/charges",
				body(`"every":"year","starts_on":"`+tomorrow+`"`)).Code
		}, http.StatusCreated},
	}
	for _, tt := range tests {
		if got := tt.w(); got != tt.want {
			t.Errorf("%s: %d, want %d", tt.name, got, tt.want)
		}
	}
}

// A charge from the 31st is due on the last day of a shorter month, and on
// the 31st again after it.
func TestAChargeFromTheThirtyFirstKeepsItsDay(t *testing.T) {
	st, ctx := routerStore(t)
	if _, err := st.CreateCharge(ctx, store.RecurringCharge{
		ID: "charge_31", OrgID: "org_1", Name: "x", AmountMicros: 1_000_000, Every: "month",
		StartsOn: "2027-01-31", Actor: "test",
	}); err != nil {
		t.Fatal(err)
	}
	for paid, want := range []string{"2027-01-31", "2027-02-28", "2027-03-31"} {
		if _, err := st.Pool().Exec(ctx, `UPDATE recurring_charges SET paid_periods = $1`, paid); err != nil {
			t.Fatal(err)
		}
		c, _ := st.Charge(ctx, "charge_31")
		if got := c.DueAt.UTC().Format(time.DateOnly); got != want || c.DueAt.UTC().Hour() != 0 {
			t.Errorf("after %d periods, due %s, want %s", paid, c.DueAt, want)
		}
	}
}

// Replicas' payment rounds can overlap. One that listed a charge before
// another replica charged it must not charge the card for that period again,
// even once the other payment is settled and ten minutes have passed.
func TestAChargeListedBeforeAnotherReplicaChargedItIsNotChargedAgain(t *testing.T) {
	st, ctx := routerStore(t)
	pf := newFakePostFinance(t)
	s, _ := paymentServer(ctx, t, st, pf)
	topUp(t, s, pf)
	addCharge(t, s)

	stale, err := st.DueCharges(ctx, 20)
	if err != nil || len(stale) != 1 {
		t.Fatalf("due = %+v, %v", stale, err)
	}
	s.chargesDue(ctx)
	first, _ := pf.last()
	if _, err := st.Pool().Exec(ctx, `UPDATE payments SET ts = ts - interval '11 minutes'
		WHERE kind = 'charge'`); err != nil {
		t.Fatal(err)
	}

	s.chargeSavedCard(ctx, stale[0])
	if again, _ := pf.last(); again != first {
		t.Error("the card was charged twice for one period")
	}
}
