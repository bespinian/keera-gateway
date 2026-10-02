package store

import (
	"context"
	"errors"
	"time"
)

// Passkey is a WebAuthn credential of one person.
type Passkey struct {
	ID     string `json:"id"`
	UserID string `json:"user_id"`
	Name   string `json:"name"`
	// The credential itself is for the sign-in check only.
	CredentialID []byte     `json:"-"`
	PublicKey    []byte     `json:"-"`
	Algorithm    int        `json:"-"`
	SignCount    uint32     `json:"-"`
	CreatedAt    time.Time  `json:"created_at"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
}

const passkeyColumns = `id, user_id, name, credential_id, public_key, algorithm, sign_count,
	created_at, last_used_at`

func scanPasskey(r row) (Passkey, error) {
	var p Passkey
	var count int64
	err := r.Scan(&p.ID, &p.UserID, &p.Name, &p.CredentialID, &p.PublicKey, &p.Algorithm,
		&count, &p.CreatedAt, &p.LastUsedAt)
	p.SignCount = uint32(count)
	return p, err
}

// ErrPasskeyTaken is returned for a credential id that is already registered.
var ErrPasskeyTaken = errors.New("store: that passkey is already registered")

// AddPasskey stores a new passkey.
func (s *Store) AddPasskey(ctx context.Context, p Passkey) (Passkey, error) {
	out, err := scanPasskey(s.pool.QueryRow(ctx, `INSERT INTO passkeys
		(id, user_id, name, credential_id, public_key, algorithm, sign_count)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING `+passkeyColumns,
		p.ID, p.UserID, p.Name, p.CredentialID, p.PublicKey, p.Algorithm, int64(p.SignCount)))
	if isUnique(err) {
		return Passkey{}, ErrPasskeyTaken
	}
	return out, err
}

// PasskeyByCredentialID finds the passkey a sign-in names.
func (s *Store) PasskeyByCredentialID(ctx context.Context, credentialID []byte) (Passkey, error) {
	p, err := scanPasskey(s.pool.QueryRow(ctx,
		`SELECT `+passkeyColumns+` FROM passkeys WHERE credential_id = $1`, credentialID))
	return p, notFound(err)
}

// PasskeyByID finds one passkey.
func (s *Store) PasskeyByID(ctx context.Context, id string) (Passkey, error) {
	p, err := scanPasskey(s.pool.QueryRow(ctx,
		`SELECT `+passkeyColumns+` FROM passkeys WHERE id = $1`, id))
	return p, notFound(err)
}

// ListPasskeys returns one person's passkeys, oldest first.
func (s *Store) ListPasskeys(ctx context.Context, userID string) ([]Passkey, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+passkeyColumns+`
		FROM passkeys WHERE user_id = $1 ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanPasskey)
}

// UsePasskey records a sign-in with its new signature counter.
//
// The counter check is repeated in the statement, so two sign-ins racing with
// copies of one key cannot both pass. It answers ErrNotFound when it fails.
func (s *Store) UsePasskey(ctx context.Context, id string, signCount uint32) error {
	return s.execOne(ctx, `UPDATE passkeys SET sign_count = $2, last_used_at = now()
		WHERE id = $1 AND ($2 = 0 OR sign_count < $2)`, id, int64(signCount))
}

// ErrLastPasskey is returned for removing someone's only passkey when one
// has to stay.
var ErrLastPasskey = errors.New("store: that is the person's only passkey")

// DeletePasskey removes a passkey. With keepOne it refuses the owner's last
// one. The owner's row is locked first, so two removals at once cannot both
// pass the count.
func (s *Store) DeletePasskey(ctx context.Context, id string, keepOne bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var left int
	err = tx.QueryRow(ctx, `SELECT count(*) FROM passkeys WHERE user_id =
		(SELECT u.id FROM users u JOIN passkeys p ON p.user_id = u.id
		 WHERE p.id = $1 FOR UPDATE OF u)`, id).Scan(&left)
	if err != nil {
		return err
	}
	if left == 0 {
		return ErrNotFound
	}
	if keepOne && left <= 1 {
		return ErrLastPasskey
	}
	if _, err := tx.Exec(ctx, "DELETE FROM passkeys WHERE id = $1", id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ErrDirectoryAccount is returned for a person whose identity comes from a
// directory, or from the operator key.
var ErrDirectoryAccount = errors.New("store: that person signs in through a directory")

// MakePasskeyAccount marks a person as signing in with passkeys. It works on
// a person who is one already, or who has not signed in yet; anyone linked to
// a directory is refused.
func (s *Store) MakePasskeyAccount(ctx context.Context, userID, externalID string) error {
	err := s.execOne(ctx, `UPDATE users SET external_id = $2
		WHERE id = $1 AND (external_id IS NULL OR external_id = '' OR external_id = $2)`,
		userID, externalID)
	if errors.Is(err, ErrNotFound) {
		if _, err := s.UserByID(ctx, userID); err != nil {
			return err
		}
		return ErrDirectoryAccount
	}
	return err
}

// CreatePasskeyLink stores a set-up link. Each person has at most one: a new
// link replaces the last, so only the newest one handed out works.
func (s *Store) CreatePasskeyLink(ctx context.Context, hash []byte, userID string,
	expires time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "DELETE FROM passkey_links WHERE user_id = $1", userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO passkey_links (id, user_id, expires_at)
		VALUES ($1,$2,$3)`, hash, userID, expires); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PasskeyLinkUser reads whose a set-up link is, without using it up, so a
// cancelled browser prompt does not cost the link.
func (s *Store) PasskeyLinkUser(ctx context.Context, hash []byte) (User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users
		WHERE disabled_at IS NULL AND id = (SELECT user_id FROM passkey_links
			WHERE id = $1 AND expires_at > now())`, hash))
	return u, notFound(err)
}

// TakePasskeyLink uses up a set-up link and returns whose it was.
func (s *Store) TakePasskeyLink(ctx context.Context, hash []byte) (string, error) {
	var userID string
	err := s.pool.QueryRow(ctx, `DELETE FROM passkey_links
		WHERE id = $1 AND expires_at > now() RETURNING user_id`, hash).Scan(&userID)
	return userID, notFound(err)
}

// CreatePasskeyChallenge stores the challenge of a registration in flight.
func (s *Store) CreatePasskeyChallenge(ctx context.Context, id, userID, challenge string,
	expires time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO passkey_challenges (id, user_id, challenge, expires_at)
		VALUES ($1,$2,$3,$4)`, id, userID, challenge, expires)
	return err
}

// TakePasskeyChallenge uses up a registration challenge, so a replayed
// registration finds nothing.
func (s *Store) TakePasskeyChallenge(ctx context.Context, id string) (userID, challenge string,
	err error) {
	err = s.pool.QueryRow(ctx, `DELETE FROM passkey_challenges
		WHERE id = $1 AND expires_at > now() RETURNING user_id, challenge`, id,
	).Scan(&userID, &challenge)
	return userID, challenge, notFound(err)
}

// deletePasskeys removes everything that lets a person sign in with a
// passkey, for when they are disabled.
func deletePasskeys(ctx context.Context, db querier, userID string) error {
	for _, table := range []string{"passkeys", "passkey_links", "passkey_challenges"} {
		if _, err := db.Exec(ctx, "DELETE FROM "+table+" WHERE user_id = $1", userID); err != nil {
			return err
		}
	}
	return nil
}
