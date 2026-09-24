package store

import (
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

func TestLookupKeyCollapsesTheWholeChain(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)

	// Each level narrows what it inherits, and each says something of its own.
	if err := st.PutPolicy(ctx, policy.ScopeOrg, f.orgID, policy.Limits{
		AllowedModels:   []string{"keera-code", "keera-speed", "keera-frontier"},
		MaxOutputTokens: new(4096), RPM: new(600), TPM: new(400000),
		BudgetMicros: new(int64(10_000_000_000)), BudgetPeriod: new(policy.PeriodMonth),
		SystemPrompt: new("Never include customer data in an example."),
	}); err != nil {
		t.Fatalf("PutPolicy org: %v", err)
	}
	if err := st.PutPolicy(ctx, policy.ScopeTeam, f.teamID, policy.Limits{
		AllowedModels:   []string{"keera-code", "keera-speed"},
		MaxOutputTokens: new(2048), RPM: new(120),
		BudgetMicros: new(int64(500_000_000)), BudgetPeriod: new(policy.PeriodDay),
		SystemPrompt: new("Prefer the payments team's own libraries."),
	}); err != nil {
		t.Fatalf("PutPolicy team: %v", err)
	}
	if err := st.PutPolicy(ctx, policy.ScopeKey, f.keyID, policy.Limits{
		AllowedModels: []string{"keera-speed", "keera-frontier"},
		RPM:           new(30),
	}); err != nil {
		t.Fatalf("PutPolicy key: %v", err)
	}

	res, err := st.LookupKey(ctx, f.hash)
	if err != nil {
		t.Fatalf("LookupKey: %v", err)
	}

	if res.Key.OrgID != f.orgID || res.Key.TeamID != f.teamID || res.Key.ID != f.keyID {
		t.Errorf("resolved key = %+v, want the fixture's ids", res.Key)
	}
	// The intersection of all three allow-lists, and nothing else: keera-code is
	// dropped by the key, keera-frontier by the team.
	if len(res.AllowedModels) != 1 || res.AllowedModels[0] != "keera-speed" {
		t.Errorf("AllowedModels = %v, want [keera-speed]", res.AllowedModels)
	}
	if res.MaxOutputTokens != 2048 {
		t.Errorf("MaxOutputTokens = %d, want 2048 - the tightest of the levels that set one",
			res.MaxOutputTokens)
	}
	// Text does not narrow: the organisation's instruction reaches the request
	// whatever the team added after it.
	const wantPrompt = "Never include customer data in an example.\n\n" +
		"Prefer the payments team's own libraries."
	if res.SystemPrompt != wantPrompt {
		t.Errorf("SystemPrompt = %q, want %q", res.SystemPrompt, wantPrompt)
	}

	// Budgets are kept per level rather than merged: two limits that both hold.
	want := []policy.Scope{
		{Type: policy.ScopeOrg, ID: f.orgID, RPM: 600, TPM: 400000,
			BudgetMicros: 10_000_000_000, Period: policy.PeriodMonth},
		{Type: policy.ScopeTeam, ID: f.teamID, RPM: 120, TPM: 0,
			BudgetMicros: 500_000_000, Period: policy.PeriodDay},
		{Type: policy.ScopeKey, ID: f.keyID, RPM: 30, TPM: 0,
			BudgetMicros: 0, Period: policy.PeriodMonth},
	}
	if len(res.Scopes) != len(want) {
		t.Fatalf("Scopes = %+v, want three levels", res.Scopes)
	}
	for i, w := range want {
		if res.Scopes[i] != w {
			t.Errorf("scope %d = %+v, want %+v", i, res.Scopes[i], w)
		}
	}
}

func TestLookupKeyWithNoPoliciesAnywhere(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)

	res, err := st.LookupKey(ctx, f.hash)
	if err != nil {
		t.Fatalf("LookupKey: %v", err)
	}
	// Unrestricted, but still three scopes: the gateway counts requests against
	// every level whether or not a limit is set on it.
	if res.AllowedModels != nil {
		t.Errorf("AllowedModels = %v, want nil - no allow-list is every model", res.AllowedModels)
	}
	if len(res.Scopes) != 3 {
		t.Errorf("%d scopes, want 3", len(res.Scopes))
	}
	for _, sc := range res.Scopes {
		if sc.RPM != 0 || sc.TPM != 0 || sc.BudgetMicros != 0 {
			t.Errorf("scope %+v carries a limit nobody set", sc)
		}
	}
}

func TestLookupKeyRejectsUnknownRevokedAndExpiredKeys(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)

	if _, err := st.LookupKey(ctx, []byte("no-such-key-hash-000000000000000")); err != policy.ErrUnknownKey {
		t.Errorf("an unknown hash gave %v, want ErrUnknownKey", err)
	}

	past := time.Now().Add(-time.Hour)
	if _, err := st.CreateKey(ctx, KeyInfo{
		ID: "key_expired", OrgID: f.orgID, Alias: "expired", Prefix: "keera_sk_exp",
		ExpiresAt: &past,
	}, []byte("hash-expired-0000000000000000000")); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if _, err := st.LookupKey(ctx, []byte("hash-expired-0000000000000000000")); err != policy.ErrKeyExpired {
		t.Errorf("an expired key gave %v, want ErrKeyExpired", err)
	}

	if err := st.RevokeKey(ctx, f.keyID); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	if _, err := st.LookupKey(ctx, f.hash); err != policy.ErrKeyRevoked {
		t.Errorf("a revoked key gave %v, want ErrKeyRevoked", err)
	}
	// Revoking twice is not a second event.
	if err := st.RevokeKey(ctx, f.keyID); err != ErrNotFound {
		t.Errorf("revoking an already revoked key gave %v, want ErrNotFound", err)
	}
	// A key that has been revoked is kept, so its usage history keeps
	// resolving.
	keys, err := st.KeySummaries(ctx, KeyQuery{OrgID: f.orgID})
	if err != nil {
		t.Fatalf("KeySummaries: %v", err)
	}
	if len(keys) != 2 {
		t.Errorf("%d keys after a revocation, want both still listed", len(keys))
	}
}

func TestLookupKeyIgnoresATeamInAnotherOrganisation(t *testing.T) {
	// The control plane refuses to bind a key to a team outside its own
	// organisation. This is the second lock on the same door: a row written
	// another way must resolve as a key with no team - inheriting nothing and
	// consuming nothing of a tenant it does not belong to - rather than
	// inheriting the other tenant's guardrails.
	st, ctx := db(t)
	f := newFixture(t, st, ctx)

	if _, err := st.CreateOrg(ctx, "org_2", "Another Bank"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := st.CreateTeam(ctx, "team_elsewhere", "org_2", "Somebody Else"); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if err := st.PutPolicy(ctx, policy.ScopeTeam, "team_elsewhere", policy.Limits{
		AllowedModels: []string{"a-model-this-key-must-not-reach"},
		BudgetMicros:  new(int64(999_000_000)),
	}); err != nil {
		t.Fatalf("PutPolicy: %v", err)
	}
	// Written past the control plane, which is the case this defends against.
	if _, err := st.pool.Exec(ctx,
		"UPDATE api_keys SET team_id = 'team_elsewhere' WHERE id = $1", f.keyID); err != nil {
		t.Fatalf("planting the cross-tenant row: %v", err)
	}

	res, err := st.LookupKey(ctx, f.hash)
	if err != nil {
		t.Fatalf("LookupKey: %v", err)
	}
	if res.Key.TeamID != "" {
		t.Errorf("TeamID = %q, want empty: the team is outside the key's org", res.Key.TeamID)
	}
	if len(res.Scopes) != 2 {
		t.Errorf("%d scopes, want 2 - org and key, with no team between them", len(res.Scopes))
	}
	if res.AllowedModels != nil {
		t.Errorf("the other tenant's allow-list was inherited: %v", res.AllowedModels)
	}
	for _, sc := range res.Scopes {
		if sc.BudgetMicros == 999_000_000 {
			t.Errorf("the other tenant's budget was inherited by scope %+v", sc)
		}
	}
}
