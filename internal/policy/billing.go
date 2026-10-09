package policy

// Billing is how calls to a model on the deployment's own provider key are
// billed: the organisation pays the list price, and the deployment pays the
// provider that less its discount.
type Billing struct {
	Provider string
	// Currency is the provider's, which the list prices are quoted in.
	Currency string
	// DiscountBP is the provider's discount on its list price, in basis
	// points: 1250 is 12.5%.
	DiscountBP int64
	// CreditRate converts the list price into the credit organisations pay
	// in advance: CHF micros per micro-unit of Currency, times a million. A
	// rate of 800_000 turns USD 1.00 into CHF 0.80. Zero takes nothing from
	// the credit, which is how it is without payments. See docs/billing.md.
	CreditRate int64
}

// BillLine is one billable call to a model on the deployment's key.
type BillLine struct {
	// Alias is the organisation's name for the model.
	Alias        string
	Provider     string
	BackendModel string
	Currency     string
	InputTokens  int
	// CachedInputTokens and CacheWriteTokens are parts of InputTokens, as
	// providers report them.
	CachedInputTokens int
	CacheWriteTokens  int
	OutputTokens      int
	// Micros is what the organisation is billed, at list price.
	Micros int64
	// ProviderMicros is what the provider charges the deployment for it.
	ProviderMicros int64
	// CreditMicros is what it takes from the organisation's credit, in CHF.
	CreditMicros int64
}

// Bill is the bill line for one call to m, or false when m is not on the
// deployment's key. A subscription pays for its own calls.
func (m Model) Bill(t Tokens) (BillLine, bool) {
	if m.Billing == nil || m.Subscription {
		return BillLine{}, false
	}
	t = t.clamped()
	micros := m.Cost(t)
	return BillLine{
		Alias:             m.Alias,
		Provider:          m.Billing.Provider,
		BackendModel:      m.BackendModel,
		Currency:          m.Billing.Currency,
		InputTokens:       t.Input,
		CachedInputTokens: t.CachedInput,
		CacheWriteTokens:  t.CacheWrite,
		OutputTokens:      t.Output,
		Micros:            micros,
		ProviderMicros:    micros * (10_000 - m.Billing.DiscountBP) / 10_000,
		CreditMicros:      micros * m.Billing.CreditRate / 1_000_000,
	}, true
}

// ErrNoCredit refuses a request that would use the deployment's provider key
// for an organisation whose credit has run out, or is held by requests still
// running.
type ErrNoCredit struct {
	BalanceMicros int64
	// HeldMicros is the most the requests still running may take.
	HeldMicros int64
}

func (e *ErrNoCredit) Error() string {
	if e.HeldMicros > 0 {
		return "your organisation's credit (balance CHF " + FormatMicros(e.BalanceMicros) +
			") is held by requests still running, which may take up to CHF " +
			FormatMicros(e.HeldMicros) + "; try again when they end, or an administrator " +
			"can add credit under Billing"
	}
	return "your organisation has used up its credit (balance CHF " +
		FormatMicros(e.BalanceMicros) + "); an administrator can add credit under Billing"
}
