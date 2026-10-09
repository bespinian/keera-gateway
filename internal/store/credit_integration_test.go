package store

import (
	"errors"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/id"
	"github.com/bespinian/keera-gateway/internal/policy"
)

func balance(t *testing.T, st *Store, orgID string) int64 {
	t.Helper()
	a, err := st.CreditAccount(t.Context(), orgID)
	if err != nil {
		t.Fatalf("CreditAccount: %v", err)
	}
	return a.BalanceMicros
}

// What a batch's calls on the deployment's keys cost is taken from each
// organisation's credit in the same batch, even one that never paid.
func TestWriteEventsTakesTheCredit(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	bill := func(credit int64) []policy.BillLine {
		return []policy.BillLine{{Alias: "fast", Provider: "anthropic", BackendModel: "claude-haiku-4-5",
			Currency: "USD", Micros: credit * 2, CreditMicros: credit}}
	}
	if _, err := st.Grant(ctx, Payment{ID: id.New("pay"), OrgID: f.orgID,
		Micros: 10_000_000, Actor: "operator", Note: "trial"}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	now := time.Now()
	if err := st.WriteEvents(ctx, []Event{
		{TS: now, OrgID: f.orgID, Alias: "fast", Status: 200, Bills: bill(1_500_000)},
		{TS: now, OrgID: f.orgID, Alias: "fast", Status: 200, Bills: bill(500_000)},
		{TS: now, OrgID: "org_never_paid", Alias: "fast", Status: 200, Bills: bill(300_000)},
	}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}
	if got := balance(t, st, f.orgID); got != 8_000_000 {
		t.Errorf("balance = %d, want CHF 8.00", got)
	}
	if got := balance(t, st, "org_never_paid"); got != -300_000 {
		t.Errorf("balance = %d, want CHF -0.30", got)
	}
	rows, err := st.Billing(ctx, f.orgID, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || len(rows) != 1 || rows[0].CreditMicros != 2_000_000 {
		t.Errorf("billing = %+v, %v; want CHF 2.00 taken from the credit", rows, err)
	}
	credit, err := st.LoadCredit(ctx)
	if err != nil || len(credit) != 2 {
		t.Errorf("LoadCredit = %+v, %v", credit, err)
	}
}

// A check's calls are billed and take the credit, but write no usage row:
// a check is not a request.
func TestACheckIsBilledWithoutAUsageRow(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now()
	if err := st.WriteEvents(ctx, []Event{{TS: now, OrgID: f.orgID, BillsOnly: true,
		Bills: []policy.BillLine{{Alias: "fast", Provider: "anthropic",
			BackendModel: "claude-haiku-4-5", Currency: "USD", Micros: 20, CreditMicros: 10}}},
	}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}
	if got := balance(t, st, f.orgID); got != -10 {
		t.Errorf("balance = %d, want the check's 10 taken", got)
	}
	var rows int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM usage_events WHERE org_id = $1`,
		f.orgID).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("usage rows = %d, %v; want none", rows, err)
	}
}

// A webhook and a poll can arrive together: the payment is credited once.
func TestAPaymentIsCreditedOnce(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	p := Payment{ID: id.New("pay"), OrgID: f.orgID, Kind: PaymentTopUp, Micros: 100_000_000,
		VATMicros: 8_100_000, SaveCard: true, Actor: "ada@example.ch"}
	if err := st.CreatePayment(ctx, p); err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if err := st.SetTransaction(ctx, p.ID, 42); err != nil {
		t.Fatalf("SetTransaction: %v", err)
	}
	if got, err := st.PaymentByTransaction(ctx, 42); err != nil || got.ID != p.ID {
		t.Fatalf("PaymentByTransaction = %+v, %v", got, err)
	}
	card := &SavedCard{Label: "Visa •••• 4821"}
	for i, want := range []bool{true, false} {
		got, settled, _, err := st.Settle(ctx, p.ID, Settlement{Paid: true, Card: card, Token: 7})
		if err != nil || settled != want || got.State != PaymentPaid {
			t.Fatalf("settle %d: %+v settled=%v err=%v", i, got, settled, err)
		}
	}
	a, err := st.CreditAccount(ctx, f.orgID)
	if err != nil {
		t.Fatal(err)
	}
	if a.BalanceMicros != 100_000_000 || a.CardToken != 7 || a.Card == nil ||
		a.Card.Label != "Visa •••• 4821" {
		t.Errorf("account = %+v, want CHF 100.00 and the card", a)
	}
}

// A new card replaces the saved one, and the old token is handed back so
// PostFinance forgets it.
func TestANewCardReplacesTheSavedOne(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	pay := func(token int64) int64 {
		p := Payment{ID: id.New("pay"), OrgID: f.orgID, Kind: PaymentTopUp,
			Micros: 20_000_000, SaveCard: true, Actor: "ada@example.ch"}
		if err := st.CreatePayment(ctx, p); err != nil {
			t.Fatal(err)
		}
		_, _, old, err := st.Settle(ctx, p.ID, Settlement{Paid: true,
			Card: &SavedCard{Label: "card"}, Token: token})
		if err != nil {
			t.Fatal(err)
		}
		return old
	}
	if old := pay(7); old != 0 {
		t.Errorf("the first card replaced %d", old)
	}
	if old := pay(8); old != 7 {
		t.Errorf("the second card replaced %d, want 7", old)
	}
	if token, err := st.ForgetCard(ctx, f.orgID); err != nil || token != 8 {
		t.Errorf("ForgetCard = %d, %v; want 8", token, err)
	}
	if a, _ := st.CreditAccount(ctx, f.orgID); a.Card != nil || a.BalanceMicros != 40_000_000 {
		t.Errorf("account = %+v, want no card and CHF 40.00", a)
	}
}

// An account below its threshold is due a top-up until one is under way,
// and one that failed on the card is not tried again until someone acts.
func TestAnAccountBelowItsThresholdIsDueATopUp(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	below, amount := int64(50_000_000), int64(200_000_000)
	if _, err := st.UpdateCredit(ctx, f.orgID, CreditSettings{
		TopUpBelowMicros: &below, TopUpMicros: &amount}); err != nil {
		t.Fatal(err)
	}
	due := func() bool {
		t.Helper()
		list, err := st.DueTopUps(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		return len(list) == 1 && list[0].OrgID == f.orgID && list[0].TopUpMicros == amount
	}
	if due() {
		t.Error("due without a card")
	}
	seed := Payment{ID: id.New("pay"), OrgID: f.orgID, Kind: PaymentTopUp, Micros: 20_000_000,
		SaveCard: true, Actor: "ada@example.ch"}
	if err := st.CreatePayment(ctx, seed); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.Settle(ctx, seed.ID, Settlement{Paid: true,
		Card: &SavedCard{Label: "card"}, Token: 7}); err != nil {
		t.Fatal(err)
	}
	if !due() {
		t.Fatal("not due at CHF 20.00 with a threshold of CHF 50.00")
	}

	auto := Payment{ID: id.New("pay"), OrgID: f.orgID, Kind: PaymentAuto, Micros: amount,
		Actor: "automatic top-up"}
	if err := st.CreatePayment(ctx, auto); err != nil {
		t.Fatal(err)
	}
	second := auto
	second.ID = id.New("pay")
	if err := st.CreatePayment(ctx, second); !errors.Is(err, ErrTopUpUnderway) {
		t.Errorf("a second top-up started: %v", err)
	}
	if due() {
		t.Error("due while a top-up is under way")
	}

	if _, _, _, err := st.Settle(ctx, auto.ID, Settlement{Note: "card declined"}); err != nil {
		t.Fatal(err)
	}
	if a, _ := st.CreditAccount(ctx, f.orgID); a.TopUpError != "card declined" {
		t.Errorf("top-up error = %q", a.TopUpError)
	}
	if due() {
		t.Error("tried again after the card was declined")
	}
	// Changing the top-up is someone acting on it.
	if _, err := st.UpdateCredit(ctx, f.orgID, CreditSettings{
		TopUpBelowMicros: &below, TopUpMicros: &amount}); err != nil {
		t.Fatal(err)
	}
	if a, _ := st.CreditAccount(ctx, f.orgID); a.TopUpError != "" {
		t.Errorf("top-up error = %q after the settings changed", a.TopUpError)
	}
}

func TestADeletedOrganisationsCardIsNotCharged(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	p := Payment{ID: id.New("pay"), OrgID: f.orgID, Kind: PaymentTopUp, Micros: 20_000_000,
		SaveCard: true, Actor: "ada@example.ch"}
	if err := st.CreatePayment(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.Settle(ctx, p.ID, Settlement{Paid: true,
		Card: &SavedCard{Label: "card"}, Token: 7}); err != nil {
		t.Fatal(err)
	}
	below, amount := int64(50_000_000), int64(100_000_000)
	if _, err := st.UpdateCredit(ctx, f.orgID, CreditSettings{
		TopUpBelowMicros: &below, TopUpMicros: &amount}); err != nil {
		t.Fatal(err)
	}
	gone, err := st.DeleteOrg(ctx, f.orgID)
	if err != nil {
		t.Fatalf("DeleteOrg: %v", err)
	}
	// The token is handed back so PostFinance forgets the card too.
	if gone.CardToken != 7 {
		t.Errorf("card token = %d, want 7", gone.CardToken)
	}
	if a, _ := st.CreditAccount(ctx, f.orgID); a.CardToken != 0 || a.Card != nil {
		t.Errorf("account = %+v, want the card gone", a)
	}
	if list, err := st.DueTopUps(ctx, 10); err != nil || len(list) != 0 {
		t.Errorf("due = %+v, %v; want none", list, err)
	}
	// The record of what was paid stays.
	if got, err := st.Payment(ctx, p.ID); err != nil || got.State != PaymentPaid {
		t.Errorf("payment = %+v, %v", got, err)
	}
}

// An organisation that signed itself up is limited until it pays. Credit an
// operator grants is not a payment, so it lifts nothing.
func TestTheFirstPaymentLiftsTheLimit(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	limited := true
	if _, err := st.UpdateOrg(ctx, f.orgID, OrgChange{Limited: &limited}); err != nil {
		t.Fatalf("UpdateOrg: %v", err)
	}
	isLimited := func() bool {
		t.Helper()
		ids, err := st.LimitedOrgs(ctx)
		if err != nil {
			t.Fatalf("LimitedOrgs: %v", err)
		}
		org, err := st.OrgByID(ctx, f.orgID)
		if err != nil {
			t.Fatalf("OrgByID: %v", err)
		}
		if org.Limited != (len(ids) == 1 && ids[0] == f.orgID) {
			t.Fatalf("the organisation says limited %v, the list says %v", org.Limited, ids)
		}
		return org.Limited
	}

	if _, err := st.Grant(ctx, Payment{ID: id.New("pay"), OrgID: f.orgID,
		Micros: 5_000_000, Actor: "operator", Note: "welcome"}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if !isLimited() {
		t.Fatal("a grant lifted the limit")
	}

	failed := Payment{ID: id.New("pay"), OrgID: f.orgID, Kind: PaymentTopUp,
		Micros: 20_000_000, Actor: "ada@example.ch"}
	if err := st.CreatePayment(ctx, failed); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.Settle(ctx, failed.ID, Settlement{Note: "declined"}); err != nil {
		t.Fatal(err)
	}
	if !isLimited() {
		t.Fatal("a failed payment lifted the limit")
	}

	paid := Payment{ID: id.New("pay"), OrgID: f.orgID, Kind: PaymentTopUp,
		Micros: 20_000_000, Actor: "ada@example.ch"}
	if err := st.CreatePayment(ctx, paid); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.Settle(ctx, paid.ID, Settlement{Paid: true}); err != nil {
		t.Fatal(err)
	}
	if isLimited() {
		t.Error("the organisation is still limited after paying")
	}
}
