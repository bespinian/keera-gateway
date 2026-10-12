package policy

import "testing"

func TestAModelOnTheDeploymentsKeyIsBilledAtListPrice(t *testing.T) {
	m := Model{
		Alias: "frontier", BackendModel: "claude-opus-5-5",
		InputMicrosPerMTok: 4_000_000, OutputMicrosPerMTok: 20_000_000,
		CachedInputMicrosPerMTok: 400_000,
		Billing:                  &Billing{Provider: "anthropic"},
	}
	line, ok := m.Bill(Tokens{Input: 1_000_000, CachedInput: 500_000, Output: 100_000})
	if !ok {
		t.Fatal("not billed")
	}
	// 500k uncached at 4.00, 500k cached at 0.40, 100k out at 20.00.
	const want = 2_000_000 + 200_000 + 2_000_000
	if line.Micros != want {
		t.Errorf("billed %d, want %d", line.Micros, want)
	}
	if line.Alias != "frontier" || line.Provider != "anthropic" ||
		line.BackendModel != "claude-opus-5-5" || line.CachedInputTokens != 500_000 {
		t.Errorf("line = %+v", line)
	}
}

func TestABillLineCarriesTheCacheWrites(t *testing.T) {
	m := Model{
		InputMicrosPerMTok: 4_000_000, CacheWriteMicrosPerMTok: 5_000_000,
		Billing: &Billing{Provider: "anthropic"},
	}
	line, _ := m.Bill(Tokens{Input: 1_000_000, CacheWrite: 1_000_000})
	if line.CacheWriteTokens != 1_000_000 || line.Micros != 5_000_000 {
		t.Errorf("line = %+v, want a million written at 5.00", line)
	}
}

func TestOnlyTheDeploymentsKeyIsBilled(t *testing.T) {
	own := Model{InputMicrosPerMTok: 1_000_000}
	if _, ok := own.Bill(Tokens{Input: 10, Output: 10}); ok {
		t.Error("a model on the organisation's own key was billed")
	}
	plan := Model{InputMicrosPerMTok: 1_000_000, Subscription: true, Billing: &Billing{}}
	if _, ok := plan.Bill(Tokens{Input: 10, Output: 10}); ok {
		t.Error("a model a Claude plan pays for was billed")
	}
}

func TestABillLineTakesTheListPriceFromTheCredit(t *testing.T) {
	m := Model{
		InputMicrosPerMTok: 4_000_000,
		Billing:            &Billing{Provider: "anthropic", Credit: true},
	}
	line, _ := m.Bill(Tokens{Input: 1_000_000})
	if line.Micros != 4_000_000 || line.CreditMicros != 4_000_000 {
		t.Errorf("billed %d, credit %d; want 4.00 and 4.00", line.Micros, line.CreditMicros)
	}
	m.Billing.Credit = false
	if line, _ := m.Bill(Tokens{Input: 1_000_000}); line.CreditMicros != 0 {
		t.Errorf("without payments the credit is charged %d", line.CreditMicros)
	}
}
