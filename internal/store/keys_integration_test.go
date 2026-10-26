package store

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

func TestLookupKeyCollapsesTheWholeChain(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)

	// Each level narrows what it inherits, and each says something of its own.
	if err := st.PutGuardrail(ctx, policy.ScopeOrg, f.orgID, policy.Limits{
		AllowedModels: []string{"keera-code", "keera-speed", "keera-frontier"},
		RateLimits:    []policy.RateLimit{{Requests: 600, Tokens: 400000, Period: policy.PeriodMinute}},
		Budgets:       []policy.Budget{{BudgetMicros: 10_000_000_000, Period: policy.PeriodMonth}},
		SystemPrompt:  new("Never include customer data in an example."),
	}); err != nil {
		t.Fatalf("PutGuardrail org: %v", err)
	}
	if err := st.PutGuardrail(ctx, policy.ScopeProject, f.projectID, policy.Limits{
		AllowedModels: []string{"keera-code", "keera-speed"},
		RateLimits:    []policy.RateLimit{{Requests: 120, Period: policy.PeriodMinute}},
		Budgets:       []policy.Budget{{BudgetMicros: 500_000_000, Period: policy.PeriodDay}},
		SystemPrompt:  new("Prefer the payments project's own libraries."),
	}); err != nil {
		t.Fatalf("PutGuardrail project: %v", err)
	}
	if err := st.PutGuardrail(ctx, policy.ScopeKey, f.keyID, policy.Limits{
		AllowedModels: []string{"keera-speed", "keera-frontier"},
		RateLimits:    []policy.RateLimit{{Requests: 30, Period: policy.PeriodMinute}},
	}); err != nil {
		t.Fatalf("PutGuardrail key: %v", err)
	}

	res, err := st.LookupKey(ctx, f.hash)
	if err != nil {
		t.Fatalf("LookupKey: %v", err)
	}

	if res.Key.OrgID != f.orgID || res.Key.ProjectID != f.projectID || res.Key.ID != f.keyID {
		t.Errorf("resolved key = %+v, want the fixture's ids", res.Key)
	}
	// The intersection of all three allow-lists: keera-code is dropped by the
	// key, keera-frontier by the project.
	if len(res.AllowedModels) != 1 || res.AllowedModels[0] != "keera-speed" {
		t.Errorf("AllowedModels = %v, want [keera-speed]", res.AllowedModels)
	}
	// Prompts add up rather than narrow.
	const wantPrompt = "Never include customer data in an example.\n\n" +
		"Prefer the payments project's own libraries."
	if res.SystemPrompt != wantPrompt {
		t.Errorf("SystemPrompt = %q, want %q", res.SystemPrompt, wantPrompt)
	}

	// Budgets are kept per level, not merged. The project's daily budget is a
	// scope of its own, beside its monthly one.
	want := []policy.Scope{
		{
			Type: policy.ScopeOrg, ID: f.orgID, RPM: 600, TPM: 400000,
			BudgetMicros: 10_000_000_000, Period: policy.PeriodMonth,
		},
		{Type: policy.ScopeProject, ID: f.projectID, RPM: 120, Period: policy.PeriodMonth},
		{
			Type: policy.ScopeProject, ID: f.projectID,
			BudgetMicros: 500_000_000, Period: policy.PeriodDay,
		},
		{Type: policy.ScopeKey, ID: f.keyID, RPM: 30, Period: policy.PeriodMonth},
	}
	if len(res.Scopes) != len(want) {
		t.Fatalf("Scopes = %+v, want %+v", res.Scopes, want)
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
	// Unrestricted, but still three scopes: the gateway counts requests at
	// every level.
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
		ID: "key_expired", OrgID: f.orgID, Name: "expired", Prefix: "keera_sk_exp",
		ExpiresAt: &past,
	}, []byte("hash-expired-0000000000000000000")); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if _, err := st.LookupKey(ctx, []byte("hash-expired-0000000000000000000")); err != policy.ErrKeyExpired {
		t.Errorf("an expired key gave %v, want ErrKeyExpired", err)
	}

	if err := st.RevokeKey(ctx, AnyOrg, f.keyID); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	if _, err := st.LookupKey(ctx, f.hash); err != policy.ErrKeyRevoked {
		t.Errorf("a revoked key gave %v, want ErrKeyRevoked", err)
	}
	// Revoking twice is not a second event.
	if err := st.RevokeKey(ctx, AnyOrg, f.keyID); !errors.Is(err, policy.ErrKeyRevoked) {
		t.Errorf("revoking an already revoked key gave %v, want ErrKeyRevoked", err)
	}
	if err := st.RevokeKey(ctx, AnyOrg, "key_no_such_thing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoking a key that does not exist gave %v, want ErrNotFound", err)
	}
	// A revoked key is kept, so its usage history still resolves.
	keys, err := st.KeySummaries(ctx, KeyQuery{OrgID: f.orgID})
	if err != nil {
		t.Fatalf("KeySummaries: %v", err)
	}
	if len(keys) != 2 {
		t.Errorf("%d keys after a revocation, want both still listed", len(keys))
	}
}

func TestLookupKeyIgnoresAProjectInAnotherOrganisation(t *testing.T) {
	// A key bound to another organisation's project, written past the control
	// plane, resolves with no project, not the other tenant's guardrails.
	st, ctx := db(t)
	f := newFixture(t, st, ctx)

	if _, err := st.CreateOrg(ctx, Org{ID: "org_2", Name: "Another Bank"}, OrgTemplate{}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := st.CreateProject(ctx, Project{ID: "project_elsewhere", OrgID: "org_2", Name: "Somebody Else"}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := st.PutGuardrail(ctx, policy.ScopeProject, "project_elsewhere", policy.Limits{
		AllowedModels: []string{"a-model-this-key-must-not-reach"},
		Budgets:       []policy.Budget{{BudgetMicros: 999_000_000}},
	}); err != nil {
		t.Fatalf("PutGuardrail: %v", err)
	}
	// Written past the control plane.
	if _, err := st.pool.Exec(ctx,
		"UPDATE api_keys SET project_id = 'project_elsewhere' WHERE id = $1", f.keyID); err != nil {
		t.Fatalf("planting the cross-tenant row: %v", err)
	}

	res, err := st.LookupKey(ctx, f.hash)
	if err != nil {
		t.Fatalf("LookupKey: %v", err)
	}
	if res.Key.ProjectID != "" {
		t.Errorf("ProjectID = %q, want empty: the project is outside the key's org", res.Key.ProjectID)
	}
	if len(res.Scopes) != 2 {
		t.Errorf("%d scopes, want 2 - org and key, with no project between them", len(res.Scopes))
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

func TestLookupKeyByIDMatchesTheHashLookup(t *testing.T) {
	// The playground finds a key by its id, with the same guardrails as its
	// requests.
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	if err := st.PutGuardrail(ctx, policy.ScopeProject, f.projectID, policy.Limits{
		AllowedModels: []string{"keera-code"},
		RateLimits:    []policy.RateLimit{{Requests: 120}},
	}); err != nil {
		t.Fatalf("PutGuardrail project: %v", err)
	}

	byHash, err := st.LookupKey(ctx, f.hash)
	if err != nil {
		t.Fatalf("LookupKey: %v", err)
	}
	byID, err := st.LookupKeyByID(ctx, f.keyID)
	if err != nil {
		t.Fatalf("LookupKeyByID: %v", err)
	}
	if byID.Key != byHash.Key || len(byID.Scopes) != len(byHash.Scopes) ||
		!slices.Equal(byID.AllowedModels, byHash.AllowedModels) {
		t.Errorf("by id = %+v, by hash = %+v; want the same key and chain", byID, byHash)
	}

	if _, err := st.LookupKeyByID(ctx, "key_nobody"); !errors.Is(err, policy.ErrUnknownKey) {
		t.Errorf("unknown id: err = %v, want ErrUnknownKey", err)
	}
	if err := st.RevokeKey(ctx, AnyOrg, f.keyID); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	if _, err := st.LookupKeyByID(ctx, f.keyID); !errors.Is(err, policy.ErrKeyRevoked) {
		t.Errorf("revoked key: err = %v, want ErrKeyRevoked", err)
	}
}

// A pending key has no secret until it is activated, and it is activated once.
func TestActivateKeyGivesAPendingKeyItsSecretOnce(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)

	created, err := st.CreateKey(ctx, KeyInfo{
		ID: "key_pending", OrgID: f.orgID, Name: "Ada's laptop",
	}, nil)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if !created.Pending || created.State(time.Now()) != "pending" {
		t.Errorf("created %+v, want a pending key", created)
	}
	if _, err := st.LookupKeyByID(ctx, "key_pending"); !errors.Is(err, policy.ErrKeyPending) {
		t.Errorf("pending key by id: err = %v, want ErrKeyPending", err)
	}
	if _, err := st.RotateKey(ctx, AnyOrg, "key_pending", KeyInfo{ID: "key_rotated"},
		[]byte("hash-rotated-0000000000000000000")); !errors.Is(err, policy.ErrKeyPending) {
		t.Errorf("rotating a pending key: err = %v, want ErrKeyPending", err)
	}

	hash := []byte("hash-activated-00000000000000000")
	active, err := st.ActivateKey(ctx, AnyOrg, "key_pending", "claude-code on laptop", hash, "keera_sk_act")
	if err != nil {
		t.Fatalf("ActivateKey: %v", err)
	}
	if active.Pending || active.Prefix != "keera_sk_act" || active.Name != "claude-code on laptop" ||
		active.ProjectID == "" {
		t.Errorf("activated %+v, want the new prefix and name, in a project", active)
	}
	if res, err := st.LookupKey(ctx, hash); err != nil || res.Key.ID != "key_pending" {
		t.Errorf("LookupKey after activation: %v, %v", res, err)
	}
	if _, err := st.ActivateKey(ctx, AnyOrg, "key_pending", "", []byte("hash-again-000000000000000000000"),
		"keera_sk_again"); !errors.Is(err, ErrKeyActive) {
		t.Errorf("activating twice: err = %v, want ErrKeyActive", err)
	}

	if _, err := st.CreateKey(ctx, KeyInfo{ID: "key_gone", OrgID: f.orgID, Name: "gone"}, nil); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if err := st.RevokeKey(ctx, AnyOrg, "key_gone"); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	if _, err := st.ActivateKey(ctx, AnyOrg, "key_gone", "", []byte("hash-gone-0000000000000000000000"),
		"keera_sk_gone"); !errors.Is(err, policy.ErrKeyRevoked) {
		t.Errorf("activating a revoked key: err = %v, want ErrKeyRevoked", err)
	}
	past := time.Now().Add(-time.Hour)
	if _, err := st.CreateKey(ctx, KeyInfo{
		ID: "key_lapsed", OrgID: f.orgID, Name: "lapsed",
		ExpiresAt: &past,
	}, nil); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if _, err := st.ActivateKey(ctx, AnyOrg, "key_lapsed", "", []byte("hash-lapsed-00000000000000000000"),
		"keera_sk_lapsed"); !errors.Is(err, policy.ErrKeyExpired) {
		t.Errorf("activating an expired key: err = %v, want ErrKeyExpired", err)
	}
	if _, err := st.ActivateKey(ctx, AnyOrg, "key_nobody", "", []byte("hash-nobody-00000000000000000000"),
		"keera_sk_nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("activating an unknown key: err = %v, want ErrNotFound", err)
	}
}
