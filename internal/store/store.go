// Package store is Keera Gateway's Postgres layer: schema migration, the
// control-plane queries, and the batched writes the gateway produces.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bespinian/keera-gateway/internal/policy"
)

//go:embed migrations/*.sql
var migrations embed.FS

// NotifyChannel carries control-plane changes to every gateway replica, so a
// revoked key stops working within one round trip rather than one cache TTL.
const NotifyChannel = "keera_policy"

// EventsChannel says that a batch of usage events was written, so a panel
// watching the request log sees new requests quickly. The payload names the
// organisations the batch wrote requests for; EventOrgs reads it.
//
// It is separate from NotifyChannel because it fires several times a second,
// and a policy notification makes every replica drop its whole cache.
const EventsChannel = "keera_events"

// eventsPayloadMax keeps the payload well under the 8000 bytes Postgres
// allows. A batch for more organisations than fit sends none, which wakes
// every stream.
const eventsPayloadMax = 4000

// eventsPayload names the organisations events wrote requests for. ok is
// false when they wrote none, so there is nothing to announce.
func eventsPayload(events []Event) (payload string, ok bool) {
	var b strings.Builder
	seen := make(map[string]bool)
	for _, e := range events {
		if e.BillsOnly || seen[e.OrgID] {
			continue
		}
		seen[e.OrgID] = true
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(e.OrgID)
	}
	if b.Len() > eventsPayloadMax {
		return "", true
	}
	return b.String(), len(seen) > 0
}

// EventOrgs reads an EventsChannel payload. all says every organisation may
// have new requests, for a payload that names none.
func EventOrgs(payload string) (orgs []string, all bool) {
	if payload == "" {
		return nil, true
	}
	return strings.Split(payload, ","), false
}

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("store: not found")

type Store struct {
	pool *pgxpool.Pool
}

// Open connects to Postgres and verifies the connection.
func Open(ctx context.Context, dsn string, maxConns int32) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	// Report queries filter with "$1 = '' OR org_id = $1". A cached generic
	// plan cannot use the org index for that, and scans every tenant's rows.
	cfg.ConnConfig.RuntimeParams["plan_cache_mode"] = "force_custom_plan"
	// Days are UTC days, as for budgets and usage_daily, whatever the
	// database's own time zone is.
	cfg.ConnConfig.RuntimeParams["timezone"] = "UTC"
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 10 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

// Pool exposes the underlying pool, for tests that set up rows directly.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Migrate applies any embedded migration the database has not seen. It takes an
// advisory lock first, so starting several replicas at once is safe.
func (s *Store) Migrate(ctx context.Context) (applied []string, err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()

	const lockID = 0x60A_0001 // arbitrary, but stable across versions
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lockID); err != nil {
		return nil, fmt.Errorf("advisory lock: %w", err)
	}
	defer func() {
		_, uerr := conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", lockID)
		if err == nil && uerr != nil {
			err = fmt.Errorf("advisory unlock: %w", uerr)
		}
	}()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}

	names, err := migrationNames()
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		var seen bool
		if err := conn.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", name,
		).Scan(&seen); err != nil {
			return nil, err
		}
		if seen {
			continue
		}
		if err := applyMigration(ctx, conn, name); err != nil {
			return applied, err
		}
		applied = append(applied, name)
	}
	return applied, nil
}

// migrationNames lists the embedded migrations in the order they apply.
func migrationNames() ([]string, error) {
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names, nil
}

// applyMigration runs one migration and records it, in one transaction.
func applyMigration(ctx context.Context, conn *pgxpool.Conn, name string) error {
	body, err := migrations.ReadFile("migrations/" + name)
	if err != nil {
		return err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, string(body)); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("migration %s: %w", name, err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", name); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

// Notify tells every replica to drop its cached policy. The payload says what
// changed, as registry.Change spells it.
func (s *Store) Notify(ctx context.Context, payload string) error {
	_, err := s.pool.Exec(ctx, "SELECT pg_notify($1, $2)", NotifyChannel, payload)
	return err
}

// Listen holds a dedicated connection on a notify channel and calls fn with
// the payload of every notification. It returns the error that ended the
// subscription.
//
// It tries once and does not reconnect, because each caller handles a lost
// connection its own way. The connection is its own, outside the pool, so
// callers should be few and long-lived.
func (s *Store) Listen(ctx context.Context, channel string, fn func(payload string)) error {
	pooled, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	// Taken out of the pool and closed at the end, so the setting and the
	// LISTEN below never reach a pooled query.
	conn := pooled.Hijack()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(ctx)
	}()
	// A listener sends nothing while it waits, so a server-side
	// idle_session_timeout would end it every time it ran out.
	if _, err := conn.Exec(ctx, "SET idle_session_timeout = 0"); err != nil {
		return err
	}
	// A channel name cannot be a placeholder, so it must only ever come from
	// this binary's own constants.
	if _, err := conn.Exec(ctx, "LISTEN "+channel); err != nil {
		return err
	}
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		fn(n.Payload)
	}
}

// row is what pgx's Row and Rows have in common: one Scan.
type row interface{ Scan(dest ...any) error }

// queryAll runs a query and reads every row with scan. No rows gives an empty
// slice rather than nil, so a list encodes as [] in JSON.
func queryAll[T any](ctx context.Context, pool *pgxpool.Pool, scan func(row) (T, error),
	sql string, args ...any) ([]T, error) {
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (T, error) { return scan(r) })
}

// queryMap runs a query whose first column is a key and the rest what targets
// points at, and returns its rows by key.
func queryMap[V any](ctx context.Context, pool *pgxpool.Pool, targets func(*V) []any,
	sql string, args ...any) (map[string]V, error) {
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	out := map[string]V{}
	var (
		key string
		v   V
	)
	_, err = pgx.ForEachRow(rows, append([]any{&key}, targets(&v)...), func() error {
		out[key] = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// execOne runs a statement that should touch a row, and returns ErrNotFound if
// it touched none.
func (s *Store) execOne(ctx context.Context, sql string, args ...any) error {
	tag, err := s.pool.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// notFound turns pgx's no-rows into ErrNotFound, which the control plane maps
// to a 404.
func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// IsMissingOrg reports whether a write named an organisation that does not
// exist. Every org_id is a foreign key into orgs that cascades on delete, so
// this is the only way one of them fails.
func IsMissingOrg(err error) bool {
	e, ok := errors.AsType[*pgconn.PgError](err)
	return ok && e.Code == "23503" && strings.HasSuffix(e.ConstraintName, "_org_id_fkey")
}

func isUnique(err error) bool {
	e, ok := errors.AsType[*pgconn.PgError](err)
	return ok && e.Code == "23505"
}

// uniqueOn reports whether err is a unique violation of the named constraint,
// for a table with more than one.
func uniqueOn(err error, constraint string) bool {
	return isUnique(err) && violates(err, constraint)
}

func violates(err error, constraint string) bool {
	e, ok := errors.AsType[*pgconn.PgError](err)
	return ok && e.ConstraintName == constraint
}

// nullable stores an empty string as NULL.
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// nullableTime stores a zero time as NULL, which the queries read as "no
// bound". Otherwise it would be year 1.
func nullableTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// periodPtr and periodStr move a budget period across the driver, which sees
// budget_period as a nullable text column.
func periodPtr(s *string) *policy.Period {
	if s == nil {
		return nil
	}
	p := policy.Period(*s)
	return &p
}

func periodStr(p *policy.Period) *string {
	if p == nil {
		return nil
	}
	s := string(*p)
	return &s
}

// pageLimit is how many rows a list reads: n, def when n is unset, and at
// most max. A cap keeps one request from reading a whole table.
func pageLimit(n, def, max int) int {
	if n <= 0 {
		return def
	}
	return min(n, max)
}

// countOf counts the rows whose col is one of vals. The values are this
// package's own constants, never a caller's string, so they can go into the
// SQL text, quoted.
func countOf[T ~string](col string, vals ...T) string {
	quoted := make([]string, len(vals))
	for i, v := range vals {
		quoted[i] = "'" + string(v) + "'"
	}
	return "count(*) FILTER (WHERE " + col + " IN (" + strings.Join(quoted, ", ") + "))"
}
