package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// billingRun is one 'keera billing' invocation.
type billingRun struct {
	*cmdRun
	month    string
	amount   float64
	below    float64
	off      bool
	saveCard bool
	note     string
	limit    int
}

func billingCmd(ctx context.Context, args []string) error {
	// The bill and the payments are reports, which span every organisation
	// the caller can see unless told one. The other verbs act on one.
	org := "org"
	v := verbAsked("billing", args)
	if v == "" || v == "report" || v == "payments" {
		org = "orgs"
	}
	r := &billingRun{cmdRun: newCmdRun("billing", args, org, "json")}
	// An operator grants credit to one organisation, so it has to be named.
	if v == "grant" || v == "invoiced" {
		r.fs.Lookup("org").Usage = "organisation id (required)"
	}
	r.fs.StringVar(&r.month, "month", "", "the month to bill, as YYYY-MM (default: this month)")
	r.fs.Float64Var(&r.amount, "amount", 0, "an amount in CHF, such as 100")
	r.fs.Float64Var(&r.below, "below", 0, "the balance in CHF that starts an automatic top-up")
	r.fs.BoolVar(&r.off, "off", false, "turn the automatic top-up off")
	r.fs.BoolVar(&r.saveCard, "save-card", false,
		"keep the card for automatic top-ups and the next payment")
	r.fs.StringVar(&r.note, "note", "", "why, shown in the organisation's payments")
	r.fs.IntVar(&r.limit, "limit", 50, "the most payments to list")
	if done, err := r.parse(); done {
		return err
	}
	switch r.verb {
	case "credit":
		return r.credit(ctx)
	case "topup":
		return r.topUp(ctx)
	case "auto-topup":
		return r.autoTopUp(ctx)
	case "forget-card":
		return r.forgetCard(ctx)
	case "grant":
		return r.grant(ctx)
	case "invoiced":
		return r.invoiced(ctx)
	case "payments":
		return r.payments(ctx)
	default:
		return r.report(ctx)
	}
}

// account is path for the organisation the verb acts on: the one given, or
// the only one there is.
func (r *billingRun) account(ctx context.Context, path string) (string, error) {
	org, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return "", err
	}
	return inOrg(path, org), nil
}

func (r *billingRun) report(ctx context.Context) error {
	q := url.Values{}
	setIfGiven(q, map[string]string{"org_id": r.org, "month": r.month})
	var res billingResponse
	if err := r.c.do(ctx, "GET", "/v1/billing?"+q.Encode(), nil, &res); err != nil {
		return err
	}
	return out(r.asJSON, res, func(w *table) { printBilling(w, res) })
}

type billingResponse struct {
	Month  string         `json:"month"`
	Data   []billingRow   `json:"data"`
	Totals []billingTotal `json:"totals"`
}

type billingRow struct {
	store.BillingRow
	// Only an operator is sent these two.
	ProviderMicros *int64 `json:"provider_micros"`
	MarginMicros   *int64 `json:"margin_micros"`
}

type billingTotal struct {
	Currency       string `json:"currency"`
	Calls          int64  `json:"calls"`
	Micros         int64  `json:"micros"`
	ProviderMicros *int64 `json:"provider_micros"`
	MarginMicros   *int64 `json:"margin_micros"`
}

func printBilling(w *table, res billingResponse) {
	if len(res.Data) == 0 {
		printNone(w, "use of this deployment's provider keys in "+res.Month, "")
		return
	}
	operator := res.Data[0].ProviderMicros != nil
	head := "ORG\tMODEL\tCALLS\tIN\tCACHED\tWRITTEN\tOUT\tAMOUNT"
	if operator {
		head += "\tPROVIDER COST\tMARGIN"
	}
	w.header(head)
	for _, b := range res.Data {
		org := b.OrgName
		if org == "" {
			org = b.OrgID + " (deleted)"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s/%s\t%d\t%d\t%d\t%d\t%d\t%s", org, b.Provider,
			b.BackendModel, b.Calls, b.InputTokens, b.CachedInputTokens, b.CacheWriteTokens,
			b.OutputTokens,
			money(b.Micros, b.Currency))
		if operator {
			_, _ = fmt.Fprintf(w, "\t%s\t%s", money(*b.ProviderMicros, b.Currency),
				money(*b.MarginMicros, b.Currency))
		}
		_, _ = fmt.Fprintln(w)
	}
	for _, t := range res.Totals {
		_, _ = fmt.Fprintf(w, "total %s\t\t%d\t\t\t\t\t%s", res.Month, t.Calls,
			money(t.Micros, t.Currency))
		if operator {
			_, _ = fmt.Fprintf(w, "\t%s\t%s", money(*t.ProviderMicros, t.Currency),
				money(*t.MarginMicros, t.Currency))
		}
		_, _ = fmt.Fprintln(w)
	}
}

func money(micros int64, currency string) string {
	return currency + " " + policy.FormatMicros(micros)
}

func chf(micros int64) string { return money(micros, "CHF") }

// creditResponse is GET /v1/billing/account.
type creditResponse struct {
	Payments bool                `json:"payments"`
	Account  store.CreditAccount `json:"account"`
	Recent   []store.Payment     `json:"recent"`
}

// errNoPayments is what the payment verbs say on a deployment without them.
var errNoPayments = errors.New("this deployment takes no payments: nobody pays in advance " +
	"(see docs/billing.md)")

func (r *billingRun) credit(ctx context.Context) error {
	path, err := r.account(ctx, "/v1/billing/account")
	if err != nil {
		return err
	}
	var res creditResponse
	if err := r.c.do(ctx, "GET", path, nil, &res); err != nil {
		return err
	}
	if !res.Payments {
		return errNoPayments
	}
	return out(r.asJSON, res, func(w *table) { printCredit(w, res) })
}

func printCredit(w *table, res creditResponse) {
	a := res.Account
	show(w, "balance", chf(a.BalanceMicros))
	if a.Invoiced {
		show(w, "billed", "by invoice: it may use the keys without credit")
	}
	if a.TopUpMicros == 0 {
		show(w, "automatic top-up", "off")
	} else {
		show(w, "automatic top-up", chf(a.TopUpMicros)+" when below "+chf(a.TopUpBelowMicros))
	}
	if a.Card != nil {
		card := a.Card.Label
		if a.Card.Expires != nil {
			card += ", expires " + a.Card.Expires.Format("01/06")
		}
		show(w, "card", card)
	} else {
		show(w, "card", "none: 'keera billing topup --save-card' saves one")
	}
	if a.TopUpError != "" {
		show(w, "last top-up failed", style.warn(a.TopUpError))
	}
	if len(res.Recent) > 0 {
		_, _ = fmt.Fprintln(w)
		printPayments(w, res.Recent)
	}
}

func printPayments(w *table, list []store.Payment) {
	if len(list) == 0 {
		printNone(w, "payments", "keera billing topup --amount 100")
		return
	}
	w.header("WHEN\tKIND\tAMOUNT\tVAT\tSTATE\tBY\tNOTE")
	for _, p := range list {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", p.TS.Local().Format("2006-01-02 15:04"),
			p.Kind, chf(p.Micros), chf(p.VATMicros), p.State, p.Actor, dash(p.Note))
	}
}

// amountMicros reads --amount, which every verb that moves money needs.
func (r *billingRun) amountMicros() (int64, error) {
	if !given(r.fs, "amount") {
		return 0, fmt.Errorf("--amount is required, in CHF (see: keera help billing %s)", r.verb)
	}
	return micros(r.amount), nil
}

func (r *billingRun) topUp(ctx context.Context) error {
	amount, err := r.amountMicros()
	if err != nil {
		return err
	}
	path, err := r.account(ctx, "/v1/billing/topups")
	if err != nil {
		return err
	}
	var res struct {
		Payment store.Payment `json:"payment"`
		URL     string        `json:"url"`
	}
	if err := r.c.do(ctx, "POST", path,
		map[string]any{"amount_micros": amount, "save_card": r.saveCard}, &res); err != nil {
		return err
	}
	if r.asJSON {
		return out(true, res, nil)
	}
	// The address alone on stdout, so it can go straight to a browser.
	fmt.Fprintf(os.Stderr, "Pay %s plus %s VAT on PostFinance Checkout. The credit is added "+
		"once the payment is through:\n", chf(res.Payment.Micros), chf(res.Payment.VATMicros))
	fmt.Println(res.URL)
	return nil
}

func (r *billingRun) autoTopUp(ctx context.Context) error {
	var below, amount int64
	switch {
	case r.off && (given(r.fs, "below") || given(r.fs, "amount")):
		return errors.New("--off takes neither --below nor --amount")
	case r.off:
	case !given(r.fs, "below") || !given(r.fs, "amount"):
		return errors.New("give --below and --amount, such as --below 50 --amount 200, or --off")
	default:
		below, amount = micros(r.below), micros(r.amount)
	}
	path, err := r.account(ctx, "/v1/billing/account")
	if err != nil {
		return err
	}
	var a store.CreditAccount
	if err := r.c.do(ctx, "PATCH", path,
		map[string]int64{"topup_below_micros": below, "topup_micros": amount}, &a); err != nil {
		return err
	}
	return out(r.asJSON, a, func(w *table) {
		if a.TopUpMicros == 0 {
			_, _ = fmt.Fprintln(w, "The automatic top-up is off.")
			return
		}
		_, _ = fmt.Fprintf(w, "Below %s, the saved card is charged %s.\n",
			chf(a.TopUpBelowMicros), chf(a.TopUpMicros))
		if a.Card == nil {
			_, _ = fmt.Fprintln(w, style.warn("There is no saved card yet: "+
				"'keera billing topup --save-card' saves one."))
		}
	})
}

func (r *billingRun) forgetCard(ctx context.Context) error {
	path, err := r.account(ctx, "/v1/billing/account/card")
	if err != nil {
		return err
	}
	if err := r.c.do(ctx, "DELETE", path, nil, nil); err != nil {
		return err
	}
	fmt.Println("The saved card is forgotten, and the automatic top-up stops with it.")
	return nil
}

func (r *billingRun) grant(ctx context.Context) error {
	amount, err := r.amountMicros()
	if err != nil {
		return err
	}
	if r.org == "" {
		return errors.New("--org is required: credit is granted to one organisation")
	}
	if r.note == "" {
		return errors.New("--note is required: it tells the organisation why")
	}
	var p store.Payment
	if err := r.c.do(ctx, "POST", "/v1/billing/grants", map[string]any{
		"org_id": r.org, "amount_micros": amount, "note": r.note,
	}, &p); err != nil {
		return err
	}
	return out(r.asJSON, p, func(w *table) {
		_, _ = fmt.Fprintf(w, "Granted %s to %s.\n", chf(p.Micros), p.OrgID)
	})
}

func (r *billingRun) invoiced(ctx context.Context) error {
	on, err := strconv.ParseBool(map[string]string{"on": "true", "off": "false"}[r.fs.Arg(0)])
	if err != nil {
		return errors.New("usage: keera billing invoiced on|off --org <id>")
	}
	if r.org == "" {
		return errors.New("--org is required")
	}
	var a store.CreditAccount
	if err := r.c.do(ctx, "PATCH", inOrg("/v1/billing/account", r.org),
		map[string]bool{"invoiced": on}, &a); err != nil {
		return err
	}
	return out(r.asJSON, a, func(w *table) {
		if on {
			_, _ = fmt.Fprintf(w, "%s is billed by invoice: it may use the keys without credit.\n", a.OrgID)
			return
		}
		_, _ = fmt.Fprintf(w, "%s pays in advance again.\n", a.OrgID)
	})
}

func (r *billingRun) payments(ctx context.Context) error {
	q := url.Values{}
	setIfGiven(q, map[string]string{"org_id": r.org, "limit": strconv.Itoa(r.limit)})
	list, err := list[store.Payment](ctx, r.c, "/v1/billing/payments?"+q.Encode())
	if err != nil {
		return err
	}
	return out(r.asJSON, list, func(w *table) { printPayments(w, list) })
}
