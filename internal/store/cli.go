package store

import (
	"context"
	"time"
)

// A command-line sign-in: a one-time code the browser hands to the process
// on somebody's laptop, and the token that code is exchanged for. The schema's
// cli_codes and cli_tokens tables say why neither is a session.

// CLICode is a sign-in that has been authenticated and not yet collected.
type CLICode struct {
	UserID string
	// Challenge is the S256 challenge the command line sent when it started
	// this sign-in. Redeeming the code needs the matching verifier.
	Challenge string
}

// CreateCLICode records the code the browser is about to carry to a loopback
// listener.
func (s *Store) CreateCLICode(ctx context.Context, hash []byte, userID, challenge string,
	expires time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO cli_codes (id, user_id, challenge, expires_at)
		VALUES ($1,$2,$3,$4)`, hash, userID, challenge, expires)
	return err
}

// TakeCLICode consumes a code. Like TakeLoginFlow it deletes and returns in one
// statement, so a code copied from a browser or shell history is worthless.
func (s *Store) TakeCLICode(ctx context.Context, hash []byte) (CLICode, error) {
	var c CLICode
	err := s.pool.QueryRow(ctx, `DELETE FROM cli_codes
		WHERE id = $1 AND expires_at > now()
		RETURNING user_id, challenge`, hash).Scan(&c.UserID, &c.Challenge)
	return c, notFound(err)
}

// CLIToken is one signed-in machine.
type CLIToken struct {
	UserID     string
	Label      string
	ExpiresAt  time.Time
	LastSeenAt time.Time
}

// CLITokenUser is a token joined to the person it belongs to. Like
// SessionUser it is one query, because it runs on every command.
type CLITokenUser struct {
	Token CLIToken
	User  User
}

// CreateCLIToken stores a token under its hash.
func (s *Store) CreateCLIToken(ctx context.Context, hash []byte, userID, label string,
	expires time.Time) error {
	// The label is whatever the machine calls itself, so it is bounded. It is
	// cut on a rune boundary because a text column refuses half a character.
	label = truncate(label, 200)
	_, err := s.pool.Exec(ctx, `INSERT INTO cli_tokens (id, user_id, label, expires_at)
		VALUES ($1,$2,$3,$4)`, hash, userID, label, expires)
	return err
}

// LookupCLIToken resolves a presented token. An expired one, or one of a
// disabled person, is reported as missing, so no caller has to check either.
func (s *Store) LookupCLIToken(ctx context.Context, hash []byte) (CLITokenUser, error) {
	var tu CLITokenUser
	err := s.pool.QueryRow(ctx, `SELECT t.user_id, t.label, t.expires_at, t.last_seen_at,
		u.id, u.org_id, u.email, COALESCE(u.external_id,''), u.role, u.created_at
		FROM cli_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.id = $1 AND t.expires_at > now() AND u.disabled_at IS NULL`, hash,
	).Scan(&tu.Token.UserID, &tu.Token.Label, &tu.Token.ExpiresAt, &tu.Token.LastSeenAt,
		&tu.User.ID, &tu.User.OrgID, &tu.User.Email, &tu.User.ExternalID, &tu.User.Role,
		&tu.User.CreatedAt)
	return tu, notFound(err)
}

// TouchCLIToken records that a token was used, at most once a minute, like
// TouchSession.
func (s *Store) TouchCLIToken(ctx context.Context, hash []byte) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE cli_tokens SET last_seen_at = now()
		 WHERE id = $1 AND last_seen_at < now() - interval '1 minute'`, hash)
	return err
}

// DeleteCLIToken signs one machine out.
func (s *Store) DeleteCLIToken(ctx context.Context, hash []byte) error {
	_, err := s.pool.Exec(ctx, "DELETE FROM cli_tokens WHERE id = $1", hash)
	return err
}
