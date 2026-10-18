// The store's tests run against a real Postgres, because no fake can cover the
// schema, the queries, and what Postgres itself does: the advisory lock, the
// cascades, the upserts, LISTEN/NOTIFY.
//
// Set KEERA_TEST_DATABASE_URL to a database these may create, truncate and drop
// tables in. Without it every test here skips, so `go test ./...` works
// anywhere; `make test-integration` starts one in a container.
//
// There is no build tag on purpose: tagged files are skipped by `go vet
// ./...`, so they can stop compiling unnoticed.
package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

var (
	testDSN   = os.Getenv("KEERA_TEST_DATABASE_URL")
	testStore *Store
)

func TestMain(m *testing.M) {
	if testDSN != "" {
		testStore = openTestStore()
	}
	code := m.Run()
	if testStore != nil {
		testStore.Close()
	}
	os.Exit(code)
}

// openTestStore connects to KEERA_TEST_DATABASE_URL and migrates it.
func openTestStore() *Store {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := Open(ctx, testDSN, 8)
	if err != nil {
		panic("KEERA_TEST_DATABASE_URL: " + err.Error())
	}
	if _, err := st.Migrate(ctx); err != nil {
		panic("migrate: " + err.Error())
	}
	return st
}

// db returns the store on an empty database. Every test starts from nothing,
// so no test can be made to pass by another one's leftovers.
func db(t *testing.T) (*Store, context.Context) {
	t.Helper()
	if testStore == nil {
		t.Skip("set KEERA_TEST_DATABASE_URL to run the store's integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	if _, err := testStore.pool.Exec(ctx, `TRUNCATE orgs, projects, users, api_keys, guardrails,
		models, filters, filter_runs, routers, usage_events, spend, audit_log, sessions,
		login_flows, cli_codes, cli_tokens, sandboxes, sandbox_classes, mcp_servers, tool_calls,
		billing_lines, credit_accounts, payments, identities, job_runs
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("emptying the database: %v", err)
	}
	return testStore, ctx
}

// fixture is the tenancy every policy test needs: one org, one project in it, and
// one key bound to both.
type fixture struct {
	orgID, projectID, keyID string
	key                     string
	hash                    []byte
}

func newFixture(t *testing.T, st *Store, ctx context.Context) fixture {
	t.Helper()
	f := fixture{orgID: "org_1", projectID: "project_1", keyID: "key_1", key: "keera_sk_test"}
	f.hash = []byte("hash-of-keera_sk_test-32-bytes!!!")
	if _, err := st.CreateOrg(ctx, Org{ID: f.orgID, Name: "Example Bank"}, OrgTemplate{}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := st.CreateProject(ctx, Project{ID: f.projectID, OrgID: f.orgID, Name: "Payments Platform"}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := st.CreateKey(ctx, KeyInfo{
		ID: f.keyID, OrgID: f.orgID, ProjectID: f.projectID,
		Name: "a developer's laptop", Prefix: "keera_sk_test",
	}, f.hash); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	return f
}

func TestMigrateIsIdempotentAndSafeInParallel(t *testing.T) {
	st, ctx := db(t)

	// From nothing: every migration is applied, in order.
	if _, err := st.pool.Exec(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
		t.Fatalf("dropping the schema: %v", err)
	}
	applied, err := st.Migrate(ctx)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(applied) == 0 {
		t.Fatal("Migrate applied nothing to an empty database")
	}
	for i := 1; i < len(applied); i++ {
		if applied[i-1] >= applied[i] {
			t.Errorf("migrations were applied out of order: %v", applied)
		}
	}

	// Again: nothing, because every version is recorded.
	again, err := st.Migrate(ctx)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("the second Migrate applied %v, want nothing", again)
	}

	// Several replicas starting at once is the ordinary case, not an edge one.
	// The advisory lock is what keeps it from being two processes running the
	// same CREATE TABLE.
	if _, err := st.pool.Exec(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
		t.Fatalf("dropping the schema: %v", err)
	}
	type result struct {
		applied []string
		err     error
	}
	const replicas = 4
	results := make(chan result, replicas)
	for range replicas {
		go func() {
			a, err := st.Migrate(ctx)
			results <- result{a, err}
		}()
	}
	total := 0
	for range replicas {
		r := <-results
		if r.err != nil {
			t.Errorf("concurrent Migrate: %v", r.err)
		}
		total += len(r.applied)
	}
	if total != len(applied) {
		t.Errorf("%d migrations were applied across %d replicas, want %d exactly once",
			total, replicas, len(applied))
	}
}

func TestNotifyReachesAListener(t *testing.T) {
	// This is what makes a revoked key stop working within a round trip rather
	// than within a cache lifetime, so it is worth a test that a notification
	// actually arrives.
	st, ctx := db(t)

	pooled, err := st.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// Closed rather than released, so the LISTEN does not follow the
	// connection into later tests.
	conn := pooled.Hijack()
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "LISTEN "+NotifyChannel); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}

	if err := st.Notify(ctx, "key:k1"); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	n, err := conn.WaitForNotification(waitCtx)
	if err != nil {
		t.Fatalf("WaitForNotification: %v", err)
	}
	if n.Channel != NotifyChannel {
		t.Errorf("notification arrived on %q, want %q", n.Channel, NotifyChannel)
	}
	if n.Payload != "key:k1" {
		t.Errorf("payload = %q, want key:k1", n.Payload)
	}
}

func TestListenOutlivesIdleSessionTimeout(t *testing.T) {
	// Managed Postgres often sets idle_session_timeout, and a listener sends
	// nothing while it waits. Without its own override it is cut off every time
	// the timeout runs out.
	base, ctx := db(t)
	sep := "?"
	if strings.Contains(testDSN, "?") {
		sep = "&"
	}
	st, err := Open(ctx, testDSN+sep+"idle_session_timeout=300ms", 2)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	listenCtx, stop := context.WithCancel(ctx)
	defer stop()
	got := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- st.Listen(listenCtx, NotifyChannel, func(p string) { got <- p })
	}()

	time.Sleep(time.Second)
	if err := base.Notify(ctx, "key:k1"); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	select {
	case p := <-got:
		if p != "key:k1" {
			t.Errorf("payload = %q, want key:k1", p)
		}
	case err := <-done:
		t.Fatalf("Listen ended: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("no notification arrived")
	}
}
