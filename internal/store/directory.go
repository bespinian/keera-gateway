package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Directory is how recently a person's identity provider vouched for them.
// Session and token lookups carry it, so deciding whether to ask again costs
// no extra query.
type Directory struct {
	// Linked means a refresh token is stored, so the directory can be asked.
	Linked    bool
	CheckedAt time.Time
}

// Due reports whether the directory should be asked again.
func (d Directory) Due(now time.Time, every time.Duration) bool {
	return d.Linked && now.Sub(d.CheckedAt) >= every
}

// directoryColumns reads Directory from a query that joins users as u.
const directoryColumns = `u.refresh_token IS NOT NULL,
	COALESCE(u.directory_checked_at, 'epoch'::timestamptz)`

// RecordDirectoryCheck notes that the directory just vouched for someone and
// keeps the refresh token it returned, sealed. Nil keeps the stored one:
// Google hands one out only the first time a person consents.
func (s *Store) RecordDirectoryCheck(ctx context.Context, userID string, sealed []byte) error {
	return s.execOne(ctx, `UPDATE users
		SET refresh_token = COALESCE($2, refresh_token), directory_checked_at = now()
		WHERE id = $1`, userID, sealed)
}

// ClaimDirectoryCheck takes the next check of someone's directory, if one is
// due, and returns the sealed refresh token to make it with. Only one caller
// wins: a provider that rotates refresh tokens refuses the second use of one,
// and that refusal would sign the person out.
func (s *Store) ClaimDirectoryCheck(ctx context.Context, userID string,
	checkedBefore time.Time) ([]byte, bool, error) {
	var sealed []byte
	err := s.pool.QueryRow(ctx, `UPDATE users SET directory_checked_at = now()
		WHERE id = $1 AND refresh_token IS NOT NULL
		AND COALESCE(directory_checked_at, 'epoch'::timestamptz) < $2
		RETURNING refresh_token`, userID, checkedBefore).Scan(&sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return sealed, err == nil, err
}

// EndDirectoryLink drops someone's refresh token and every sign-in they hold,
// for when their directory no longer vouches for them.
func (s *Store) EndDirectoryLink(ctx context.Context, userID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "UPDATE users SET refresh_token = NULL WHERE id = $1", userID); err != nil {
		return err
	}
	if err := deleteUserSessions(ctx, tx, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// HasRefreshToken reports whether the directory can be asked about someone
// again without them signing in.
func (s *Store) HasRefreshToken(ctx context.Context, userID string) (bool, error) {
	var linked bool
	err := s.pool.QueryRow(ctx, "SELECT refresh_token IS NOT NULL FROM users WHERE id = $1",
		userID).Scan(&linked)
	return linked, notFound(err)
}
