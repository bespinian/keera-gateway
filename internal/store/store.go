// Package store is Keera Gateway's Postgres layer: schema migration, the
// control-plane queries, and the batched writes the gateway produces.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"slices"
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
// watching the request log sees new requests quickly.
//
// It is separate from NotifyChannel because it fires several times a second,
// and a policy notification makes every replica drop its whole cache.
const EventsChannel = "keera_events"

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("store: not found")

// Store owns the connection pool.
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

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Pool exposes the underlying pool for the LISTEN connection.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Ping reports whether the database is reachable.
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

// Notify tells every replica to drop its cached policy.
func (s *Store) Notify(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, "SELECT pg_notify($1, '')", NotifyChannel)
	return err
}

// Listen holds a dedicated connection on a notify channel and calls fn for
// every notification. It returns the error that ended the subscription.
//
// It tries once and does not reconnect, because each caller handles a lost
// connection its own way. The connection is held outside the pool for as long
// as the subscription lasts, so callers should be few and long-lived.
func (s *Store) Listen(ctx context.Context, channel string, fn func()) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	// A channel name cannot be a placeholder, so it must only ever come from
	// this binary's own constants.
	if _, err := conn.Exec(ctx, "LISTEN "+channel); err != nil {
		return err
	}
	for {
		if _, err := conn.Conn().WaitForNotification(ctx); err != nil {
			return err
		}
		fn()
	}
}

// row is what pgx's Row and Rows have in common: one Scan.
type row interface{ Scan(dest ...any) error }

// collect reads every row with scan. No rows gives an empty slice rather than
// nil, so a list encodes as [] in JSON.
func collect[T any](rows pgx.Rows, scan func(row) (T, error)) ([]T, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (T, error) { return scan(r) })
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

// isUnique reports whether err is a unique violation.
func isUnique(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
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
