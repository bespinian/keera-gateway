package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Directory is how recently a person's identity provider vouched for them.
// Session and token lookups carry it, to save a query.
type Directory struct {
	// Linked means a refresh token is stored, so the directory can be asked.
	Linked    bool
	CheckedAt time.Time
}

// Due reports whether the directory should be asked again.
func (d Directory) Due(now time.Time, every time.Duration) bool {
	return d.Linked && now.Sub(d.CheckedAt) >= every
}

// directoryColumns reads Directory from a query that joins users as u and
// identityJoin.
const directoryColumns = `i.refresh_token IS NOT NULL,
	COALESCE(i.directory_checked_at, 'epoch'::timestamptz)`

// identityJoin brings in what the directory said about the identity of users
// row u. Every row of one person shares it.
const identityJoin = ` LEFT JOIN identities i ON i.external_id = u.external_id`

// RecordDirectoryCheck notes that the directory just vouched for an identity
// and keeps its sealed refresh token. Nil keeps the stored one, since Google
// issues one only at first consent.
func (s *Store) RecordDirectoryCheck(ctx context.Context, externalID string, sealed []byte) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO identities (external_id, refresh_token, directory_checked_at)
		VALUES ($1, $2, now())
		ON CONFLICT (external_id) DO UPDATE
		SET refresh_token = COALESCE(EXCLUDED.refresh_token, identities.refresh_token),
			directory_checked_at = now()`, externalID, sealed)
	return err
}

// ClaimDirectoryCheck takes the next due check of an identity's directory and
// returns the sealed refresh token for it. Only one caller wins, since a
// provider that rotates tokens would refuse a second use and sign the person
// out.
func (s *Store) ClaimDirectoryCheck(ctx context.Context, externalID string,
	checkedBefore time.Time,
) ([]byte, bool, error) {
	var sealed []byte
	err := s.pool.QueryRow(ctx, `UPDATE identities SET directory_checked_at = now()
		WHERE external_id = $1 AND refresh_token IS NOT NULL
		AND COALESCE(directory_checked_at, 'epoch'::timestamptz) < $2
		RETURNING refresh_token`, externalID, checkedBefore).Scan(&sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return sealed, err == nil, err
}

// EndDirectoryLink drops an identity's refresh token and all its sign-ins, in
// every organisation, when its directory no longer vouches for it.
func (s *Store) EndDirectoryLink(ctx context.Context, externalID string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "UPDATE identities SET refresh_token = NULL WHERE external_id = $1",
			externalID); err != nil {
			return err
		}
		return deleteSessionsWhere(ctx, tx, "SELECT id FROM users WHERE external_id = $1",
			externalID)
	})
}

// HasRefreshToken reports whether the directory can be asked about an
// identity again without the person signing in.
func (s *Store) HasRefreshToken(ctx context.Context, externalID string) (bool, error) {
	var linked bool
	err := s.pool.QueryRow(ctx, "SELECT refresh_token IS NOT NULL FROM identities WHERE external_id = $1",
		externalID).Scan(&linked)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return linked, err
}
