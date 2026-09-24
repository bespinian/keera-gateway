package store

import "testing"

func TestAuditIsScopedFilterableAndPagedByACursor(t *testing.T) {
	st, ctx := db(t)
	if _, err := st.CreateOrg(ctx, "org_1", "Example Bank"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := st.CreateOrg(ctx, "org_2", "Another Bank"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	for _, e := range []struct{ actor, org, action string }{
		{"alice@example.ch", "org_1", "policy.set"},
		{"alice@example.ch", "org_1", "key.create"},
		{"bob@example.ch", "org_1", "key.revoke"},
		{"carol@another.example.ch", "org_2", "key.create"},
		// An action that belongs to no tenant: a change to the shared
		// catalogue, which only an operator sees.
		{"operator key", "", "model.put"},
	} {
		if err := st.Audit(ctx, e.actor, e.org, e.action, "thing", "id-1",
			map[string]any{"detail": e.action}); err != nil {
			t.Fatalf("Audit: %v", err)
		}
	}

	// An operator sees every tenant, and the unscoped entries too.
	all, err := st.ListAudit(ctx, AuditQuery{})
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(all) != 5 {
		t.Errorf("%d entries for an operator, want 5", len(all))
	}
	if all[0].Action != "model.put" {
		t.Errorf("first entry = %q, want the newest", all[0].Action)
	}
	if len(all[0].Detail) == 0 {
		t.Error("the detail was not stored")
	}

	// An organisation's administrator sees their own tenant and nothing else -
	// including none of the unscoped entries, which are not their history.
	mine, err := st.ListAudit(ctx, AuditQuery{OrgID: "org_1"})
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(mine) != 3 {
		t.Errorf("%d entries for org_1, want 3: %+v", len(mine), mine)
	}
	for _, e := range mine {
		if e.Actor == "carol@another.example.ch" || e.Action == "model.put" {
			t.Errorf("org_1 can read %+v", e)
		}
	}

	if got, err := st.ListAudit(ctx, AuditQuery{OrgID: "org_1", Actor: "alice@example.ch"}); err != nil {
		t.Fatal(err)
	} else if len(got) != 2 {
		t.Errorf("%d entries by alice, want 2", len(got))
	}
	if got, err := st.ListAudit(ctx, AuditQuery{OrgID: "org_1", Action: "key.create"}); err != nil {
		t.Fatal(err)
	} else if len(got) != 1 {
		t.Errorf("%d key.create entries, want 1", len(got))
	}

	// Ids are monotonic, so paging on one is stable even while the log is being
	// written to - which "offset" would not be.
	page, err := st.ListAudit(ctx, AuditQuery{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 {
		t.Fatalf("%d entries in a page of 2", len(page))
	}
	next, err := st.ListAudit(ctx, AuditQuery{Limit: 2, Before: page[1].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 2 || next[0].ID >= page[1].ID {
		t.Errorf("the cursor did not page backwards: %+v then %+v", page, next)
	}

	// The filters offered are the ones that occur, so none of them matches
	// nothing and none that matters is hidden.
	facets, err := st.AuditFilters(ctx, "org_1")
	if err != nil {
		t.Fatalf("AuditFilters: %v", err)
	}
	if len(facets.Actors) != 2 || len(facets.Actions) != 3 {
		t.Errorf("facets = %+v, want org_1's two actors and three actions", facets)
	}
}
