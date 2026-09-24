package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Session is one signed-in browser.
type Session struct {
	UserID     string
	CSRF       string
	ExpiresAt  time.Time
	LastSeenAt time.Time
}

// CreateSession stores a session under the hash of its cookie value.
func (s *Store) CreateSession(ctx context.Context, hash []byte, userID, csrf string,
	expires time.Time, userAgent, ip string) error {
	// The user agent comes from the client, and is only read by a person
	// checking their own sessions, so it is cut short.
	if len(userAgent) > 400 {
		userAgent = userAgent[:400]
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO sessions
		(id, user_id, csrf, expires_at, user_agent, ip) VALUES ($1,$2,$3,$4,$5,$6)`,
		hash, userID, csrf, expires, userAgent, ip)
	return err
}

// SessionUser is a session joined to the person it belongs to. It is one query
// because it runs on every request the browser makes.
type SessionUser struct {
	Session Session
	User    User
}

// LookupSession resolves a session cookie. An expired session, or one of a
// disabled person, is reported as missing, so no caller has to check either.
func (s *Store) LookupSession(ctx context.Context, hash []byte) (SessionUser, error) {
	var su SessionUser
	err := s.pool.QueryRow(ctx, `SELECT s.user_id, s.csrf, s.expires_at, s.last_seen_at,
		u.id, u.org_id, u.email, COALESCE(u.external_id,''), u.role, u.created_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.id = $1 AND s.expires_at > now() AND u.disabled_at IS NULL`, hash,
	).Scan(&su.Session.UserID, &su.Session.CSRF, &su.Session.ExpiresAt, &su.Session.LastSeenAt,
		&su.User.ID, &su.User.OrgID, &su.User.Email, &su.User.ExternalID, &su.User.Role,
		&su.User.CreatedAt)
	return su, notFound(err)
}

// TouchSession records that a session was used, at most once a minute, so the
// read path does not write on every request.
func (s *Store) TouchSession(ctx context.Context, hash []byte) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sessions SET last_seen_at = now()
		 WHERE id = $1 AND last_seen_at < now() - interval '1 minute'`, hash)
	return err
}

// DeleteSession signs one browser out.
func (s *Store) DeleteSession(ctx context.Context, hash []byte) error {
	_, err := s.pool.Exec(ctx, "DELETE FROM sessions WHERE id = $1", hash)
	return err
}

// DeleteUserSessions signs a person out everywhere, which a role change or a
// revocation needs. That includes command-line tokens and half-finished
// command-line sign-ins, since a pending code could still mint a token.
func (s *Store) DeleteUserSessions(ctx context.Context, userID string) error {
	for _, table := range []string{"sessions", "cli_codes", "cli_tokens"} {
		if _, err := s.pool.Exec(ctx, "DELETE FROM "+table+" WHERE user_id = $1", userID); err != nil {
			return err
		}
	}
	return nil
}

// PurgeExpired removes sessions and login flows that have run out. Every
// lookup already filters on expiry, so this only keeps the tables small.
func (s *Store) PurgeExpired(ctx context.Context) error {
	for _, table := range []string{"sessions", "login_flows", "cli_codes", "cli_tokens"} {
		if _, err := s.pool.Exec(ctx,
			"DELETE FROM "+table+" WHERE expires_at < now()"); err != nil {
			return err
		}
	}
	return nil
}

// LoginFlow is one login in flight.
type LoginFlow struct {
	State    string
	Verifier string
	Nonce    string
	// Provider names the identity provider this login started against. The
	// callback reads it to pick the right client, so all providers can share
	// one redirect URI.
	Provider   string
	RedirectTo string
	// CLIRedirect is the loopback address a command line is waiting on, empty
	// for a sign-in to the panel. CLIChallenge is the S256 challenge whose
	// verifier redeems the code, and CLIState is what the command line checks
	// the callback against.
	CLIRedirect  string
	CLIChallenge string
	CLIState     string
}

// CLI reports whether this flow was started by a command line rather than by
// somebody opening the panel.
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

// TakeLoginFlow consumes a flow. It deletes and returns in one statement, so a
// replayed callback finds nothing.
func (s *Store) TakeLoginFlow(ctx context.Context, state string) (LoginFlow, error) {
	var f LoginFlow
	err := s.pool.QueryRow(ctx, `DELETE FROM login_flows
		WHERE state = $1 AND expires_at > now()
		RETURNING state, verifier, nonce, provider, redirect_to,
		          cli_redirect, cli_challenge, cli_state`, state,
	).Scan(&f.State, &f.Verifier, &f.Nonce, &f.Provider, &f.RedirectTo,
		&f.CLIRedirect, &f.CLIChallenge, &f.CLIState)
	return f, notFound(err)
}

// UserByExternalID finds the person behind an identity provider's subject
// claim.
func (s *Store) UserByExternalID(ctx context.Context, externalID string) (User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE external_id = $1`, externalID))
	return u, notFound(err)
}

// UserByID finds one person.
func (s *Store) UserByID(ctx context.Context, userID string) (User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = $1`, userID))
	return u, notFound(err)
}

// SetUserRole changes what a person may do.
func (s *Store) SetUserRole(ctx context.Context, userID, role string) error {
	return s.execOne(ctx, "UPDATE users SET role = $2 WHERE id = $1", userID, role)
}

// orgWhere reads the one organisation the rest of the query picks.
func (s *Store) orgWhere(ctx context.Context, rest string, args ...any) (Org, error) {
	var o Org
	err := s.pool.QueryRow(ctx, "SELECT id, name, created_at FROM orgs "+rest, args...).
		Scan(&o.ID, &o.Name, &o.CreatedAt)
	return o, notFound(err)
}

// OrgByEmailDomain finds the tenant a new sign-in belongs to, so a
// multi-tenant deployment needs no invitation step.
func (s *Store) OrgByEmailDomain(ctx context.Context, domain string) (Org, error) {
	return s.orgWhere(ctx, "WHERE lower(email_domain) = lower($1)", domain)
}

// OnlyOrg returns the single organisation, if there is exactly one. Dedicated
// and on-premises deployments have one, and need no domain mapping.
func (s *Store) OnlyOrg(ctx context.Context) (Org, error) {
	return s.orgWhere(ctx, "WHERE (SELECT count(*) FROM orgs) = 1")
}

// FirstOrg returns the oldest organisation. The operator key has no
// organisation of its own, so it joins this one.
func (s *Store) FirstOrg(ctx context.Context) (Org, error) {
	return s.orgWhere(ctx, "ORDER BY created_at, id LIMIT 1")
}

// ErrDomainTaken is returned when another organisation already has the domain.
// One domain maps to one tenant, so a second claim is refused.
var ErrDomainTaken = errors.New("store: that email domain belongs to another organisation")

// SetOrgEmailDomain attaches an email domain to a tenant.
func (s *Store) SetOrgEmailDomain(ctx context.Context, orgID, domain string) error {
	var d *string
	if domain = strings.TrimSpace(domain); domain != "" {
		d = &domain
	}
	err := s.execOne(ctx, "UPDATE orgs SET email_domain = $2 WHERE id = $1", orgID, d)
	if isUnique(err) {
		return ErrDomainTaken
	}
	return err
}

// Link identifies the person behind a sign-in.
type Link struct {
	OrgID string
	Email string
	// ExternalID is the provider-qualified subject, as Identity.ExternalID
	// spells it.
	ExternalID string
	Role       string
	// AdoptByEmail lets this sign-in take over a row that already has a
	// different provider's subject. It is off by default: with several
	// providers, matching on email alone would let one directory's user take
	// over another's account and role. Turn it on only to move an
	// organisation from one provider to another.
	AdoptByEmail bool
}

// ErrEmailTaken is returned when a sign-in would have to take over a person who
// is already bound to a different provider's subject.
var ErrEmailTaken = errors.New("store: that address already belongs to an identity from another provider")

// LinkUser creates or updates the person behind an identity, matching first on
// the provider's subject and then on the email address.
//
// The email match lets an organisation create its people in advance: the
// first sign-in adopts the row instead of adding a duplicate. A row that
// already has another subject needs Link.AdoptByEmail.
func (s *Store) LinkUser(ctx context.Context, newID string, l Link) (User, error) {
	if u, err := s.UserByExternalID(ctx, l.ExternalID); err == nil {
		if !strings.EqualFold(u.Email, l.Email) {
			if _, err := s.pool.Exec(ctx, "UPDATE users SET email = $2 WHERE id = $1", u.ID, l.Email); err != nil {
				return u, err
			}
			u.Email = l.Email
		}
		return u, nil
	} else if !errors.Is(err, ErrNotFound) {
		return User{}, err
	}

	// The guard is in the conflict clause, not a separate read, so two
	// sign-ins racing on one address cannot both pass it.
	adopt := "AND (users.external_id IS NULL OR users.external_id = '')"
	if l.AdoptByEmail {
		adopt = ""
	}
	u, err := scanUser(s.pool.QueryRow(ctx, `INSERT INTO users (id, org_id, email, external_id, role)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (org_id, email) DO UPDATE SET external_id = EXCLUDED.external_id
		WHERE true `+adopt+`
		RETURNING `+userColumns,
		newID, l.OrgID, l.Email, l.ExternalID, l.Role))
	// When the guard filters out the update, no row comes back.
	if errors.Is(notFound(err), ErrNotFound) {
		return User{}, ErrEmailTaken
	}
	return u, err
}
