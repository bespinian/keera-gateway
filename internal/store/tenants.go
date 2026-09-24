package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// Org is a customer in a multi-tenant deployment, or the whole enterprise in a
// dedicated or on-premises one.
type Org struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// EmailDomain places a first sign-in in the right tenant. Empty in a
	// deployment that has only one organisation.
	EmailDomain string    `json:"email_domain,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// Team is a team or department inside an org.
type Team struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"org_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// User is a person, identified either by Keera Gateway or by the customer's IdP.
type User struct {
	ID         string    `json:"id"`
	OrgID      string    `json:"org_id"`
	Email      string    `json:"email"`
	ExternalID string    `json:"external_id,omitempty"`
	Role       string    `json:"role"`
	CreatedAt  time.Time `json:"created_at"`
	// DisabledAt is when this person was turned off. A disabled person cannot
	// sign in and has no working key.
	DisabledAt *time.Time `json:"disabled_at,omitempty"`
}

// Disabled reports whether this person has been turned off.
func (u User) Disabled() bool { return u.DisabledAt != nil }

// userColumns is the select list scanUser reads, in its order.
const userColumns = `id, org_id, email, COALESCE(external_id,''), role, created_at, disabled_at`

func scanUser(r row) (User, error) {
	var u User
	err := r.Scan(&u.ID, &u.OrgID, &u.Email, &u.ExternalID, &u.Role, &u.CreatedAt, &u.DisabledAt)
	return u, err
}

// KeyInfo is an issued key without its secret.
//
// The secret is shown once and never stored, so screens, reports and audit
// entries name the key by its alias. A rotation moves the alias to a new
// secret on purpose.
type KeyInfo struct {
	ID        string     `json:"id"`
	OrgID     string     `json:"org_id"`
	TeamID    string     `json:"team_id,omitempty"`
	UserID    string     `json:"user_id,omitempty"`
	Alias     string     `json:"alias"`
	Prefix    string     `json:"prefix"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// CreateOrg inserts an organisation.
func (s *Store) CreateOrg(ctx context.Context, id, name string) (Org, error) {
	o := Org{ID: id, Name: name}
	err := s.pool.QueryRow(ctx,
		"INSERT INTO orgs (id, name) VALUES ($1,$2) RETURNING created_at", id, name,
	).Scan(&o.CreatedAt)
	return o, err
}

// DeletedOrg is what a deletion took with it. The counts are read in the same
// transaction as the delete, so they describe what was actually removed.
type DeletedOrg struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Teams int    `json:"teams"`
	Users int    `json:"users"`
	Keys  int    `json:"keys"`
}

// DeleteOrg removes an organisation and everything scoped to it.
//
// Teams, users, keys and sessions go through the foreign keys. Guardrails and
// spend are cleared first, while the team and key ids that name them still
// exist.
//
// Usage events and the audit log stay: finance invoices from them, and the
// audit log must keep the record of this deletion.
func (s *Store) DeleteOrg(ctx context.Context, orgID string) (DeletedOrg, error) {
	var gone DeletedOrg
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return gone, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	err = tx.QueryRow(ctx, `SELECT o.name,
			(SELECT count(*) FROM teams    WHERE org_id = o.id),
			(SELECT count(*) FROM users    WHERE org_id = o.id),
			(SELECT count(*) FROM api_keys WHERE org_id = o.id)
		FROM orgs o WHERE o.id = $1 FOR UPDATE`, orgID,
	).Scan(&gone.Name, &gone.Teams, &gone.Users, &gone.Keys)
	if err != nil {
		return gone, notFound(err)
	}
	gone.ID = orgID

	// An organisation spans three scopes, and each is named explicitly.
	const scoped = `(scope_type = 'org'  AND scope_id = $1)
		OR (scope_type = 'team' AND scope_id IN (SELECT id FROM teams    WHERE org_id = $1))
		OR (scope_type = 'key'  AND scope_id IN (SELECT id FROM api_keys WHERE org_id = $1))`
	if err := deleteScoped(ctx, tx, scoped, orgID); err != nil {
		return gone, err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM orgs WHERE id = $1", orgID); err != nil {
		return gone, err
	}
	return gone, tx.Commit(ctx)
}

// deleteScoped clears the guardrails and spend rows that match where. Those
// tables name their scope by plain id with no foreign key, so no cascade
// reaches them.
func deleteScoped(ctx context.Context, tx pgx.Tx, where, id string) error {
	for _, table := range []string{"guardrails", "spend"} {
		if _, err := tx.Exec(ctx, "DELETE FROM "+table+" WHERE "+where, id); err != nil {
			return err
		}
	}
	return nil
}

// ListOrgs returns every organisation.
func (s *Store) ListOrgs(ctx context.Context) ([]Org, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT id, name, COALESCE(email_domain, ''), created_at FROM orgs ORDER BY created_at")
	if err != nil {
		return nil, err
	}
	return collect(rows, func(r row) (Org, error) {
		var o Org
		err := r.Scan(&o.ID, &o.Name, &o.EmailDomain, &o.CreatedAt)
		return o, err
	})
}

// CreateTeam inserts a team.
func (s *Store) CreateTeam(ctx context.Context, id, orgID, name string) (Team, error) {
	t := Team{ID: id, OrgID: orgID, Name: name}
	err := s.pool.QueryRow(ctx,
		"INSERT INTO teams (id, org_id, name) VALUES ($1,$2,$3) RETURNING created_at",
		id, orgID, name,
	).Scan(&t.CreatedAt)
	if isUnique(err) {
		return t, fmt.Errorf("team %q already exists in %s", name, orgID)
	}
	return t, err
}

// ErrTeamNameTaken means another team in the organisation already has the
// name. The control plane turns it into a 409 with a readable message.
var ErrTeamNameTaken = errTeamNameTaken{}

type errTeamNameTaken struct{}

func (errTeamNameTaken) Error() string {
	return "store: a team of that name already exists in this organisation"
}

// RenameTeam gives a team a different name. Everything else refers to a team
// by id, so this changes one column. The old name is kept in the audit log.
func (s *Store) RenameTeam(ctx context.Context, teamID, name string) (Team, error) {
	var t Team
	err := s.pool.QueryRow(ctx,
		`UPDATE teams SET name = $2 WHERE id = $1
			RETURNING id, org_id, name, created_at`, teamID, name,
	).Scan(&t.ID, &t.OrgID, &t.Name, &t.CreatedAt)
	if isUnique(err) {
		return t, ErrTeamNameTaken
	}
	return t, notFound(err)
}

// TeamInUseError is a team that still has working keys. It lists their
// aliases, so somebody can decide which to revoke before the team can go.
type TeamInUseError struct {
	Team    string
	Aliases []string
}

func (e *TeamInUseError) Error() string {
	return fmt.Sprintf("store: team %q still has %d key(s) that have not been revoked",
		e.Team, len(e.Aliases))
}

// DeletedTeam is what a team deletion took with it, read inside the same
// transaction as the delete.
type DeletedTeam struct {
	ID    string `json:"id"`
	OrgID string `json:"org_id"`
	Name  string `json:"name"`
	// DetachedKeys is how many revoked keys stayed behind, no longer naming any
	// team. They are kept as history.
	DetachedKeys int `json:"detached_keys"`
}

// DeleteTeam removes a team that no live key uses.
//
// The foreign key cascades, so deleting a team with working keys would
// silently break those keys. The check runs against the row locked FOR
// UPDATE, so a key issued in between cannot slip through.
//
// Revoked keys are detached, not deleted, because usage rows still name them.
// Guardrails and spend have no foreign key, so they are cleared here.
func (s *Store) DeleteTeam(ctx context.Context, teamID string) (DeletedTeam, error) {
	var gone DeletedTeam
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return gone, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	err = tx.QueryRow(ctx,
		"SELECT org_id, name FROM teams WHERE id = $1 FOR UPDATE", teamID,
	).Scan(&gone.OrgID, &gone.Name)
	if err != nil {
		return gone, notFound(err)
	}
	gone.ID = teamID

	rows, err := tx.Query(ctx, `SELECT alias FROM api_keys
		WHERE team_id = $1 AND revoked_at IS NULL ORDER BY alias`, teamID)
	if err != nil {
		return gone, err
	}
	live, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return gone, err
	}
	if len(live) > 0 {
		return gone, &TeamInUseError{Team: gone.Name, Aliases: live}
	}

	if err := deleteScoped(ctx, tx, "scope_type = 'team' AND scope_id = $1", teamID); err != nil {
		return gone, err
	}
	tag, err := tx.Exec(ctx, "UPDATE api_keys SET team_id = NULL WHERE team_id = $1", teamID)
	if err != nil {
		return gone, err
	}
	gone.DetachedKeys = int(tag.RowsAffected())
	if _, err := tx.Exec(ctx, "DELETE FROM teams WHERE id = $1", teamID); err != nil {
		return gone, err
	}
	return gone, tx.Commit(ctx)
}

// UpsertUser creates a user, or updates the one with this org and email. The
// OIDC callback uses it, and must not care whether it has seen the user before.
func (s *Store) UpsertUser(ctx context.Context, newID, orgID, email, externalID, role string) (User, error) {
	u := User{OrgID: orgID, Email: email, ExternalID: externalID, Role: role}
	var ext *string
	if externalID != "" {
		ext = &externalID
	}
	err := s.pool.QueryRow(ctx, `INSERT INTO users (id, org_id, email, external_id, role)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (org_id, email) DO UPDATE SET
			external_id = COALESCE(EXCLUDED.external_id, users.external_id),
			role = EXCLUDED.role
		RETURNING id, external_id, created_at`,
		newID, orgID, email, ext, role,
	).Scan(&u.ID, &ext, &u.CreatedAt)
	if ext != nil {
		u.ExternalID = *ext
	}
	return u, err
}

// ListUsers returns the users of one org.
func (s *Store) ListUsers(ctx context.Context, orgID string) ([]User, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+userColumns+`
		FROM users WHERE org_id = $1 ORDER BY email`, orgID)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanUser)
}

// DisableUser turns a person off: it revokes every key attributed to them and
// ends every session, in one transaction. It returns how many keys it
// revoked.
//
// Disabling somebody twice keeps the first date, and still revokes any key
// issued in between.
func (s *Store) DisableUser(ctx context.Context, userID string) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		"UPDATE users SET disabled_at = COALESCE(disabled_at, now()) WHERE id = $1", userID)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() == 0 {
		return 0, ErrNotFound
	}
	tag, err = tx.Exec(ctx,
		"UPDATE api_keys SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL", userID)
	if err != nil {
		return 0, err
	}
	revoked := int(tag.RowsAffected())
	for _, table := range []string{"sessions", "cli_codes", "cli_tokens"} {
		if _, err := tx.Exec(ctx, "DELETE FROM "+table+" WHERE user_id = $1", userID); err != nil {
			return 0, err
		}
	}
	return revoked, tx.Commit(ctx)
}

// EnableUser lets a disabled person sign in again. Their old keys stay
// revoked: they get new ones.
func (s *Store) EnableUser(ctx context.Context, userID string) error {
	return s.execOne(ctx, "UPDATE users SET disabled_at = NULL WHERE id = $1", userID)
}

// CreateKey stores an issued key. The caller holds the only copy of the secret.
func (s *Store) CreateKey(ctx context.Context, k KeyInfo, hash []byte) (KeyInfo, error) {
	err := s.pool.QueryRow(ctx, `INSERT INTO api_keys
		(id, org_id, team_id, user_id, alias, key_hash, prefix, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING created_at`,
		k.ID, k.OrgID, nullable(k.TeamID), nullable(k.UserID), k.Alias, hash, k.Prefix, k.ExpiresAt,
	).Scan(&k.CreatedAt)
	return k, err
}

// RevokeKey marks a key unusable. It is kept rather than deleted so its usage
// history keeps resolving.
func (s *Store) RevokeKey(ctx context.Context, id string) error {
	return s.execOne(ctx,
		"UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL", id)
}

// LookupKey finds a key by the hash of what the client presented, with the
// guardrails of its whole chain. It is one round trip, because it runs on a
// cache miss in the request path.
func (s *Store) LookupKey(ctx context.Context, hash []byte) (*policy.Resolved, error) {
	// The team join also requires the team to be in the key's own org. The
	// control plane already refuses anything else; this is a second check, so a
	// bad row gives a key with no team rather than another tenant's guardrails.
	// A disabled holder counts as a revoked key, which is also a second check:
	// disabling somebody revokes their keys.
	rows, err := s.pool.Query(ctx, `
		SELECT k.id, k.org_id, COALESCE(t.id,''), COALESCE(k.user_id,''),
		       k.expires_at, COALESCE(k.revoked_at, u.disabled_at), p.scope_type, `+limitColumns+`
		FROM api_keys k
		LEFT JOIN users u ON u.id = k.user_id
		LEFT JOIN teams t ON t.id = k.team_id AND t.org_id = k.org_id
		LEFT JOIN guardrails p ON
			(p.scope_type = 'org'  AND p.scope_id = k.org_id) OR
			(p.scope_type = 'team' AND p.scope_id = t.id) OR
			(p.scope_type = 'key'  AND p.scope_id = k.id)
		WHERE k.key_hash = $1`, hash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var (
		key               policy.Key
		expires, revoked  *time.Time
		orgL, teamL, ownL *policy.Limits
		found             bool
	)
	for rows.Next() {
		var (
			scopeType *string
			lim       policy.Limits
			period    *string
		)
		dest := append([]any{&key.ID, &key.OrgID, &key.TeamID, &key.UserID,
			&expires, &revoked, &scopeType}, limitTargets(&lim, &period)...)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		found = true
		if scopeType == nil {
			continue
		}
		lim.BudgetPeriod = periodPtr(period)
		switch policy.ScopeType(*scopeType) {
		case policy.ScopeOrg:
			orgL = &lim
		case policy.ScopeTeam:
			teamL = &lim
		case policy.ScopeKey:
			ownL = &lim
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	switch {
	case !found:
		return nil, policy.ErrUnknownKey
	case revoked != nil:
		return nil, policy.ErrKeyRevoked
	case expires != nil && expires.Before(time.Now()):
		return nil, policy.ErrKeyExpired
	}
	return policy.Resolve(key, orgL, teamL, ownL), nil
}
