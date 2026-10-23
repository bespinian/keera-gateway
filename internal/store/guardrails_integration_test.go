package store

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

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

// The lock holds off a second holder in the same organisation only, and goes
// with unlock.
func TestLockReferencesHoldsOffTheSameOrganisation(t *testing.T) {
	st, ctx := db(t)
	unlock, err := st.LockReferences(ctx, "org_1")
	if err != nil {
		t.Fatalf("LockReferences: %v", err)
	}
	try := func(orgID string) error {
		ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		again, err := st.LockReferences(ctx, orgID)
		if err == nil {
			again()
		}
		return err
	}
	if err := try("org_1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a second lock in org_1 gave %v, want it to wait", err)
	}
	if err := try("org_2"); err != nil {
		t.Errorf("a lock in org_2 gave %v, want it at once", err)
	}
	unlock()
	if err := try("org_1"); err != nil {
		t.Errorf("a lock in org_1 after unlock gave %v", err)
	}
}

// Each holder keeps a connection and needs another for its checks. Holders
// and waiters must never take the whole pool, or none of them finishes.
func TestLockReferencesLeavesConnectionsForItsHolders(t *testing.T) {
	st, ctx := db(t)
	size := int(st.Pool().Config().MaxConns)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 2*size)
	for range 2 * size {
		wg.Go(func() {
			unlock, err := st.LockReferences(ctx, "org_1")
			if err != nil {
				errs <- err
				return
			}
			defer unlock()
			// The check a holder makes while it has the lock.
			var one int
			errs <- st.Pool().QueryRow(ctx, "SELECT 1").Scan(&one)
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a holder could not finish: %v", err)
		}
	}
}
