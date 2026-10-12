package policy

// Currency is what every price, budget, bill and credit is in.
const Currency = "CHF"

// Billing is how calls to a model on the deployment's own provider key are
// billed: the organisation pays the list price.
type Billing struct {
	Provider string
	// Credit says organisations pay in advance, so each call takes its list
	// price from their credit. It is false without payments. See
	// docs/billing.md.
	Credit bool
}

// BillLine is one billable call to a model on the deployment's key.
type BillLine struct {
	// Alias is the organisation's name for the model.
	Alias        string
	Provider     string
	BackendModel string
	InputTokens  int
	// CachedInputTokens and CacheWriteTokens are parts of InputTokens, as
	// providers report them.
	CachedInputTokens int
	CacheWriteTokens  int
	OutputTokens      int
	// Micros is what the organisation is billed, at list price.
	Micros int64
	// CreditMicros is what it takes from the organisation's credit.
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
	var credit int64
	if m.Billing.Credit {
		credit = micros
	}
	return BillLine{
		Alias:             m.Alias,
		Provider:          m.Billing.Provider,
		BackendModel:      m.BackendModel,
		InputTokens:       t.Input,
		CachedInputTokens: t.CachedInput,
		CacheWriteTokens:  t.CacheWrite,
		OutputTokens:      t.Output,
		Micros:            micros,
		CreditMicros:      credit,
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
