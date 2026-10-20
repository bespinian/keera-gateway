package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	keeraid "github.com/bespinian/keera-gateway/internal/id"
)

// Signup is a sign-in that matched no organisation, waiting for the person to
// create one or to accept an invitation. See docs/sso.md.
type Signup struct {
	TokenHash  []byte
	Provider   string
	ExternalID string
	Email      string
	Name       string
	// Role is what the identity provider's answer maps to.
	Role string
	// RefreshToken is sealed against ExternalID, or nil when the provider
	// issued none.
	RefreshToken []byte
	// InvitedOrg is the organisation that added this address, or empty.
	// InvitedOrgName is its name, read with it.
	InvitedOrg     string
	InvitedOrgName string
	RedirectTo     string
}

// signupColumns names signups.name in full: SignupByToken joins orgs, which
// has a name too.
const signupColumns = `token_hash, provider, external_id, email, signups.name, role,
	refresh_token, COALESCE(invited_org, ''), redirect_to`

func signupTargets(su *Signup) []any {
	return []any{&su.TokenHash, &su.Provider, &su.ExternalID, &su.Email,
		&su.Name, &su.Role, &su.RefreshToken, &su.InvitedOrg, &su.RedirectTo}
}

func scanSignup(r row) (Signup, error) {
	var su Signup
	err := r.Scan(signupTargets(&su)...)
	return su, notFound(err)
}

func (s *Store) CreateSignup(ctx context.Context, su Signup, expires time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO signups (token_hash, provider,
		external_id, email, name, role, refresh_token, invited_org, redirect_to, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		su.TokenHash, su.Provider, su.ExternalID, su.Email, su.Name, su.Role,
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
	).Scan(append(signupTargets(&su), &name)...)
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
	var (
		su Signup
		u  User
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		if su, err = takeSignup(ctx, tx, hash); err != nil {
			return err
		}
		if o, err = createOrg(ctx, tx, o, tmpl); err != nil {
			return err
		}
		role := "admin"
		if su.Role == "operator" {
			role = su.Role
		}
		u, err = scanUser(tx.QueryRow(ctx, `INSERT INTO users (id, org_id, email, name, external_id, role)
			VALUES ($1,$2,$3,$4,$5,$6) RETURNING `+userColumns,
			keeraid.New("user"), o.ID, su.Email, su.Name, su.ExternalID, role))
		if isUnique(err) {
			return ErrEmailTaken
		}
		return err
	})
	if err != nil {
		return su, o, User{}, err
	}
	return su, o, u, nil
}

// JoinInvitation uses a sign-up to take the row the inviting organisation
// made for the person. The role is the one that row was given.
func (s *Store) JoinInvitation(ctx context.Context, hash []byte) (Signup, User, error) {
	var (
		su Signup
		u  User
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		if su, err = takeSignup(ctx, tx, hash); err != nil {
			return err
		}
		if su.InvitedOrg == "" {
			return ErrNotFound
		}
		// ErrNotFound: the invitation was withdrawn, or somebody else took it
		// meanwhile.
		u, err = takeInvitation(ctx, tx, su.InvitedOrg, su.Email, su.Name, su.ExternalID)
		return err
	})
	if err != nil {
		return su, User{}, err
	}
	return su, u, nil
}
