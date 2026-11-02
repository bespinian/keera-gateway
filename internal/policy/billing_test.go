package policy

import "testing"

func TestAModelOnTheDeploymentsKeyIsBilledAtListPrice(t *testing.T) {
	m := Model{
		Alias: "frontier", BackendModel: "claude-opus-5-5",
		InputMicrosPerMTok: 4_000_000, OutputMicrosPerMTok: 20_000_000,
		CachedInputMicrosPerMTok: 400_000,
		Billing:                  &Billing{Provider: "anthropic"},
	}
	line, ok := m.Bill(Tokens{Input: 1_000_000, CachedInput: 500_000, Output: 100_000}, Tokens{})
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
	line, _ := m.Bill(Tokens{Input: 1_000_000, CacheWrite: 1_000_000}, Tokens{})
	if line.CacheWriteTokens != 1_000_000 || line.Micros != 5_000_000 {
		t.Errorf("line = %+v, want a million written at 5.00", line)
	}
}

func TestOnlyBilledModelsAreBilled(t *testing.T) {
	own := Model{InputMicrosPerMTok: 1_000_000}
	if _, ok := own.Bill(Tokens{Input: 10, Output: 10}, Tokens{}); ok {
		t.Error("a model on the organisation's own key was billed")
	}
	plan := Model{InputMicrosPerMTok: 1_000_000, Subscription: true, Billing: &Billing{}}
	if _, ok := plan.Bill(Tokens{Input: 10, Output: 10}, Tokens{}); ok {
		t.Error("a model a Claude plan pays for was billed")
	}
}

func TestABillLineTakesTheListPriceFromTheCredit(t *testing.T) {
	m := Model{
		InputMicrosPerMTok: 4_000_000,
		Billing:            &Billing{Provider: "anthropic", Credit: true},
	}
	line, _ := m.Bill(Tokens{Input: 1_000_000}, Tokens{})
	if line.Micros != 4_000_000 || line.CreditMicros != 4_000_000 {
		t.Errorf("billed %d, credit %d; want 4.00 and 4.00", line.Micros, line.CreditMicros)
	}
	m.Billing.Credit = false
	if line, _ := m.Bill(Tokens{Input: 1_000_000}, Tokens{}); line.CreditMicros != 0 {
		t.Errorf("without payments the credit is charged %d", line.CreditMicros)
	}
}

func TestAModelWithAGatewayFeeIsBilledEveryTokenAtTheFee(t *testing.T) {
	m := Model{
		Alias: "mistral", Provider: "mistral", BackendModel: "mistral-large",
		InputMicrosPerMTok: 2_000_000,
		Billing:            &Billing{Provider: "mistral", Credit: true, FeeMicrosPerMTok: 50_000},
	}
	reported := Tokens{Input: 1_000_000, CachedInput: 600_000, Output: 1_000_000}
	line, ok := m.Bill(reported, Tokens{Input: 1, Output: 1})
	if !ok {
		t.Fatal("not billed")
	}
	// Two million tokens at 0.05, cached or not, as the provider reported.
	if line.Micros != 100_000 || line.CreditMicros != 100_000 || !line.GatewayFee ||
		line.InputTokens != 1_000_000 || line.Provider != "mistral" {
		t.Errorf("line = %+v, want 0.10 for the reported two million tokens", line)
	}
}

// A self-hosted backend can report anything, so the gateway's own count bills.
func TestASelfHostedModelIsBilledOnTheGatewaysCount(t *testing.T) {
	m := Model{
		Alias:   "keera-code",
		Billing: &Billing{FeeMicrosPerMTok: 1_000_000},
	}
	if !m.CountsTokens() {
		t.Fatal("a self-hosted model with a fee is not counted")
	}
	line, _ := m.Bill(Tokens{}, Tokens{Input: 3_000, Output: 1_000})
	if line.Micros != 4_000 || line.InputTokens != 3_000 || line.OutputTokens != 1_000 {
		t.Errorf("line = %+v, want 4000 counted tokens at 1.00 per million", line)
	}
	// It names no backend model, so the alias stands in.
	if line.BackendModel != "keera-code" || line.Provider != "" {
		t.Errorf("line names %q/%q", line.Provider, line.BackendModel)
	}
}

// A Claude plan pays for the model, but not for the gateway.
func TestASubscriptionModelPaysTheGatewayFee(t *testing.T) {
	plan := Model{
		Provider: "anthropic", Subscription: true, InputMicrosPerMTok: 3_000_000,
		Billing: &Billing{Provider: "anthropic", FeeMicrosPerMTok: 1_000_000},
	}
	line, ok := plan.Bill(Tokens{Input: 1_000_000}, Tokens{})
	if !ok || line.Micros != 1_000_000 || !line.GatewayFee {
		t.Errorf("line = %+v, %v; want 1.00 as the gateway fee", line, ok)
	}
}

func TestOnlyASelfHostedModelIsPricedAtTheFee(t *testing.T) {
	self := Model{
		InputMicrosPerMTok: 9, OutputMicrosPerMTok: 9,
		LongPrompt: &PriceTier{AboveTokens: 10, InputMicrosPerMTok: 99},
	}
	if !self.PriceAtFee(50_000) {
		t.Fatal("a self-hosted model was not priced at the fee")
	}
	if self.InputMicrosPerMTok != 50_000 || self.OutputMicrosPerMTok != 50_000 ||
		self.CachedInputMicrosPerMTok != 50_000 || self.CacheWriteMicrosPerMTok != 50_000 ||
		self.LongPrompt != nil {
		t.Errorf("priced %+v, want every price at 0.05", self)
	}
	hosted := Model{Provider: "mistral", InputMicrosPerMTok: 9}
	if hosted.PriceAtFee(50_000) || hosted.InputMicrosPerMTok != 9 {
		t.Errorf("a hosted model was priced at the fee: %+v", hosted)
	}
	// Without a fee it is free: nobody prices it but the deployment.
	free := Model{InputMicrosPerMTok: 9, CachedInputMicrosPerMTok: 9}
	if !free.PriceAtFee(0) || free.InputMicrosPerMTok != 0 || free.CachedInputMicrosPerMTok != 0 {
		t.Errorf("without a fee, a self-hosted model is priced %+v, want 0", free)
	}
}
