package store

import (
	"slices"
	"testing"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// A new model joins the organisation's allow-list only when there is one, and
// only once.
func TestAllowInOrgAddsToAnExistingListOnly(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)

	// No guardrail, and then one without a list: every model is allowed
	// already, and writing a list would narrow that to one.
	for _, lim := range []*policy.Limits{nil, {}} {
		if lim != nil {
			if err := st.PutGuardrail(ctx, policy.ScopeOrg, f.orgID, *lim); err != nil {
				t.Fatalf("PutGuardrail: %v", err)
			}
		}
		changed, err := st.AllowInOrg(ctx, f.orgID, "keera-code")
		if err != nil || changed {
			t.Fatalf("AllowInOrg with no list = %v, %v; want false, nil", changed, err)
		}
	}
	got, err := st.GetGuardrail(ctx, policy.ScopeOrg, f.orgID)
	if err != nil {
		t.Fatalf("GetGuardrail: %v", err)
	}
	if got.AllowedModels != nil {
		t.Fatalf("allowed models = %v, want no list", got.AllowedModels)
	}

	if err := st.PutGuardrail(ctx, policy.ScopeOrg, f.orgID,
		policy.Limits{AllowedModels: []string{"keera-speed"}}); err != nil {
		t.Fatalf("PutGuardrail: %v", err)
	}
	for i, want := range []bool{true, false} {
		changed, err := st.AllowInOrg(ctx, f.orgID, "keera-code")
		if err != nil || changed != want {
			t.Fatalf("AllowInOrg #%d = %v, %v; want %v", i+1, changed, err, want)
		}
	}
	got, err = st.GetGuardrail(ctx, policy.ScopeOrg, f.orgID)
	if err != nil {
		t.Fatalf("GetGuardrail: %v", err)
	}
	if !slices.Equal(got.AllowedModels, []string{"keera-speed", "keera-code"}) {
		t.Errorf("allowed models = %v, want keera-speed and keera-code", got.AllowedModels)
	}
}
