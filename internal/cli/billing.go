package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

type billingRun struct {
	*cmdRun
	month    string
	amount   float64
	below    float64
	off      bool
	saveCard bool
	note     string
	limit    int
	output   string
	name     string
	every    string
	start    string
}

func billingCmd(ctx context.Context, args []string) error {
	r := &billingRun{cmdRun: newCmdRun("billing", args)}
	// grant and invoiced move another organisation's money, so they name it
	// every time rather than trust the chosen one.
	r.reword("grant", "org", "organisation, by name or id (required)")
	r.reword("invoiced", "org", "organisation, by name or id (required)")
	r.reword("charge", "org", "organisation, by name or id (required)")
	r.fs.StringVar(&r.month, "month", "", "the month to bill, as YYYY-MM (default: this month)")
	r.fs.Float64Var(&r.amount, "amount", 0, "an amount in CHF, such as 100")
	r.reword("topup", "amount", "an amount in CHF, such as 100 (required)")
	r.reword("grant", "amount", "an amount in CHF, such as 100 (required)")
	r.reword("charge", "amount", "the price of each period in CHF, without VAT, such as 500 (required)")
	r.fs.StringVar(&r.name, "name", "", "what the charge pays for, shown on the payment page and the receipt (required)")
	r.fs.StringVar(&r.every, "every", "", "month or year (required)")
	r.fs.StringVar(&r.start, "start", "", "the day the first period starts, as YYYY-MM-DD (default: today)")
	r.fs.Float64Var(&r.below, "below", 0, "the balance in CHF that starts an automatic top-up")
	r.fs.BoolVar(&r.off, "off", false, "turn the automatic top-up off")
	r.reword("invoiced", "off", "make it pay in advance again")
	r.fs.BoolVar(&r.saveCard, "save-card", false,
		"keep the card for automatic top-ups and the next payment")
	r.fs.StringVar(&r.note, "note", "", "why, shown in the organisation's payments")
	r.reword("grant", "note", "why, shown in the organisation's payments (required)")
	r.fs.IntVar(&r.limit, "limit", reportLimit, limitUsage)
	r.fs.StringVar(&r.output, "output", "",
		"the file to save it to, or - for standard output (default: keera-receipt-<date>-<payment>.pdf)")
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
	case "receipt":
		return r.receipt(ctx)
	case "charges":
		return r.charges(ctx)
	case "charge":
		return r.addCharge(ctx)
	case "end-charge":
		return r.endCharge(ctx)
	case "pay-charge":
		return r.payCharge(ctx)
	default:
		return r.report(ctx)
	}
}

// account is path for the organisation given, or the only one there is.
func (r *billingRun) account(ctx context.Context, path string) (string, error) {
	org, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return "", err
	}
	return inOrg(path, org), nil
}

func (r *billingRun) report(ctx context.Context) error {
	org, err := orgIfGiven(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	q := url.Values{}
	setIfGiven(q, map[string]string{"org_id": org, "month": r.month})
	return doOut(ctx, r.c, "GET", "/v1/billing?"+q.Encode(), nil, r.asJSON, printBilling)
}

type billingResponse struct {
	Month    string             `json:"month"`
	Payments bool               `json:"payments"`
	Data     []store.BillingRow `json:"data"`
	Total    billingTotal       `json:"total"`
}

type billingTotal struct {
	Calls        int64 `json:"calls"`
	BilledMicros int64 `json:"billed_micros"`
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
			policy.FormatMoney(b.BilledMicros))
		if res.Payments {
			_, _ = fmt.Fprintf(w, "\t%s", policy.FormatMoney(b.CreditMicros))
		}
		_, _ = fmt.Fprintln(w)
	}
	t := res.Total
	_, _ = fmt.Fprintf(w, "total %s\t\t%d\t\t\t\t\t%s", res.Month, t.Calls, policy.FormatMoney(t.BilledMicros))
	_, _ = fmt.Fprintln(w)
}

type creditResponse struct {
	Payments bool                    `json:"payments"`
	Account  store.CreditAccount     `json:"account"`
	Recent   []store.Payment         `json:"recent"`
	Charges  []store.RecurringCharge `json:"charges"`
}

// confirmForget asks first because PostFinance forgets the card too, so it
// cannot be put back.
func (r *billingRun) confirmForget(ctx context.Context, orgID string) error {
	res, err := r.creditAccount(ctx, orgID)
	if err != nil {
		return err
	}
	if res.Account.Card == nil {
		return errors.New("there is no saved card to forget")
	}
	if len(res.Charges) > 0 {
		return errors.New("the card pays this organisation's recurring charges, so it cannot " +
			"be forgotten; 'keera billing topup --save-card' replaces it with another")
	}
	lines := []string{"  PostFinance forgets it too, so it cannot be put back"}
	if res.Account.TopUpMicros > 0 {
		lines = append(lines, "  the automatic top-up stops")
	}
	lines = append(lines, "'keera billing topup --save-card' saves a card again.")
	return confirm("Forgetting the card "+res.Account.Card.Label+":", lines,
		`word "forget"`, "forget", "the card was kept")
}

var errNoPayments = errors.New("this deployment takes no payments: nobody pays in advance " +
	"(see https://docs.keera.ch/manage/billing/)")

func (r *billingRun) creditAccount(ctx context.Context, orgID string) (creditResponse, error) {
	var res creditResponse
	if err := r.c.do(ctx, "GET", inOrg("/v1/billing/account", orgID), nil, &res); err != nil {
		return res, err
	}
	if !res.Payments {
		return res, errNoPayments
	}
	return res, nil
}

func (r *billingRun) credit(ctx context.Context) error {
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	res, err := r.creditAccount(ctx, orgID)
	if err != nil {
		return err
	}
	return out(r.asJSON, res, func(w *table) { printCredit(w, res) })
}

func printCredit(w *table, res creditResponse) {
	a := res.Account
	show(w, "balance", policy.FormatMoney(a.BalanceMicros))
	if a.Invoiced {
		show(w, "billed", "by invoice: it may use the keys without credit")
	}
	if a.TopUpMicros == 0 {
		show(w, "automatic top-up", "off")
	} else {
		show(w, "automatic top-up", policy.FormatMoney(a.TopUpMicros)+" when below "+policy.FormatMoney(a.TopUpBelowMicros))
	}
	if a.Card != nil {
		card := a.Card.Label
		if a.Card.ExpiresAt != nil {
			card += ", expires " + a.Card.ExpiresAt.Format("01/06")
		}
		show(w, "card", card)
	} else {
		show(w, "card", "none: 'keera billing topup --save-card' saves one")
	}
	if a.TopUpError != "" {
		show(w, "last top-up failed", style.warn(a.TopUpError))
	}
	if len(res.Charges) > 0 {
		_, _ = fmt.Fprintln(w)
		printCharges(w, res.Charges, a.Card != nil, nil)
	}
	if len(res.Recent) > 0 {
		_, _ = fmt.Fprintln(w)
		printPayments(w, res.Recent, nil)
	}
}

// printPayments adds an organisation column when orgNames is set.
func printPayments(w *table, list []store.Payment, orgNames map[string]string) {
	if len(list) == 0 {
		printNone(w, "payments", "keera billing topup --amount 100")
		return
	}
	head := "ID\tWHEN\tKIND\tAMOUNT\tVAT\tFEE\tSTATE\tBY\tNOTE"
	if orgNames != nil {
		head = "ORG\t" + head
	}
	w.header(head)
	for _, p := range list {
		if orgNames != nil {
			_, _ = fmt.Fprintf(w, "%s\t", labelled(orgNames, p.OrgID))
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", p.ID, p.CreatedAt.Local().Format("2006-01-02 15:04"),
			p.Kind, policy.FormatMoney(p.AmountMicros), policy.FormatMoney(p.VATMicros), policy.FormatMoney(p.FeeMicros), p.State, p.Actor, dash(paymentNote(p)))
	}
}

// paymentNote is what a charge paid for, and why a payment failed or a grant
// was made.
func paymentNote(p store.Payment) string {
	if p.Kind != store.PaymentCharge || p.PeriodStart == nil || p.PeriodEnd == nil {
		return p.Note
	}
	what := p.Item + ", " + p.PeriodStart.UTC().Format(time.DateOnly) + " to " +
		p.PeriodEnd.UTC().AddDate(0, 0, -1).Format(time.DateOnly)
	if p.Note != "" {
		what += ": " + p.Note
	}
	return what
}

// printCharges adds an organisation column when orgNames is set. hasCard
// says whether the organisation has a card to charge, and is ignored with
// orgNames.
func printCharges(w *table, list []store.RecurringCharge, hasCard bool, orgNames map[string]string) {
	if len(list) == 0 {
		printNone(w, "recurring charges", "")
		return
	}
	head := "ID\tNAME\tAMOUNT\tEVERY\tNEXT\tSTATE"
	if orgNames != nil {
		head = "ORG\t" + head
	}
	w.header(head)
	now := time.Now()
	for _, c := range list {
		if orgNames != nil {
			_, _ = fmt.Fprintf(w, "%s\t", labelled(orgNames, c.OrgID))
		}
		state := "charged to the saved card then"
		switch {
		case c.Error != "":
			state = style.warn("the card was declined: " + c.Error)
		case orgNames == nil && !hasCard && now.Before(c.DueAt):
			state = style.warn("no saved card: pay it before then")
		case orgNames == nil && !hasCard:
			state = style.warn("due, and no saved card: calls are refused")
		case now.After(c.DueAt):
			state = "due"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", c.ID, c.Name,
			policy.FormatMoney(c.AmountMicros), c.Every, c.DueAt.UTC().Format(time.DateOnly), state)
	}
}

func (r *billingRun) charges(ctx context.Context) error {
	org, err := orgIfGiven(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	q := url.Values{}
	setIfGiven(q, map[string]string{"org_id": org})
	charges, err := list[store.RecurringCharge](ctx, r.c, "/v1/billing/charges?"+q.Encode())
	if err != nil {
		return err
	}
	if r.asJSON {
		return out(true, charges, nil)
	}
	// One organisation's charges say whether its card can pay them.
	if org != "" || !spansOrgs(charges, func(c store.RecurringCharge) string { return c.OrgID }) {
		hasCard := true
		if len(charges) > 0 {
			res, err := r.creditAccount(ctx, charges[0].OrgID)
			if err != nil {
				return err
			}
			hasCard = res.Account.Card != nil
		}
		return out(false, charges, func(w *table) { printCharges(w, charges, hasCard, nil) })
	}
	orgs, err := list[store.Org](ctx, r.c, "/v1/orgs")
	if err != nil {
		return err
	}
	names := make(map[string]string, len(orgs))
	for _, o := range orgs {
		names[o.ID] = o.Name
	}
	return out(false, charges, func(w *table) { printCharges(w, charges, false, names) })
}

func (r *billingRun) addCharge(ctx context.Context) error {
	switch {
	case r.orgFlag == "":
		return errors.New("--org is required: the organisation to charge")
	case r.name == "":
		return errors.New("--name is required: it says what the charge pays for")
	case r.every == "":
		return errors.New("--every is required: month or year")
	}
	amount, err := r.amountMicros()
	if err != nil {
		return err
	}
	orgID, err := resolveOrg(ctx, r.c, r.orgFlag)
	if err != nil {
		return err
	}
	body := map[string]any{
		"org_id": orgID, "name": r.name, "amount_micros": amount, "every": r.every,
	}
	if r.start != "" {
		body["starts_on"] = r.start
	}
	return doOut(ctx, r.c, "POST", "/v1/billing/charges", body, r.asJSON,
		func(w *table, c store.RecurringCharge) {
			_, _ = fmt.Fprintf(w, "%s is charged %s every %s from %s, plus VAT and a transaction fee.\n",
				c.OrgID, policy.FormatMoney(c.AmountMicros), c.Every, c.StartsOn)
			_, _ = fmt.Fprintln(w, "Without a saved card, its calls are refused from that day "+
				"until an administrator pays.")
		})
}

func (r *billingRun) endCharge(ctx context.Context) error {
	return doOut(ctx, r.c, "DELETE", "/v1/billing/charges/"+url.PathEscape(r.fs.Arg(0)), nil,
		r.asJSON, func(w *table, _ map[string]any) {
			_, _ = fmt.Fprintln(w, "The charge has ended: no further period is charged.")
		})
}

func (r *billingRun) payCharge(ctx context.Context) error {
	var res struct {
		Payment store.Payment `json:"payment"`
		URL     string        `json:"url"`
	}
	if err := r.c.do(ctx, "POST", "/v1/billing/charges/"+url.PathEscape(r.fs.Arg(0))+"/pay",
		nil, &res); err != nil {
		return err
	}
	if r.asJSON {
		return out(true, res, nil)
	}
	pm := res.Payment
	fmt.Fprintf(os.Stderr, "Pay %s: %s, %s VAT and a %s transaction fee, on PostFinance "+
		"Checkout. The card is saved for the periods after:\n",
		policy.FormatMoney(pm.AmountMicros+pm.VATMicros+pm.FeeMicros), paymentNote(pm),
		policy.FormatMoney(pm.VATMicros), policy.FormatMoney(pm.FeeMicros))
	fmt.Println(res.URL)
	return nil
}

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
	pm := res.Payment
	fmt.Fprintf(os.Stderr, "Pay %s: %s credit, %s VAT and a %s transaction fee, on PostFinance "+
		"Checkout. The credit is added once the payment is through:\n",
		policy.FormatMoney(pm.AmountMicros+pm.VATMicros+pm.FeeMicros), policy.FormatMoney(pm.AmountMicros), policy.FormatMoney(pm.VATMicros), policy.FormatMoney(pm.FeeMicros))
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
	body := map[string]int64{"topup_below_micros": below, "topup_micros": amount}
	return doOut(ctx, r.c, "PATCH", path, body, r.asJSON, func(w *table, a store.CreditAccount) {
		if a.TopUpMicros == 0 {
			_, _ = fmt.Fprintln(w, "The automatic top-up is off.")
			return
		}
		_, _ = fmt.Fprintf(w, "Below %s, the saved card is charged %s.\n",
			policy.FormatMoney(a.TopUpBelowMicros), policy.FormatMoney(a.TopUpMicros))
		if a.Card == nil {
			_, _ = fmt.Fprintln(w, style.warn("There is no saved card yet: "+
				"'keera billing topup --save-card' saves one."))
		}
	})
}

func (r *billingRun) forgetCard(ctx context.Context) error {
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	if !r.yes {
		if err := r.confirmForget(ctx, orgID); err != nil {
			return err
		}
	}
	return doOut(ctx, r.c, "DELETE", inOrg("/v1/billing/account/card", orgID), nil, r.asJSON,
		func(w *table, _ map[string]any) {
			_, _ = fmt.Fprintln(w, "The saved card is forgotten, and the automatic top-up stops with it.")
		})
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
	body := map[string]any{"org_id": orgID, "amount_micros": amount, "note": r.note}
	return doOut(ctx, r.c, "POST", "/v1/billing/grants", body, r.asJSON, func(w *table, p store.Payment) {
		_, _ = fmt.Fprintf(w, "Granted %s to %s.\n", policy.FormatMoney(p.AmountMicros), p.OrgID)
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
	path, body := inOrg("/v1/billing/account", orgID), map[string]bool{"invoiced": on}
	return doOut(ctx, r.c, "PATCH", path, body, r.asJSON, func(w *table, a store.CreditAccount) {
		if on {
			_, _ = fmt.Fprintf(w, "%s is billed by invoice: it may use the keys without credit.\n", a.OrgID)
			return
		}
		_, _ = fmt.Fprintf(w, "%s pays in advance again.\n", a.OrgID)
	})
}

func (r *billingRun) payments(ctx context.Context) error {
	org, err := orgIfGiven(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	q := url.Values{}
	setIfGiven(q, map[string]string{"org_id": org, "limit": strconv.Itoa(r.limit)})
	payments, err := list[store.Payment](ctx, r.c, "/v1/billing/payments?"+q.Encode())
	if err != nil {
		return err
	}
	var names map[string]string
	if r.org == "" && !r.asJSON && spansOrgs(payments, func(p store.Payment) string { return p.OrgID }) {
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

// receipt saves the invoice PostFinance renders for a paid payment.
func (r *billingRun) receipt(ctx context.Context) error {
	paymentID := r.fs.Arg(0)
	pdf, header, err := r.c.fetch(ctx, "GET", "/v1/billing/payments/"+url.PathEscape(paymentID)+"/receipt", nil)
	if err != nil {
		return err
	}
	if r.output == "-" {
		_, err := os.Stdout.Write(pdf)
		return err
	}
	file := cmp.Or(r.output, receiptName(header), "keera-receipt-"+paymentID+".pdf")
	if err := os.WriteFile(file, pdf, 0o644); err != nil {
		return err
	}
	fmt.Println("Saved the receipt to " + file + ".")
	return nil
}

// receiptName uses the server's name for the receipt, as the panel's download
// does. Only the base is kept, so the file stays in the current directory.
func receiptName(h http.Header) string {
	_, params, err := mime.ParseMediaType(h.Get("Content-Disposition"))
	if err != nil {
		return ""
	}
	name := filepath.Base(params["filename"])
	if name == "." || name == "/" || name == ".." {
		return ""
	}
	return name
}

// spansOrgs reports whether list holds rows of more than one organisation.
func spansOrgs[T any](list []T, orgOf func(T) string) bool {
	for _, v := range list {
		if orgOf(v) != orgOf(list[0]) {
			return true
		}
	}
	return false
}
