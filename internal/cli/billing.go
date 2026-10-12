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
	// An operator moves another organisation's money with grant and
	// invoiced, so those name it every time rather than trusting whichever
	// organisation is chosen. Every other verb acts on the chosen one.
	shared := []string{"json"}
	switch verbAsked("billing", args) {
	case "grant", "invoiced":
		shared = append(shared, "org")
	case "forget-card":
		shared = append(shared, "yes")
	}
	r := &billingRun{cmdRun: newCmdRun("billing", args, shared...)}
	if f := r.fs.Lookup("org"); f != nil {
		f.Usage = "organisation, by name or id (required)"
	}
	r.fs.StringVar(&r.month, "month", "", "the month to bill, as YYYY-MM (default: this month)")
	r.fs.Float64Var(&r.amount, "amount", 0, "an amount in CHF, such as 100")
	r.fs.Float64Var(&r.below, "below", 0, "the balance in CHF that starts an automatic top-up")
	r.fs.BoolVar(&r.off, "off", false, "turn the automatic top-up off")
	r.reword("invoiced", "off", "make it pay in advance again")
	r.fs.BoolVar(&r.saveCard, "save-card", false,
		"keep the card for automatic top-ups and the next payment")
	r.fs.StringVar(&r.note, "note", "", "why, shown in the organisation's payments")
	r.fs.IntVar(&r.limit, "limit", 50, limitUsage)
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
	Month    string             `json:"month"`
	Payments bool               `json:"payments"`
	Data     []store.BillingRow `json:"data"`
	Total    billingTotal       `json:"total"`
}

type billingTotal struct {
	Calls  int64 `json:"calls"`
	Micros int64 `json:"micros"`
}

func printBilling(w *table, res billingResponse) {
	if len(res.Data) == 0 {
		printNone(w, "use of this deployment's provider keys in "+res.Month, "")
		return
	}
	head := "ORG\tMODEL\tCALLS\tIN\tCACHED\tWRITTEN\tOUT\tAMOUNT"
	if res.Payments {
		head += "\tFROM CREDIT"
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
			chf(b.Micros))
		if res.Payments {
			_, _ = fmt.Fprintf(w, "\t%s", chf(b.CreditMicros))
		}
		_, _ = fmt.Fprintln(w)
	}
	t := res.Total
	_, _ = fmt.Fprintf(w, "total %s\t\t%d\t\t\t\t\t%s", res.Month, t.Calls, chf(t.Micros))
	_, _ = fmt.Fprintln(w)
}

func chf(micros int64) string { return policy.Currency + " " + policy.FormatMicros(micros) }

// creditResponse is GET /v1/billing/account.
type creditResponse struct {
	Payments bool                `json:"payments"`
	Account  store.CreditAccount `json:"account"`
	Recent   []store.Payment     `json:"recent"`
}

// confirmForget names the card and what stops with it, as the panel does,
// and makes the administrator confirm. PostFinance forgets it too, so it
// cannot be put back.
func (r *billingRun) confirmForget(ctx context.Context) error {
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
	if res.Account.Card == nil {
		return errors.New("there is no saved card to forget")
	}
	lines := []string{"  PostFinance forgets it too, so it cannot be put back"}
	if res.Account.TopUpMicros > 0 {
		lines = append(lines, "  the automatic top-up stops")
	}
	lines = append(lines, "'keera billing topup --save-card' saves a card again.")
	return confirm("Forgetting the card "+res.Account.Card.Label+":", lines,
		`word "forget"`, "forget", "the card was kept")
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
		printPayments(w, res.Recent, nil)
	}
}

// printPayments lists payments. orgNames is set when they span organisations,
// as an operator's do when no organisation is chosen, and adds a column.
func printPayments(w *table, list []store.Payment, orgNames map[string]string) {
	if len(list) == 0 {
		printNone(w, "payments", "keera billing topup --amount 100")
		return
	}
	head := "WHEN\tKIND\tAMOUNT\tVAT\tSTATE\tBY\tNOTE"
	if orgNames != nil {
		head = "ORG\t" + head
	}
	w.header(head)
	for _, p := range list {
		if orgNames != nil {
			_, _ = fmt.Fprintf(w, "%s\t", labelled(orgNames, p.OrgID))
		}
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
	if !r.yes {
		if err := r.confirmForget(ctx); err != nil {
			return err
		}
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
	if r.orgFlag == "" {
		return errors.New("--org is required: credit is granted to one organisation")
	}
	if r.note == "" {
		return errors.New("--note is required: it tells the organisation why")
	}
	orgID, err := resolveOrg(ctx, r.c, r.orgFlag)
	if err != nil {
		return err
	}
	var p store.Payment
	if err := r.c.do(ctx, "POST", "/v1/billing/grants", map[string]any{
		"org_id": orgID, "amount_micros": amount, "note": r.note,
	}, &p); err != nil {
		return err
	}
	return out(r.asJSON, p, func(w *table) {
		_, _ = fmt.Fprintf(w, "Granted %s to %s.\n", chf(p.Micros), p.OrgID)
	})
}

func (r *billingRun) invoiced(ctx context.Context) error {
	if r.orgFlag == "" {
		return errors.New("--org is required: the organisation billed by invoice")
	}
	orgID, err := resolveOrg(ctx, r.c, r.orgFlag)
	if err != nil {
		return err
	}
	on := !r.off
	var a store.CreditAccount
	if err := r.c.do(ctx, "PATCH", inOrg("/v1/billing/account", orgID),
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
	payments, err := list[store.Payment](ctx, r.c, "/v1/billing/payments?"+q.Encode())
	if err != nil {
		return err
	}
	var names map[string]string
	if r.org == "" && !r.asJSON && spansOrgs(payments) {
		orgs, err := list[store.Org](ctx, r.c, "/v1/orgs")
		if err != nil {
			return err
		}
		names = make(map[string]string, len(orgs))
		for _, o := range orgs {
			names[o.ID] = o.Name
		}
	}
	return out(r.asJSON, payments, func(w *table) { printPayments(w, payments, names) })
}

func spansOrgs(list []store.Payment) bool {
	for _, p := range list {
		if p.OrgID != list[0].OrgID {
			return true
		}
	}
	return false
}
