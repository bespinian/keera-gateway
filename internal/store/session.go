package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Session is one signed-in browser.
type Session struct {
	CSRF      string
	CreatedAt time.Time
}

// CreateSession stores a session under the hash of its cookie value, and
// notes that its person signed in.
func (s *Store) CreateSession(ctx context.Context, hash []byte, userID, csrf string,
	expires time.Time,
) error {
	_, err := s.pool.Exec(ctx, `WITH signed_in AS (
			UPDATE users SET signed_in_at = now() WHERE id = $2)
		INSERT INTO sessions (id, user_id, csrf, expires_at)
		VALUES ($1,$2,$3,$4)`, hash, userID, csrf, expires)
	return err
}

// SessionUser is a session joined to its person, in one query because it runs
// on every browser request.
type SessionUser struct {
	Session   Session
	User      User
	Directory Directory
}

// LookupSession resolves a session cookie. An expired session, or one of a
// disabled person, is reported as missing.
func (s *Store) LookupSession(ctx context.Context, hash []byte) (SessionUser, error) {
	var su SessionUser
	dest := append([]any{&su.Session.CSRF, &su.Session.CreatedAt}, userTargets(&su.User)...)
	err := s.pool.QueryRow(ctx, `SELECT s.csrf, s.created_at,
		`+userColumnsOf("u.")+`, `+directoryColumns+`
		FROM sessions s JOIN users u ON u.id = s.user_id`+identityJoin+`
		WHERE s.id = $1 AND s.expires_at > now() AND u.disabled_at IS NULL`, hash,
	).Scan(append(dest, &su.Directory.Linked, &su.Directory.CheckedAt)...)
	return su, notFound(err)
}

// DeleteSession signs one browser out.
func (s *Store) DeleteSession(ctx context.Context, hash []byte) error {
	_, err := s.pool.Exec(ctx, "DELETE FROM sessions WHERE id = $1", hash)
	return err
}

// DeleteUserSessions signs one user row out everywhere, in its organisation
// only, as after a role change. That includes pending command-line sign-ins,
// whose code could still mint a token.
func (s *Store) DeleteUserSessions(ctx context.Context, userID string) error {
	return deleteUserSessions(ctx, s.pool, userID)
}

func deleteUserSessions(ctx context.Context, db querier, userID string) error {
	return deleteSessionsWhere(ctx, db, "SELECT $1::text", userID)
}

// DeletePersonSessions signs a person out everywhere, in every organisation
// they are in, such as after a lost laptop.
func (s *Store) DeletePersonSessions(ctx context.Context, userID string) error {
	return deleteSessionsWhere(ctx, s.pool, `SELECT id FROM users WHERE id = $1
		OR external_id = (SELECT external_id FROM users WHERE id = $1)`, userID)
}

// deleteSessionsWhere ends the sign-ins of the user rows users selects.
func deleteSessionsWhere(ctx context.Context, db querier, users string, args ...any) error {
	for _, table := range []string{"sessions", "cli_codes", "cli_tokens"} {
		if _, err := db.Exec(ctx, "DELETE FROM "+table+" WHERE user_id IN ("+users+")",
			args...); err != nil {
			return err
		}
	}
	return nil
}

// MoveSession points a browser's session at another of its person's rows,
// which puts it in that row's organisation, and notes the sign-in there.
func (s *Store) MoveSession(ctx context.Context, hash []byte, userID string) error {
	return s.moveSignIn(ctx, "sessions", hash, userID)
}

// MoveCLIToken is MoveSession for a command line's token.
func (s *Store) MoveCLIToken(ctx context.Context, hash []byte, userID string) error {
	return s.moveSignIn(ctx, "cli_tokens", hash, userID)
}

func (s *Store) moveSignIn(ctx context.Context, table string, hash []byte, userID string) error {
	return s.execOne(ctx, `WITH signed_in AS (
			UPDATE users SET signed_in_at = now() WHERE id = $2)
		UPDATE `+table+` SET user_id = $2 WHERE id = $1 AND expires_at > now()`, hash, userID)
}

// PurgeExpired removes expired sessions, sign-in and sign-up flows and passkey
// links. Lookups already check expiry, so this mostly keeps the tables small.
//
// It also removes the refresh token of an identity no enabled row uses, so a
// directory credential does not outlive its accounts.
func (s *Store) PurgeExpired(ctx context.Context) error {
	for _, table := range []string{
		"sessions", "login_flows", "cli_codes", "cli_tokens",
		"passkey_links", "passkey_challenges", "signups",
	} {
		if _, err := s.pool.Exec(ctx,
			"DELETE FROM "+table+" WHERE expires_at < now()"); err != nil {
			return err
		}
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM identities i WHERE NOT EXISTS (
		SELECT 1 FROM users u WHERE u.external_id = i.external_id AND u.disabled_at IS NULL)`)
	return err
}

// LoginFlow is one login in flight.
type LoginFlow struct {
	State    string
	Verifier string
	Nonce    string
	// Provider names the identity provider this login started with, so all
	// providers can share one redirect URI.
	Provider   string
	RedirectTo string
	// CLIRedirect is the loopback address a command line waits on, empty for
	// the panel. CLIChallenge is the S256 challenge whose verifier redeems the
	// code, and CLIState is what the command line checks the callback against.
	CLIRedirect  string
	CLIChallenge string
	CLIState     string
}

// CLI reports whether a command line started this flow.
func (f LoginFlow) CLI() bool { return f.CLIRedirect != "" }

// CreateLoginFlow records the PKCE verifier and nonce for a login about to be
// redirected to the identity provider.
func (s *Store) CreateLoginFlow(ctx context.Context, f LoginFlow, expires time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO login_flows
		(state, verifier, nonce, provider, redirect_to, cli_redirect, cli_challenge,
		 cli_state, expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		f.State, f.Verifier, f.Nonce, f.Provider, f.RedirectTo,
		f.CLIRedirect, f.CLIChallenge, f.CLIState, expires)
	return err
}

const loginFlowColumns = `state, verifier, nonce, provider, redirect_to,
	cli_redirect, cli_challenge, cli_state`

func scanLoginFlow(r row) (LoginFlow, error) {
	var f LoginFlow
	err := r.Scan(&f.State, &f.Verifier, &f.Nonce, &f.Provider, &f.RedirectTo,
		&f.CLIRedirect, &f.CLIChallenge, &f.CLIState)
	return f, notFound(err)
}

// TakeLoginFlow consumes a flow. It deletes and returns in one statement, so a
// replayed callback finds nothing.
func (s *Store) TakeLoginFlow(ctx context.Context, state string) (LoginFlow, error) {
	return scanLoginFlow(s.pool.QueryRow(ctx, `DELETE FROM login_flows
		WHERE state = $1 AND expires_at > now()
		RETURNING `+loginFlowColumns, state))
}

// LoginFlowByState reads a flow without using it up, as a command-line passkey
// sign-in needs, and so a mistyped password does not end a password sign-in.
func (s *Store) LoginFlowByState(ctx context.Context, state string) (LoginFlow, error) {
	return scanLoginFlow(s.pool.QueryRow(ctx, `SELECT `+loginFlowColumns+`
		FROM login_flows WHERE state = $1 AND expires_at > now()`, state))
}

// UserByExternalID finds an identity's most recently used row. A person in
// several organisations has one row in each.
func (s *Store) UserByExternalID(ctx context.Context, externalID string) (User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users
		WHERE external_id = $1
		ORDER BY signed_in_at DESC NULLS LAST, created_at, id LIMIT 1`, externalID))
	return u, notFound(err)
}

// UserInOrg finds an identity's row in one organisation. A disabled row is
// not found.
func (s *Store) UserInOrg(ctx context.Context, externalID, orgID string) (User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users
		WHERE external_id = $1 AND org_id = $2 AND disabled_at IS NULL`, externalID, orgID))
	return u, notFound(err)
}

// Membership is one organisation a person is in, or has been invited to.
type Membership struct {
	UserID  string `json:"user_id"`
	OrgID   string `json:"org_id"`
	OrgName string `json:"org_name"`
	Role    string `json:"role"`
	// Invited means the organisation added the person's address and no
	// identity has taken that row yet.
	Invited  bool `json:"invited,omitempty"`
	Disabled bool `json:"-"`
}

// Memberships lists an identity's organisations, the last used first. With
// invitations, it also lists untaken, enabled rows other organisations made
// for email.
func (s *Store) Memberships(ctx context.Context, externalID, email string,
	invitations bool,
) ([]Membership, error) {
	return queryAll(ctx, s.pool, func(r row) (Membership, error) {
		var m Membership
		err := r.Scan(&m.UserID, &m.OrgID, &m.OrgName, &m.Role, &m.Invited, &m.Disabled)
		return m, err
	}, `SELECT u.id, u.org_id, o.name, u.role,
			COALESCE(u.external_id, '') = '', u.disabled_at IS NOT NULL
		FROM users u JOIN orgs o ON o.id = u.org_id
		WHERE u.external_id = $1
			OR ($2 AND u.email = $3 AND COALESCE(u.external_id, '') = '' AND u.disabled_at IS NULL)
		ORDER BY u.signed_in_at DESC NULLS LAST, o.name, o.id`, externalID, invitations, email)
}

// ExternalIDsFor lists the identities rows at an address are linked to.
func (s *Store) ExternalIDsFor(ctx context.Context, email string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT external_id FROM users
		WHERE email = $1 AND COALESCE(external_id, '') <> ''`, email)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// AcceptInvitation links an identity to the row an organisation made for its
// address, keeping the row's role. ErrNotFound means there is no such row, or
// it was withdrawn or taken meanwhile.
func (s *Store) AcceptInvitation(ctx context.Context, orgID, email, name, externalID string) (User, error) {
	return takeInvitation(ctx, s.pool, orgID, email, name, externalID)
}

func takeInvitation(ctx context.Context, db querier, orgID, email, name, externalID string) (User, error) {
	u, err := scanUser(db.QueryRow(ctx, `UPDATE users SET external_id = $3, name = $4
		WHERE org_id = $1 AND email = $2 AND COALESCE(external_id, '') = ''
			AND disabled_at IS NULL
		RETURNING `+userColumns, orgID, email, externalID, name))
	if isUnique(err) {
		return u, ErrEmailTaken
	}
	return u, notFound(err)
}

// followDirectory sets the address and name of one identity in every
// organisation it is in, as its directory now gives them.
func (s *Store) followDirectory(ctx context.Context, externalID, email, name string) error {
	_, err := s.pool.Exec(ctx, "UPDATE users SET email = $2, name = $3 WHERE external_id = $1",
		externalID, email, name)
	return err
}

// SetIdentityName sets the name of one identity in every organisation it is
// in, as its directory now gives it.
func (s *Store) SetIdentityName(ctx context.Context, externalID, name string) error {
	_, err := s.pool.Exec(ctx, "UPDATE users SET name = $2 WHERE external_id = $1 AND name <> $2",
		externalID, name)
	return err
}

func (s *Store) UserByID(ctx context.Context, userID string) (User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = $1`, userID))
	return u, notFound(err)
}

func (s *Store) SetUserRole(ctx context.Context, orgID, userID, role string) error {
	return s.execOne(ctx, "UPDATE users SET role = $2 WHERE id = $1 AND "+inOrg("org_id", "$3"),
		userID, role, orgID)
}

func (s *Store) orgWhere(ctx context.Context, rest string, args ...any) (Org, error) {
	o, err := scanOrg(s.pool.QueryRow(ctx, orgColumns+" FROM orgs "+rest, args...))
	return o, notFound(err)
}

// OrgByEmailDomain finds the tenant a new sign-in belongs to, so a
// multi-tenant deployment needs no invitation step.
func (s *Store) OrgByEmailDomain(ctx context.Context, domain string) (Org, error) {
	return s.orgWhere(ctx, "WHERE lower(email_domain) = lower($1)", domain)
}

func (s *Store) OrgByID(ctx context.Context, orgID string) (Org, error) {
	return s.orgWhere(ctx, "WHERE id = $1", orgID)
}

// OnlyOrg returns the single organisation, if there is exactly one, as in
// dedicated and on-premises deployments.
func (s *Store) OnlyOrg(ctx context.Context) (Org, error) {
	return s.orgWhere(ctx, "WHERE (SELECT count(*) FROM orgs) = 1")
}

// FirstOrg returns the oldest organisation. The operator key has no
// organisation of its own, so it joins this one.
func (s *Store) FirstOrg(ctx context.Context) (Org, error) {
	return s.orgWhere(ctx, "ORDER BY created_at, id LIMIT 1")
}

// ErrDomainTaken means another organisation already has the domain.
var ErrDomainTaken = errors.New("store: that email domain belongs to another organisation")

// Link identifies the person behind a sign-in.
type Link struct {
	OrgID string
	Email string
	Name  string
	// ExternalID is the provider-qualified subject, as Identity.ExternalID
	// spells it.
	ExternalID string
	Role       string
}

// ErrEmailTaken means a sign-in would take over a person already bound to a
// different subject.
var ErrEmailTaken = errors.New("store: that address already belongs to a different identity")

// LinkUser creates or updates the person behind an identity in one
// organisation, matching on the subject first and then on email.
//
// The email match lets the first sign-in adopt a row made in advance. A row
// that already has another subject is refused, so a reused address or another
// directory's user cannot take over the account and its role.
func (s *Store) LinkUser(ctx context.Context, newID string, l Link) (User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users
		WHERE org_id = $1 AND external_id = $2`, l.OrgID, l.ExternalID))
	if err == nil {
		if !strings.EqualFold(u.Email, l.Email) || u.Name != l.Name {
			if err := s.followDirectory(ctx, l.ExternalID, l.Email, l.Name); err != nil {
				return u, err
			}
			u.Email, u.Name = l.Email, l.Name
		}
		return u, nil
	} else if !errors.Is(notFound(err), ErrNotFound) {
		return User{}, err
	}

	// The guard is in the conflict clause, not a separate read, so two racing
	// sign-ins cannot both pass it.
	u, err = scanUser(s.pool.QueryRow(ctx, `INSERT INTO users (id, org_id, email, name, external_id, role)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (org_id, email) DO UPDATE
		SET external_id = EXCLUDED.external_id, name = EXCLUDED.name
		WHERE users.external_id IS NULL OR users.external_id = ''
		RETURNING `+userColumns,
		newID, l.OrgID, l.Email, l.Name, l.ExternalID, l.Role))
	// When the guard filters out the update, no row comes back.
	if errors.Is(notFound(err), ErrNotFound) {
		return User{}, ErrEmailTaken
	}
	return u, err
}
