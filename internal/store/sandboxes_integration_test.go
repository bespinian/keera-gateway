package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// The sandbox tables against a real Postgres. What is covered here is the half
// no fake can be: the partial unique index that makes a name reusable, the
// accumulating clock, and the cascades a deleted tenant leaves behind.

func newSandboxClass(t *testing.T, st *Store, ctx context.Context, orgID, name string) policy.SandboxClass {
	t.Helper()
	c := policy.SandboxClass{
		OrgID: orgID, Name: name, Image: "example/sandbox:1", Isolation: policy.IsolationIsolated,
		CPU: 4000, Memory: 16384, Disk: 51200,
		DefaultTTL: 4 * time.Hour, MaxTTL: 24 * time.Hour,
		Purposes: []policy.Purpose{policy.PurposeEngineer},
	}
	if err := st.UpsertSandboxClass(ctx, &c); err != nil {
		t.Fatalf("UpsertSandboxClass: %v", err)
	}
	if c.CreatedAt.IsZero() || c.UpdatedAt.IsZero() {
		t.Error("the upsert should write the timestamps back, so that a PUT can " +
			"answer with what was saved rather than with year one")
	}
	return c
}

func newSandbox(t *testing.T, st *Store, ctx context.Context, f fixture, name string) Sandbox {
	t.Helper()
	expires := time.Now().Add(time.Hour)
	sb, err := st.CreateSandbox(ctx, Sandbox{
		ID: "sbx_" + name, OrgID: f.orgID, ProjectID: f.projectID,
		Owner: "dev@example.ch", Name: name, Class: "standard",
		Purpose: policy.PurposeEngineer, State: policy.SandboxPending,
		Image: "example/sandbox:1", Isolation: policy.IsolationIsolated,
		CPU: 4000, Memory: 16384, Disk: 51200,
		KeyID: f.keyID, ExpiresAt: &expires,
	})
	if err != nil {
		t.Fatalf("CreateSandbox %s: %v", name, err)
	}
	return sb
}

func TestSandboxClassRoundTrip(t *testing.T) {
	st, ctx := db(t)
	anOrg(t, st, ctx, "org_1")
	anOrg(t, st, ctx, "org_2")
	want := newSandboxClass(t, st, ctx, "org_1", "standard")

	got, err := st.SandboxClass(ctx, "org_1", "standard")
	if err != nil {
		t.Fatalf("SandboxClass: %v", err)
	}
	if got.Image != want.Image || got.CPU != want.CPU || got.Disk != want.Disk ||
		got.Isolation != want.Isolation || len(got.Purposes) != 1 {
		t.Errorf("read back %+v, want %+v", got, want)
	}
	// The two durations cross the driver as seconds and have to come back the
	// same duration, which is the one thing a column of integers can get wrong.
	if got.DefaultTTL != 4*time.Hour || got.MaxTTL != 24*time.Hour {
		t.Errorf("lifetimes = %s / %s", got.DefaultTTL, got.MaxTTL)
	}

	// The class is org_1's alone, and org_2 may have its own of the same name.
	if _, err := st.SandboxClass(ctx, "org_2", "standard"); !errors.Is(err, ErrNotFound) {
		t.Errorf("org_2 reads org_1's class: %v", err)
	}
	newSandboxClass(t, st, ctx, "org_2", "standard")
	if list, _ := st.ListSandboxClasses(ctx, "org_1"); len(list) != 1 {
		t.Errorf("org_1 lists %d classes, want its one", len(list))
	}
	if all, _ := st.LoadSandboxClasses(ctx); len(all) != 2 {
		t.Errorf("every organisation's classes are %d, want 2", len(all))
	}
	if err := st.DeleteSandboxClass(ctx, "org_2", "standard"); err != nil {
		t.Fatalf("DeleteSandboxClass: %v", err)
	}
	if _, err := st.SandboxClass(ctx, "org_1", "standard"); err != nil {
		t.Errorf("removing org_2's class removed org_1's: %v", err)
	}
}

func TestSandboxNameIsUniqueOnlyWhileLive(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	newSandboxClass(t, st, ctx, f.orgID, "standard")

	first := newSandbox(t, st, ctx, f, "fix-login")
	named := func(id, owner string) error {
		_, err := st.CreateSandbox(ctx, Sandbox{
			ID: id, OrgID: f.orgID, Owner: owner, Name: "fix-login", Class: "standard",
			Purpose: policy.PurposeEngineer, State: policy.SandboxPending,
		})
		return err
	}

	// A second live one of the same person with the same name is refused:
	// `keera sandbox ssh fix-login` has to mean something.
	if err := named("sbx_second", first.Owner); !errors.Is(err, ErrSandboxNameTaken) {
		t.Fatalf("err = %v, want ErrSandboxNameTaken", err)
	}
	// A colleague may use the name. Refusing it would tell them what the
	// first person called theirs.
	if err := named("sbx_colleague", "other@example.ch"); err != nil {
		t.Fatalf("a colleague using the same name: %v", err)
	}
	found, err := st.LiveSandboxesByName(ctx, f.orgID, "fix-login")
	if err != nil || len(found) != 2 {
		t.Fatalf("LiveSandboxesByName = %d sandboxes, %v; want both", len(found), err)
	}

	// A failed engineer sandbox keeps its volume, so it keeps its name too.
	if err := st.ObserveSandbox(ctx, first.ID, SandboxObservation{
		State: policy.SandboxFailed, Detail: "OOMKilled",
	}); err != nil {
		t.Fatalf("ObserveSandbox: %v", err)
	}
	if err := named("sbx_second", first.Owner); !errors.Is(err, ErrSandboxNameTaken) {
		t.Fatalf("err = %v, want the failed sandbox to keep its name", err)
	}
	if n, err := st.SandboxClassInUse(ctx, f.orgID, "standard"); err != nil || n != 2 {
		t.Errorf("live count = %d, %v; want the failed sandbox counted", n, err)
	}

	// Once the first is gone the name is free again. A developer who finishes
	// with "fix-login" on Tuesday and wants it again on Thursday is asking for
	// something perfectly reasonable.
	if err := st.ObserveSandbox(ctx, first.ID, SandboxObservation{
		State: policy.SandboxTerminated, Detail: "terminated",
	}); err != nil {
		t.Fatalf("ObserveSandbox: %v", err)
	}
	if err := named("sbx_third", first.Owner); err != nil {
		t.Fatalf("reusing the name of a terminated sandbox: %v", err)
	}
}

func TestObserveSandboxTimestamps(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	newSandboxClass(t, st, ctx, f.orgID, "standard")
	sb := newSandbox(t, st, ctx, f, "obs")

	mustObserve := func(state policy.SandboxState, detail string) Sandbox {
		t.Helper()
		if err := st.ObserveSandbox(ctx, sb.ID, SandboxObservation{
			State: state, Detail: detail, Address: "10.1.2.3", Node: "node-a",
		}); err != nil {
			t.Fatalf("ObserveSandbox(%s): %v", state, err)
		}
		got, err := st.Sandbox(ctx, sb.ID)
		if err != nil {
			t.Fatalf("Sandbox: %v", err)
		}
		return got
	}

	ready := mustObserve(policy.SandboxReady, "")
	if ready.ReadyAt == nil {
		t.Fatal("ready_at should be set the first time a sandbox is seen ready")
	}
	firstReady := *ready.ReadyAt

	suspended := mustObserve(policy.SandboxSuspended, "suspended")
	if suspended.SuspendedAt == nil {
		t.Error("suspended_at should be set while suspended")
	}

	// ready_at is when the machine became usable and a resume is not that, so
	// it never moves again. suspended_at does move, because it is "since when
	// has this been idle".
	again := mustObserve(policy.SandboxReady, "")
	if again.ReadyAt == nil || !again.ReadyAt.Equal(firstReady) {
		t.Errorf("ready_at moved on resume: %v, want %v", again.ReadyAt, firstReady)
	}
	if again.SuspendedAt != nil {
		t.Error("suspended_at should be cleared once a sandbox is running again")
	}
	// active_at does move, so idle suspension counts from the resume.
	if again.ActiveAt == nil || again.ActiveAt.Before(firstReady) {
		t.Errorf("active_at = %v on resume, want it at or after %v", again.ActiveAt, firstReady)
	}
	resumedAt := *again.ActiveAt
	if err := st.TouchSandbox(ctx, sb.ID); err != nil {
		t.Fatalf("TouchSandbox: %v", err)
	}
	touched, err := st.Sandbox(ctx, sb.ID)
	if err != nil {
		t.Fatalf("Sandbox: %v", err)
	}
	if touched.ActiveAt == nil || touched.ActiveAt.Before(resumedAt) {
		t.Errorf("active_at = %v after a touch, want it at or after %v", touched.ActiveAt, resumedAt)
	}

	terminated := mustObserve(policy.SandboxTerminated, "terminated")
	if terminated.TerminatedAt == nil {
		t.Error("terminated_at should be set")
	}
	// Nothing happens to a terminated sandbox afterwards: the sweep observing one
	// that somebody removed in between must not resurrect it.
	back := mustObserve(policy.SandboxReady, "")
	if back.State != policy.SandboxTerminated {
		t.Errorf("state = %s; a terminated sandbox should stay terminated", back.State)
	}
}

func TestAccountSandboxesAccumulates(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	newSandboxClass(t, st, ctx, f.orgID, "standard")
	sb := newSandbox(t, st, ctx, f, "clock")

	if err := st.ObserveSandbox(ctx, sb.ID, SandboxObservation{State: policy.SandboxReady}); err != nil {
		t.Fatalf("ObserveSandbox: %v", err)
	}
	// Wind the clock back rather than waiting: what is being tested is that the
	// interval comes from the column, which is also what makes this correct
	// across replicas and across a gateway that was down.
	if _, err := st.pool.Exec(ctx,
		"UPDATE sandboxes SET accounted_at = now() - interval '90 seconds' WHERE id = $1",
		sb.ID); err != nil {
		t.Fatalf("winding the clock back: %v", err)
	}

	if _, err := st.AccountSandboxes(ctx, time.Now()); err != nil {
		t.Fatalf("AccountSandboxes: %v", err)
	}
	got, err := st.Sandbox(ctx, sb.ID)
	if err != nil {
		t.Fatalf("Sandbox: %v", err)
	}
	if got.RunningSeconds < 85 || got.RunningSeconds > 95 {
		t.Errorf("running_seconds = %d, want about 90", got.RunningSeconds)
	}
	// Core-seconds is running time weighted by how large the machine was, which
	// is the number a chargeback wants: two minutes of thirty-two cores is not
	// two minutes of two.
	if got.CoreSeconds() != got.RunningSeconds*4 {
		t.Errorf("core_seconds = %d for a four-core sandbox", got.CoreSeconds())
	}

	// A second pass a moment later adds only that moment, not another ninety
	// seconds: two gateways running the sweep must not double-charge.
	before := got.RunningSeconds
	if _, err := st.AccountSandboxes(ctx, time.Now()); err != nil {
		t.Fatalf("AccountSandboxes: %v", err)
	}
	got, _ = st.Sandbox(ctx, sb.ID)
	if got.RunningSeconds-before > 5 {
		t.Errorf("a second sweep added %d seconds", got.RunningSeconds-before)
	}

	// A suspended sandbox's clock does not run.
	if err := st.StopAccounting(ctx, sb.ID, time.Now()); err != nil {
		t.Fatalf("StopAccounting: %v", err)
	}
	if err := st.ObserveSandbox(ctx, sb.ID, SandboxObservation{
		State: policy.SandboxSuspended,
	}); err != nil {
		t.Fatalf("ObserveSandbox: %v", err)
	}
	frozen, _ := st.Sandbox(ctx, sb.ID)
	if _, err := st.pool.Exec(ctx,
		"UPDATE sandboxes SET accounted_at = now() - interval '60 seconds' WHERE id = $1",
		sb.ID); err != nil {
		t.Fatalf("winding the clock back: %v", err)
	}
	if _, err := st.AccountSandboxes(ctx, time.Now()); err != nil {
		t.Fatalf("AccountSandboxes: %v", err)
	}
	after, _ := st.Sandbox(ctx, sb.ID)
	if after.RunningSeconds != frozen.RunningSeconds {
		t.Errorf("a suspended sandbox was charged %d extra seconds",
			after.RunningSeconds-frozen.RunningSeconds)
	}

	// Nor is the time it was suspended charged once it is resumed.
	if err := st.ObserveSandbox(ctx, sb.ID, SandboxObservation{State: policy.SandboxReady}); err != nil {
		t.Fatalf("ObserveSandbox: %v", err)
	}
	if _, err := st.AccountSandboxes(ctx, time.Now()); err != nil {
		t.Fatalf("AccountSandboxes: %v", err)
	}
	resumed, _ := st.Sandbox(ctx, sb.ID)
	if resumed.RunningSeconds-frozen.RunningSeconds > 5 {
		t.Errorf("resuming charged %d seconds of the suspension",
			resumed.RunningSeconds-frozen.RunningSeconds)
	}
}

func TestCountLiveSandboxes(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	newSandboxClass(t, st, ctx, f.orgID, "standard")
	a := newSandbox(t, st, ctx, f, "a")
	newSandbox(t, st, ctx, f, "b")

	for _, scope := range []struct {
		typ policy.ScopeType
		id  string
	}{{policy.ScopeOrg, f.orgID}, {policy.ScopeProject, f.projectID}} {
		n, err := st.CountLiveSandboxes(ctx, scope.typ, scope.id)
		if err != nil {
			t.Fatalf("CountLiveSandboxes(%s): %v", scope.typ, err)
		}
		if n != 2 {
			t.Errorf("%s holds %d sandboxes, want 2", scope.typ, n)
		}
	}

	// A suspended sandbox still counts: it holds its volume, and a quota that
	// ignored it would be a quota somebody could get round by suspending.
	if err := st.ObserveSandbox(ctx, a.ID, SandboxObservation{
		State: policy.SandboxSuspended,
	}); err != nil {
		t.Fatalf("ObserveSandbox: %v", err)
	}
	if n, _ := st.CountLiveSandboxes(ctx, policy.ScopeOrg, f.orgID); n != 2 {
		t.Errorf("a suspended sandbox stopped counting: %d", n)
	}

	// A terminated one does not.
	if err := st.ObserveSandbox(ctx, a.ID, SandboxObservation{
		State: policy.SandboxTerminated,
	}); err != nil {
		t.Fatalf("ObserveSandbox: %v", err)
	}
	if n, _ := st.CountLiveSandboxes(ctx, policy.ScopeOrg, f.orgID); n != 1 {
		t.Errorf("a terminated sandbox still counts: %d", n)
	}
}

func TestSandboxesPastExpiry(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	newSandboxClass(t, st, ctx, f.orgID, "standard")
	sb := newSandbox(t, st, ctx, f, "expiring")
	newSandbox(t, st, ctx, f, "not-yet")

	if err := st.SetSandboxExpiry(ctx, sb.ID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("SetSandboxExpiry: %v", err)
	}
	due, err := st.SandboxesPastExpiry(ctx, time.Now(), 10)
	if err != nil {
		t.Fatalf("SandboxesPastExpiry: %v", err)
	}
	if len(due) != 1 || due[0].ID != sb.ID {
		t.Fatalf("got %d sandboxes past expiry, want just %s", len(due), sb.ID)
	}
}

func TestSandboxExpiryMovesItsKey(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	newSandboxClass(t, st, ctx, f.orgID, "standard")
	sb := newSandbox(t, st, ctx, f, "extended")

	until := time.Now().Add(8 * time.Hour).UTC().Truncate(time.Second)
	if err := st.SetSandboxExpiry(ctx, sb.ID, until); err != nil {
		t.Fatalf("SetSandboxExpiry: %v", err)
	}
	var got time.Time
	if err := st.pool.QueryRow(ctx,
		"SELECT expires_at FROM api_keys WHERE id = $1", f.keyID).Scan(&got); err != nil {
		t.Fatalf("reading the key: %v", err)
	}
	if !got.Equal(until) {
		t.Errorf("key expires at %v, want %v", got, until)
	}

	if err := st.SetSandboxExpiry(ctx, "sbx_missing", until); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown sandbox: err = %v, want ErrNotFound", err)
	}
}

func TestSandboxPolicyLimitsRoundTrip(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)

	ttl := 8 * 3600
	want := policy.Limits{
		MaxSandboxes:         new(6),
		MaxSandboxTTLSeconds: &ttl,
		SandboxClasses:       []string{"standard", "small"},
		MaxSandboxCPU:        new(8000),
		MaxSandboxMemory:     new(32768)}
	if err := st.PutGuardrail(ctx, policy.ScopeOrg, f.orgID, want); err != nil {
		t.Fatalf("PutGuardrail: %v", err)
	}
	got, err := st.GetGuardrail(ctx, policy.ScopeOrg, f.orgID)
	if err != nil {
		t.Fatalf("GetGuardrail: %v", err)
	}
	if got.MaxSandboxes == nil || *got.MaxSandboxes != 6 {
		t.Errorf("max_sandboxes = %v", got.MaxSandboxes)
	}
	if got.MaxSandboxTTLSeconds == nil || *got.MaxSandboxTTLSeconds != ttl {
		t.Errorf("max_sandbox_ttl_seconds = %v", got.MaxSandboxTTLSeconds)
	}
	if len(got.SandboxClasses) != 2 {
		t.Errorf("sandbox_classes = %v", got.SandboxClasses)
	}

	// And they have to reach the hot path, because that is the only place they
	// are enforced: LookupKey is the one read a sandbox request resolves
	// through.
	resolved, err := st.LookupKey(ctx, f.hash)
	if err != nil {
		t.Fatalf("LookupKey: %v", err)
	}
	if resolved.Sandbox.MaxSandboxes != 6 {
		t.Errorf("resolved MaxSandboxes = %d", resolved.Sandbox.MaxSandboxes)
	}
	if resolved.Sandbox.MaxSandboxCPU != 8000 {
		t.Errorf("resolved MaxSandboxCPU = %d", resolved.Sandbox.MaxSandboxCPU)
	}
}

func TestSandboxUsageBy(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	newSandboxClass(t, st, ctx, f.orgID, "standard")
	a := newSandbox(t, st, ctx, f, "a")
	newSandbox(t, st, ctx, f, "b")

	if _, err := st.pool.Exec(ctx,
		"UPDATE sandboxes SET running_seconds = 600 WHERE id = $1", a.ID); err != nil {
		t.Fatalf("seeding running time: %v", err)
	}

	rows, err := st.SandboxUsageBy(ctx, f.orgID, "class", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SandboxUsageBy: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d groups, want one class", len(rows))
	}
	r := rows[0]
	switch {
	case r.Key != "standard":
		t.Errorf("key = %q", r.Key)
	case r.Count != 2:
		t.Errorf("count = %d", r.Count)
	case r.Running != 600:
		t.Errorf("running = %d", r.Running)
	case r.CoreSeconds != 2400:
		t.Errorf("core_seconds = %d, want 600 seconds of four cores", r.CoreSeconds)
	case r.Live != 2:
		t.Errorf("live = %d", r.Live)
	}

	// Grouping by person reads the address that was copied onto the row rather
	// than joining, so that "whose was this" keeps an answer after somebody is
	// deleted from the directory.
	byUser, err := st.SandboxUsageBy(ctx, f.orgID, "user", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SandboxUsageBy(user): %v", err)
	}
	if len(byUser) != 1 || byUser[0].Key != "dev@example.ch" {
		t.Errorf("by user = %+v", byUser)
	}

	// Across every organisation a class is named with its organisation, since
	// two of them can each have a "standard".
	all, err := st.SandboxUsageBy(ctx, "", "class", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SandboxUsageBy(all orgs): %v", err)
	}
	if len(all) != 1 || all[0].Key != "standard" || all[0].OrgID != f.orgID {
		t.Errorf("across orgs = %+v, want the class with its organisation", all)
	}
}

func TestDeletingAnOrgDeletesItsSandboxes(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	newSandboxClass(t, st, ctx, f.orgID, "standard")
	sb := newSandbox(t, st, ctx, f, "gone")

	// A live sandbox holds a machine, which nothing would stop once its row
	// was gone.
	gone, err := st.DeleteOrg(ctx, f.orgID)
	if !errors.Is(err, ErrOrgHasSandboxes) || gone.LiveSandboxes != 1 {
		t.Fatalf("DeleteOrg with a live sandbox = %+v, %v; want it refused", gone, err)
	}
	if err := st.ObserveSandbox(ctx, sb.ID, SandboxObservation{
		State: policy.SandboxTerminated, Detail: "terminated",
	}); err != nil {
		t.Fatal(err)
	}

	// Sandboxes cascade with their organisation, unlike the usage log. The
	// difference is what each is for: the usage log is what finance invoices
	// from and outlives the tenant, and a sandbox row describes a machine that
	// went away with the tenant that owned it.
	if _, err := st.DeleteOrg(ctx, f.orgID); err != nil {
		t.Fatalf("DeleteOrg: %v", err)
	}
	list, err := st.ListSandboxes(ctx, SandboxQuery{All: true})
	if err != nil {
		t.Fatalf("ListSandboxes: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("got %d sandboxes after the org was deleted", len(list))
	}
}
