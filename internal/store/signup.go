package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Signup is a sign-in that matched no organisation, waiting for the person to
// create one or to accept an invitation. See docs/sso.md.
type Signup struct {
	TokenHash []byte
	// UserID is the id the account gets. The refresh token is sealed
	// against it.
	UserID     string
	Provider   string
	ExternalID string
	Email      string
	// Role is what the identity provider's answer maps to.
	Role string
	// RefreshToken is sealed, or nil when the provider issued none.
	RefreshToken []byte
	// InvitedOrg is the organisation that added this address, or empty.
	// InvitedOrgName is its name, read with it.
	InvitedOrg     string
	InvitedOrgName string
	RedirectTo     string
}

const signupColumns = `token_hash, user_id, provider, external_id, email, role, refresh_token,
	COALESCE(invited_org, ''), redirect_to`

func scanSignup(r row) (Signup, error) {
	var su Signup
	err := r.Scan(&su.TokenHash, &su.UserID, &su.Provider, &su.ExternalID, &su.Email,
		&su.Role, &su.RefreshToken, &su.InvitedOrg, &su.RedirectTo)
	return su, notFound(err)
}

// CreateSignup records a sign-up until expires.
func (s *Store) CreateSignup(ctx context.Context, su Signup, expires time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO signups (token_hash, user_id, provider,
		external_id, email, role, refresh_token, invited_org, redirect_to, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		su.TokenHash, su.UserID, su.Provider, su.ExternalID, su.Email, su.Role,
		su.RefreshToken, nullable(su.InvitedOrg), su.RedirectTo, expires)
	return err
}

// SignupByToken reads a sign-up without using it up, with the name of the
// organisation that invited the person.
func (s *Store) SignupByToken(ctx context.Context, hash []byte) (Signup, error) {
	var name *string
	var su Signup
	err := s.pool.QueryRow(ctx, `SELECT `+signupColumns+`, o.name
		FROM signups LEFT JOIN orgs o ON o.id = signups.invited_org
		WHERE token_hash = $1 AND expires_at > now()`, hash,
	).Scan(&su.TokenHash, &su.UserID, &su.Provider, &su.ExternalID, &su.Email,
		&su.Role, &su.RefreshToken, &su.InvitedOrg, &su.RedirectTo, &name)
	if name != nil {
		su.InvitedOrgName = *name
	}
	return su, notFound(err)
}

// InvitationFor finds the organisation that added a person by address before
// they ever signed in: the oldest row at that address that no identity has
// taken yet. A disabled row is no invitation.
func (s *Store) InvitationFor(ctx context.Context, email string) (Org, error) {
	return s.orgWhere(ctx, `WHERE id = (SELECT org_id FROM users
		WHERE email = $1 AND (external_id IS NULL OR external_id = '') AND disabled_at IS NULL
		ORDER BY created_at, id LIMIT 1)`, email)
}

// takeSignup uses a sign-up up inside tx, so it is gone only if what it was
// for happens too.
func takeSignup(ctx context.Context, tx pgx.Tx, hash []byte) (Signup, error) {
	return scanSignup(tx.QueryRow(ctx, `DELETE FROM signups
		WHERE token_hash = $1 AND expires_at > now() RETURNING `+signupColumns, hash))
}

// SignUp uses a sign-up to create an organisation, with the person as its
// administrator, or as an operator if they are one. It is one transaction: a
// name already taken leaves the sign-up for another try, and two tabs cannot
// each create one.
func (s *Store) SignUp(ctx context.Context, hash []byte, o Org, tmpl OrgTemplate) (
	Signup, Org, User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Signup{}, o, User{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	su, err := takeSignup(ctx, tx, hash)
	if err != nil {
		return su, o, User{}, err
	}
	if o, err = createOrg(ctx, tx, o, tmpl); err != nil {
		return su, o, User{}, err
	}
	role := "admin"
	if su.Role == "operator" {
		role = su.Role
	}
	u, err := scanUser(tx.QueryRow(ctx, `INSERT INTO users (id, org_id, email, external_id, role)
		VALUES ($1,$2,$3,$4,$5) RETURNING `+userColumns,
		su.UserID, o.ID, su.Email, su.ExternalID, role))
	if isUnique(err) {
		return su, o, User{}, ErrEmailTaken
	}
	if err != nil {
		return su, o, User{}, err
	}
	return su, o, u, tx.Commit(ctx)
}

// JoinInvitation uses a sign-up to take the row the inviting organisation
// made for the person. The role is the one that row was given.
func (s *Store) JoinInvitation(ctx context.Context, hash []byte) (Signup, User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Signup{}, User{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	su, err := takeSignup(ctx, tx, hash)
	if err != nil {
		return su, User{}, err
	}
	if su.InvitedOrg == "" {
		return su, User{}, ErrNotFound
	}
	u, err := scanUser(tx.QueryRow(ctx, `UPDATE users SET external_id = $3
		WHERE org_id = $1 AND email = $2 AND (external_id IS NULL OR external_id = '')
			AND disabled_at IS NULL
		RETURNING `+userColumns, su.InvitedOrg, su.Email, su.ExternalID))
	if errors.Is(err, pgx.ErrNoRows) {
		// The invitation was withdrawn, or somebody else took it meanwhile.
		return su, User{}, ErrNotFound
	}
	if isUnique(err) {
		return su, User{}, ErrEmailTaken
	}
	if err != nil {
		return su, User{}, err
	}
	return su, u, tx.Commit(ctx)
}
