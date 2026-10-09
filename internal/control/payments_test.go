package control

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/payment"
	"github.com/bespinian/keera-gateway/internal/registry"
	"github.com/bespinian/keera-gateway/internal/secret"
	"github.com/bespinian/keera-gateway/internal/store"
)

// fakePostFinance is PostFinance Checkout's API, as far as Keera uses it.
type fakePostFinance struct {
	*httptest.Server
	mu      sync.Mutex
	next    int64
	created map[int64]payment.NewTransaction
	state   map[int64]payment.Transaction
	// charged is the state a charge on a saved card ends in.
	charged payment.Transaction
	deleted []int64
}

func newFakePostFinance(t *testing.T) *fakePostFinance {
	f := &fakePostFinance{next: 100, created: map[int64]payment.NewTransaction{},
		state: map[int64]payment.Transaction{}, charged: payment.Transaction{State: "FULFILL"}}
	mux := http.NewServeMux()
	const base = "/api/v2.0/payment"
	mux.HandleFunc("POST "+base+"/transactions", func(w http.ResponseWriter, r *http.Request) {
		var in payment.NewTransaction
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Errorf("decoding a transaction: %v", err)
		}
		f.mu.Lock()
		f.next++
		id := f.next
		f.created[id] = in
		amount, _ := in.LineItems[0].AmountIncludingTax.Float64()
		tx := payment.Transaction{ID: id, State: "PENDING", Currency: in.Currency,
			AuthorizationAmount: amount, CustomerID: in.CustomerID,
			MerchantReference: in.MerchantReference}
		f.state[id] = tx
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(tx)
	})
	mux.HandleFunc("GET "+base+"/transactions/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(f.get(r))
	})
	mux.HandleFunc("GET "+base+"/transactions/{id}/payment-page-url", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "https://checkout.postfinance.ch/pay/%d", f.get(r).ID)
	})
	mux.HandleFunc("POST "+base+"/transactions/{id}/process-without-interaction",
		func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			tx := f.state[pathID(r)]
			tx.State, tx.UserFailureMessage = f.charged.State, f.charged.UserFailureMessage
			f.state[tx.ID] = tx
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(tx)
		})
	mux.HandleFunc("GET "+base+"/tokens/{id}/active-version", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"•••• 4821","expiresOn":"2028-09-30T00:00:00Z",
			"paymentMethodBrand":{"name":{"en-US":"Visa"}}}`))
	})
	mux.HandleFunc("DELETE "+base+"/tokens/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.deleted = append(f.deleted, pathID(r))
		f.mu.Unlock()
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

func (f *fakePostFinance) get(r *http.Request) payment.Transaction {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state[pathID(r)]
}

// pay marks a transaction paid, with a saved card if token is not zero.
func (f *fakePostFinance) pay(id, token int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tx := f.state[id]
	tx.State = "FULFILL"
	if token != 0 {
		tx.Token = &struct {
			ID int64 `json:"id"`
		}{token}
	}
	f.state[id] = tx
}

func (f *fakePostFinance) last() (int64, payment.NewTransaction) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.next, f.created[f.next]
}

// paymentServer is a control server on a deployment that takes payments.
func paymentServer(ctx context.Context, t *testing.T, st *store.Store, pf *fakePostFinance) (*Server, *registry.Registry) {
	t.Helper()
	if _, err := st.Pool().Exec(ctx, "TRUNCATE credit_accounts, payments, audit_log"); err != nil {
		t.Fatal(err)
	}
	box, err := secret.New("a-secret-key-long-enough")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := registry.New(ctx, st, registry.Options{Secrets: box, Platform: testPlatform,
		Prepaid: true}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	s := New(st, reg, nil, nil, Options{
		OperatorKey: testOperatorKey, Currency: "CHF", Secrets: box, Platform: testPlatform,
		PublicURL: "https://keera.example",
		Payments: &Payments{
			Client: payment.New(payment.Settings{URL: pf.URL, SpaceID: 4711, UserID: "1",
				AuthKey: []byte("key")}),
			SpaceID: 4711, VATBP: 810,
		},
	}, slog.New(slog.DiscardHandler))
	return s, reg
}

func webhook(s *Server, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.postFinanceWebhook(w, httptest.NewRequest(http.MethodPost,
		httpx.ControlPrefix+"/billing/postfinance", strings.NewReader(body)))
	return w
}

func transactionChanged(id int64) string {
	return fmt.Sprintf(`{"eventId":1,"entityId":%d,"listenerEntityTechnicalName":"Transaction",
		"spaceId":4711,"webhookListenerId":2,"timestamp":"2026-10-08T10:00:00+0000"}`, id)
}

type accountView struct {
	Payments bool                `json:"payments"`
	Account  store.CreditAccount `json:"account"`
	Recent   []store.Payment     `json:"recent"`
}

func readAccount(t *testing.T, s *Server) accountView {
	t.Helper()
	w := invoke(s.creditAccount, admin("org_1"), http.MethodGet, "/v1/billing/account", "")
	if w.Code != http.StatusOK {
		t.Fatalf("account: %d %s", w.Code, w.Body)
	}
	var v accountView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// topUp pays CHF 100 on the payment page, saving the card, and returns the
// transaction.
func topUp(t *testing.T, s *Server, pf *fakePostFinance) int64 {
	t.Helper()
	w := invoke(s.createTopUp, admin("org_1"), http.MethodPost, "/v1/billing/topups",
		`{"amount_micros":100000000,"save_card":true}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("top-up: %d %s", w.Code, w.Body)
	}
	id, _ := pf.last()
	pf.pay(id, 7)
	if w := webhook(s, transactionChanged(id)); w.Code != http.StatusOK {
		t.Fatalf("webhook: %d %s", w.Code, w.Body)
	}
	return id
}

func TestATopUpIsCreditedOnceWhenPostFinanceSaysItIsPaid(t *testing.T) {
	st, ctx := routerStore(t)
	pf := newFakePostFinance(t)
	s, reg := paymentServer(ctx, t, st, pf)

	if err := reg.Budgets().AllowCredit("org_1"); err == nil {
		t.Fatal("an organisation that never paid may use the deployment's keys")
	}
	w := invoke(s.createTopUp, admin("org_1"), http.MethodPost, "/v1/billing/topups",
		`{"amount_micros":100000000,"save_card":true}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var started struct {
		URL     string        `json:"url"`
		Payment store.Payment `json:"payment"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &started)
	id, sent := pf.last()
	if started.URL != fmt.Sprintf("https://checkout.postfinance.ch/pay/%d", id) {
		t.Errorf("url = %q", started.URL)
	}
	// CHF 100 plus 8.1% VAT, for this organisation, saving the card, and
	// back to the panel afterwards.
	if string(sent.LineItems[0].AmountIncludingTax) != "108.10" || sent.Currency != "CHF" ||
		sent.CustomerID != "org_1" || sent.TokenizationMode != payment.SaveCard ||
		sent.SuccessURL != "https://keera.example/billing?payment="+started.Payment.ID ||
		sent.MerchantReference != started.Payment.ID {
		t.Errorf("transaction = %+v", sent)
	}
	if v := readAccount(t, s); v.Account.BalanceMicros != 0 || v.Recent[0].State != store.PaymentPending {
		t.Errorf("credited before it was paid: %+v", v)
	}

	pf.pay(id, 7)
	for range 2 {
		if w := webhook(s, transactionChanged(id)); w.Code != http.StatusOK {
			t.Fatalf("webhook: %d %s", w.Code, w.Body)
		}
	}
	v := readAccount(t, s)
	if v.Account.BalanceMicros != 100_000_000 || v.Account.Card == nil ||
		v.Account.Card.Label != "Visa •••• 4821" || v.Recent[0].State != store.PaymentPaid {
		t.Errorf("account = %+v", v)
	}
	if err := reg.Budgets().AllowCredit("org_1"); err != nil {
		t.Errorf("refused after paying: %v", err)
	}
}

// The webhook proves nothing, so it is only acted on for a pending payment
// of this deployment's space, and then only after reading the transaction.
func TestAWebhookOnlySaysWhatToReadBack(t *testing.T) {
	st, ctx := routerStore(t)
	pf := newFakePostFinance(t)
	s, _ := paymentServer(ctx, t, st, pf)
	invoke(s.createTopUp, admin("org_1"), http.MethodPost, "/v1/billing/topups",
		`{"amount_micros":50000000}`)
	id, _ := pf.last()

	// Claimed paid, but PostFinance says it is still pending.
	webhook(s, transactionChanged(id))
	other := strings.Replace(transactionChanged(id), `"spaceId":4711`, `"spaceId":1`, 1)
	pf.pay(id, 0)
	for _, body := range []string{other, transactionChanged(999), `{"listenerEntityTechnicalName":"Refund"}`} {
		if w := webhook(s, body); w.Code != http.StatusOK {
			t.Errorf("%s: %d", body, w.Code)
		}
	}
	if v := readAccount(t, s); v.Account.BalanceMicros != 50_000_000 {
		// Reading the account settles a recent payment by itself.
		t.Errorf("balance = %d", v.Account.BalanceMicros)
	}
	if w := webhook(s, "not json"); w.Code != http.StatusBadRequest {
		t.Errorf("garbage: %d", w.Code)
	}
}

// A top-up left open on the payment page is not looked up after a day, but
// stays pending, so paying it later still adds the credit.
func TestATopUpPaidLateIsStillCredited(t *testing.T) {
	st, ctx := routerStore(t)
	pf := newFakePostFinance(t)
	s, _ := paymentServer(ctx, t, st, pf)
	invoke(s.createTopUp, admin("org_1"), http.MethodPost, "/v1/billing/topups",
		`{"amount_micros":20000000}`)
	id, _ := pf.last()
	s.settlePending(ctx, time.Now().Add(25*time.Hour))
	pf.pay(id, 0)
	if w := webhook(s, transactionChanged(id)); w.Code != http.StatusOK {
		t.Fatalf("webhook: %d", w.Code)
	}
	if a, _ := st.CreditAccount(ctx, "org_1"); a.BalanceMicros != 20_000_000 {
		t.Errorf("balance = %d, want CHF 20.00", a.BalanceMicros)
	}
}

// An automatic top-up still open after a day is given up on, so the next can
// be tried once someone looks. If PostFinance takes the money after all, the
// credit is added.
func TestAnAutomaticTopUpGivenUpOnIsCreditedIfPaidLater(t *testing.T) {
	st, ctx := routerStore(t)
	pf := newFakePostFinance(t)
	s, _ := paymentServer(ctx, t, st, pf)
	topUp(t, s, pf)
	if w := invoke(s.updateCreditAccount, admin("org_1"), http.MethodPatch, "/v1/billing/account",
		`{"topup_below_micros":200000000,"topup_micros":200000000}`); w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	pf.mu.Lock()
	pf.charged = payment.Transaction{State: "PROCESSING"}
	pf.mu.Unlock()
	s.topUpDue(ctx)
	id, _ := pf.last()

	s.settlePending(ctx, time.Now().Add(25*time.Hour))
	if a, _ := st.CreditAccount(ctx, "org_1"); a.TopUpError == "" || a.BalanceMicros != 100_000_000 {
		t.Fatalf("account = %+v, want the top-up given up on", a)
	}
	pf.pay(id, 0)
	for range 2 {
		if w := webhook(s, transactionChanged(id)); w.Code != http.StatusOK {
			t.Fatalf("webhook: %d", w.Code)
		}
	}
	if a, _ := st.CreditAccount(ctx, "org_1"); a.TopUpError != "" || a.BalanceMicros != 300_000_000 {
		t.Errorf("account = %+v, want CHF 300.00 credited once and the top-up working again", a)
	}
}

func TestAPaymentWhoseWebhookWasLostIsSettledAnyway(t *testing.T) {
	st, ctx := routerStore(t)
	pf := newFakePostFinance(t)
	s, _ := paymentServer(ctx, t, st, pf)
	invoke(s.createTopUp, admin("org_1"), http.MethodPost, "/v1/billing/topups",
		`{"amount_micros":20000000}`)
	id, _ := pf.last()
	pf.pay(id, 0)
	s.settlePending(ctx, time.Now().Add(2*time.Minute))
	a, _ := st.CreditAccount(ctx, "org_1")
	if a.BalanceMicros != 20_000_000 {
		t.Errorf("balance = %d, want CHF 20.00", a.BalanceMicros)
	}
}

// Below its threshold, an account is charged on its saved card, at most once
// every ten minutes; once the card is declined, it is not tried again until
// someone acts.
func TestAnAccountThatRanLowIsToppedUpFromTheSavedCard(t *testing.T) {
	st, ctx := routerStore(t)
	pf := newFakePostFinance(t)
	s, _ := paymentServer(ctx, t, st, pf)
	topUp(t, s, pf)
	w := invoke(s.updateCreditAccount, admin("org_1"), http.MethodPatch, "/v1/billing/account",
		`{"topup_below_micros":200000000,"topup_micros":200000000}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}

	s.topUpDue(ctx)
	id, sent := pf.last()
	if sent.Token != 7 || sent.CustomersPresence != "NOT_PRESENT" ||
		string(sent.LineItems[0].AmountIncludingTax) != "216.20" {
		t.Errorf("transaction %d = %+v", id, sent)
	}
	if a, _ := st.CreditAccount(ctx, "org_1"); a.BalanceMicros != 300_000_000 {
		t.Errorf("balance = %d, want CHF 300.00", a.BalanceMicros)
	}

	// Far below zero, say by a burst of traffic: no run of charges follows.
	if _, err := st.Grant(ctx, store.Payment{ID: "pay_burst", OrgID: "org_1",
		Micros: -1_000_000_000, Actor: "test", Note: "burst"}); err != nil {
		t.Fatal(err)
	}
	s.topUpDue(ctx)
	if again, _ := pf.last(); again != id {
		t.Fatal("charged again within ten minutes")
	}

	ageAutoTopUps := func() {
		t.Helper()
		if _, err := st.Pool().Exec(ctx, `UPDATE payments SET ts = ts - interval '11 minutes'
			WHERE kind = 'auto'`); err != nil {
			t.Fatal(err)
		}
	}
	ageAutoTopUps()
	pf.mu.Lock()
	pf.charged = payment.Transaction{State: "DECLINE", UserFailureMessage: "The card was declined."}
	pf.mu.Unlock()
	s.topUpDue(ctx)
	a, _ := st.CreditAccount(ctx, "org_1")
	if a.TopUpError != "The card was declined." || a.BalanceMicros != -700_000_000 {
		t.Errorf("account = %+v", a)
	}
	before, _ := pf.last()
	ageAutoTopUps()
	s.topUpDue(ctx)
	if after, _ := pf.last(); after != before {
		t.Error("a declined card was charged again")
	}
}

func TestATopUpSmallerThanItsThresholdIsRefused(t *testing.T) {
	st, ctx := routerStore(t)
	pf := newFakePostFinance(t)
	s, _ := paymentServer(ctx, t, st, pf)
	w := invoke(s.updateCreditAccount, admin("org_1"), http.MethodPatch, "/v1/billing/account",
		`{"topup_below_micros":200000000,"topup_micros":50000000}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d: %s", w.Code, w.Body)
	}
	// Large amounts fit too.
	w = invoke(s.updateCreditAccount, admin("org_1"), http.MethodPatch, "/v1/billing/account",
		`{"topup_below_micros":5000000000,"topup_micros":10000000000}`)
	if w.Code != http.StatusOK {
		t.Errorf("CHF 10,000: %d %s", w.Code, w.Body)
	}
}

// A payment method PostFinance cannot save is paid all the same, and the
// payment says the card was not saved.
func TestAPaymentThatSavedNoCardSaysSo(t *testing.T) {
	st, ctx := routerStore(t)
	pf := newFakePostFinance(t)
	s, _ := paymentServer(ctx, t, st, pf)
	invoke(s.createTopUp, admin("org_1"), http.MethodPost, "/v1/billing/topups",
		`{"amount_micros":20000000,"save_card":true}`)
	id, _ := pf.last()
	pf.pay(id, 0)
	webhook(s, transactionChanged(id))
	v := readAccount(t, s)
	if v.Account.BalanceMicros != 20_000_000 || v.Account.Card != nil ||
		!strings.Contains(v.Recent[0].Note, "cannot be saved") {
		t.Errorf("account = %+v, payment = %+v", v.Account, v.Recent[0])
	}
}

func TestForgettingTheCardForgetsItAtPostFinance(t *testing.T) {
	st, ctx := routerStore(t)
	pf := newFakePostFinance(t)
	s, _ := paymentServer(ctx, t, st, pf)
	topUp(t, s, pf)
	if w := invoke(s.updateCreditAccount, admin("org_1"), http.MethodPatch, "/v1/billing/account",
		`{"topup_below_micros":200000000,"topup_micros":200000000}`); w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if w := invoke(s.forgetCard, admin("org_1"), http.MethodDelete, "/v1/billing/account/card", ""); w.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	// The automatic top-up stops with it, and a new card does not start it.
	if a, _ := st.CreditAccount(ctx, "org_1"); a.Card != nil || a.TopUpMicros != 0 {
		t.Errorf("account = %+v", a)
	}
	if len(pf.deleted) != 1 || pf.deleted[0] != 7 {
		t.Errorf("deleted at PostFinance: %v", pf.deleted)
	}
}

func TestWhoMayDoWhatWithCredit(t *testing.T) {
	st, ctx := routerStore(t)
	pf := newFakePostFinance(t)
	s, reg := paymentServer(ctx, t, st, pf)

	tests := []struct {
		name string
		w    *httptest.ResponseRecorder
		want int
	}{
		{"a member cannot pay", invoke(s.createTopUp, member("org_1"), http.MethodPost,
			"/v1/billing/topups", `{"amount_micros":100000000}`), http.StatusForbidden},
		{"a member cannot read the account", invoke(s.creditAccount, member("org_1"),
			http.MethodGet, "/v1/billing/account", ""), http.StatusForbidden},
		{"another organisation's admin cannot pay for it", invoke(s.createTopUp, admin("org_2"),
			http.MethodPost, "/v1/billing/topups?org_id=org_1", `{"amount_micros":100000000}`),
			http.StatusForbidden},
		{"too little", invoke(s.createTopUp, admin("org_1"), http.MethodPost,
			"/v1/billing/topups", `{"amount_micros":5000000}`), http.StatusBadRequest},
		{"half a centime", invoke(s.createTopUp, admin("org_1"), http.MethodPost,
			"/v1/billing/topups", `{"amount_micros":10005000}`), http.StatusBadRequest},
		{"an admin cannot grant", invoke(s.grantCredit, admin("org_1"), http.MethodPost,
			"/v1/billing/grants", `{"org_id":"org_1","amount_micros":1000000,"note":"x"}`),
			http.StatusForbidden},
		{"an admin cannot bill by invoice", invoke(s.updateCreditAccount, admin("org_1"),
			http.MethodPatch, "/v1/billing/account", `{"invoiced":true}`), http.StatusForbidden},
		{"a grant needs a reason", invoke(s.grantCredit, operator(), http.MethodPost,
			"/v1/billing/grants", `{"org_id":"org_1","amount_micros":1000000}`), http.StatusBadRequest},
		{"a grant needs an organisation", invoke(s.grantCredit, operator(), http.MethodPost,
			"/v1/billing/grants", `{"org_id":"org_nope","amount_micros":1000000,"note":"x"}`),
			http.StatusNotFound},
		{"an operator grants", invoke(s.grantCredit, operator(), http.MethodPost,
			"/v1/billing/grants", `{"org_id":"org_1","amount_micros":25000000,"note":"trial"}`),
			http.StatusCreated},
	}
	for _, tt := range tests {
		if tt.w.Code != tt.want {
			t.Errorf("%s: %d, want %d: %s", tt.name, tt.w.Code, tt.want, tt.w.Body)
		}
	}
	if err := reg.Budgets().AllowCredit("org_1"); err != nil {
		t.Errorf("refused after a grant: %v", err)
	}
	if _, err := st.CreateOrg(ctx, store.Org{ID: "org_2", Name: "Another Bank"}, store.OrgTemplate{}); err != nil {
		t.Fatal(err)
	}
	w := invoke(s.updateCreditAccount, operator(), http.MethodPatch,
		"/v1/billing/account?org_id=org_2", `{"invoiced":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("invoiced: %d %s", w.Code, w.Body)
	}
	if err := reg.Budgets().AllowCredit("org_2"); err != nil {
		t.Errorf("an organisation billed by invoice was refused: %v", err)
	}
}

func TestWithoutPaymentsTheAccountSaysSo(t *testing.T) {
	st, ctx := routerStore(t)
	s := platformServer(ctx, t, st)
	w := invoke(s.creditAccount, admin("org_1"), http.MethodGet, "/v1/billing/account", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"payments":false`) {
		t.Errorf("account: %d %s", w.Code, w.Body)
	}
	if w := invoke(s.createTopUp, admin("org_1"), http.MethodPost, "/v1/billing/topups",
		`{"amount_micros":100000000}`); w.Code != http.StatusConflict {
		t.Errorf("top-up: %d %s", w.Code, w.Body)
	}
}
