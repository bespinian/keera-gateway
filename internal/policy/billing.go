package policy

import (
	"cmp"
	"time"
)

// Currency is what every price, budget, bill and credit is in.
const Currency = "CHF"

// Billing is how calls to a model are billed. On the deployment's own provider
// key the organisation pays the list price. On any other model it pays
// FeeMicrosPerMTok for the gateway, when the deployment sets one.
type Billing struct {
	Provider string
	// Credit says organisations prepay, so each call takes from their credit.
	// It is false without payments. See docs/billing.md.
	Credit bool
	// FeeMicrosPerMTok is the gateway's price per million tokens on a model
	// the deployment does not bill otherwise. Zero means list price.
	FeeMicrosPerMTok int64
	// AtProvider says every backend is the provider's own endpoint, so the
	// usage it reports is billed. Any other backend can report what it likes.
	AtProvider bool
}

// BillLine is one billable call: to a model on the deployment's key, or the
// gateway fee on any other.
type BillLine struct {
	Alias        string
	Provider     string
	BackendModel string
	InputTokens  int
	// CachedInputTokens and CacheWriteTokens are parts of InputTokens.
	CachedInputTokens int
	CacheWriteTokens  int
	OutputTokens      int
	// Micros is the bill; CreditMicros is what it takes from credit.
	Micros       int64
	CreditMicros int64
	// GatewayFee says the line is the gateway fee, not a list price.
	GatewayFee bool
}

// Bill is the bill line for one call to m, or false when m is not billed.
// reported is what the backend said it used. counted is the gateway's own
// estimate from what it passed on, which bills a model that CountsTokens.
func (m Model) Bill(reported, counted Tokens) (BillLine, bool) {
	if m.Billing == nil {
		return BillLine{}, false
	}
	if m.Billing.FeeMicrosPerMTok > 0 {
		return m.fee(reported, counted), true
	}
	if m.Subscription {
		return BillLine{}, false
	}
	t := reported.clamped()
	return m.line(t, m.Cost(t)), true
}

// fee is the gateway fee for one call: every token at the same price, cached
// or not, since the gateway handles each the same.
func (m Model) fee(reported, counted Tokens) BillLine {
	t := reported
	if m.CountsTokens() {
		t = counted
	}
	t = t.clamped()
	const perM = 1_000_000
	line := m.line(t, int64(t.Input+t.Output)*m.Billing.FeeMicrosPerMTok/perM)
	line.GatewayFee = true
	// A self-hosted model may name no model of its backend's.
	line.BackendModel = cmp.Or(line.BackendModel, m.Alias)
	return line
}

func (m Model) line(t Tokens, micros int64) BillLine {
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
	}
}

// CountsTokens says the gateway bills m on its own count of the tokens: m has
// a gateway fee and is not served by its provider's own endpoint. Naming a
// provider is not enough, or any server could be named one and report nothing.
func (m Model) CountsTokens() bool {
	return m.Billing != nil && m.Billing.FeeMicrosPerMTok > 0 && !m.Billing.AtProvider
}

// SelfHosted says m runs on a server of the organisation's, not at a hosted
// provider.
func (m Model) SelfHosted() bool { return m.Provider == "" }

// PriceAtFee sets every price of a self-hosted model to the gateway fee, or to
// 0 without one. That is all it costs the organisation, so its budgets and
// reports show what it pays, and nobody sets a price of their own. It reports
// whether it did; other models keep their prices.
func (m *Model) PriceAtFee(fee int64) bool {
	if !m.SelfHosted() {
		return false
	}
	m.InputMicrosPerMTok, m.OutputMicrosPerMTok = fee, fee
	m.CachedInputMicrosPerMTok, m.CacheWriteMicrosPerMTok = fee, fee
	m.LongPrompt = nil
	return true
}

// ErrNoCredit refuses a billed request when credit has run out, or is held by requests still running.
type ErrNoCredit struct {
	BalanceMicros int64
	// HeldMicros is the most the requests still running may take.
	HeldMicros int64
}

func (e *ErrNoCredit) Error() string {
	if e.HeldMicros > 0 {
		return "your organisation's credit (balance " + FormatMoney(e.BalanceMicros) +
			") is held by requests still running, which may take up to " +
			FormatMoney(e.HeldMicros) + "; try again when they end, or an administrator " +
			"can add credit under Billing"
	}
	return "your organisation has used up its credit (balance " +
		FormatMoney(e.BalanceMicros) + "); an administrator can add credit under Billing"
}

// ErrChargeDue refuses every call of an organisation with a recurring charge
// due that the saved card cannot pay: there is no card, or it was declined.
type ErrChargeDue struct {
	Since time.Time
}

func (e *ErrChargeDue) Error() string {
	return "your organisation has a recurring charge due since " +
		e.Since.UTC().Format(time.DateOnly) + " that no saved card can pay; an administrator " +
		"can pay it and save a card under Billing"
}
