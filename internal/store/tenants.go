package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	keeraid "github.com/bespinian/keera-gateway/internal/id"
	"github.com/bespinian/keera-gateway/internal/policy"
)

// Org is a customer in a multi-tenant deployment, or the whole enterprise in a
// dedicated or on-premises one.
type Org struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// EmailDomain places a first sign-in in the right tenant. Empty in a
	// deployment that has only one organisation.
	EmailDomain string `json:"email_domain,omitempty"`
	// Limited is set on an organisation somebody created by signing up, until
	// it pays once or an operator lifts it. Until then its models may not run
	// on the deployment's own machines, and it gets no sandboxes.
	Limited   bool      `json:"limited,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Project groups keys inside an org, so they share guardrails and show up
// together in reports.
type Project struct {
	ID          string    `json:"id"`
	OrgID       string    `json:"org_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

// defaultProjectName is what the project an organisation is created with is
// called. It is an ordinary project, and its name can be changed.
const defaultProjectName = "default"

// User is a person, identified either by Keera Gateway or by the customer's IdP.
type User struct {
	ID         string    `json:"id"`
	OrgID      string    `json:"org_id"`
	Email      string    `json:"email"`
	Name       string    `json:"name,omitempty"`
	ExternalID string    `json:"external_id,omitempty"`
	Role       string    `json:"role"`
	CreatedAt  time.Time `json:"created_at"`
	// DisabledAt is when this person was turned off. A disabled person cannot
	// sign in and has no working key.
	DisabledAt *time.Time `json:"disabled_at,omitempty"`
	// LastActiveAt is the later of this person's last sign-in and their last
	// request through one of their keys. Only ListUsers fills it in.
	LastActiveAt *time.Time `json:"last_active_at,omitempty"`
}

func (u User) Disabled() bool { return u.DisabledAt != nil }

// userColumnsOf is the select list userTargets fills, in its order, with each
// column prefixed by table ("u." or ""). A join needs the prefix, because the
// tables it joins share column names.
func userColumnsOf(table string) string {
	return fmt.Sprintf(`%[1]sid, %[1]sorg_id, %[1]semail, %[1]sname, COALESCE(%[1]sexternal_id,''),
	%[1]srole, %[1]screated_at, %[1]sdisabled_at`, table)
}

// userColumns is the select list scanUser reads, from users alone.
var userColumns = userColumnsOf("")

// userTargets points at the fields userColumnsOf fills, in its order.
func userTargets(u *User) []any {
	return []any{&u.ID, &u.OrgID, &u.Email, &u.Name, &u.ExternalID, &u.Role, &u.CreatedAt,
		&u.DisabledAt}
}

func scanUser(r row) (User, error) {
	var u User
	err := r.Scan(userTargets(&u)...)
	return u, err
}

// KeyInfo is an issued key without its secret.
//
// The secret is shown once and never stored, so screens, reports and audit
// entries name the key by its name. A rotation carries the name over to the
// new key.
type KeyInfo struct {
	ID        string `json:"id"`
	OrgID     string `json:"org_id"`
	ProjectID string `json:"project_id,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	Name      string `json:"name"`
	Prefix    string `json:"prefix"`
	// Kind is standard or subscription. Empty is stored as standard.
	Kind      policy.KeyKind `json:"kind"`
	CreatedAt time.Time      `json:"created_at"`
	ExpiresAt *time.Time     `json:"expires_at,omitempty"`
	RevokedAt *time.Time     `json:"revoked_at,omitempty"`
	// Pending is a key waiting to be activated. It has no secret and no
	// prefix until then.
	Pending bool `json:"pending,omitempty"`
}

// keyColumnsOf is the select list keyTargets fills, in its order, with each
// column prefixed by table, as in userColumnsOf.
func keyColumnsOf(table string) string {
	return fmt.Sprintf(`%[1]sid, %[1]sorg_id, COALESCE(%[1]sproject_id,''),
	COALESCE(%[1]suser_id,''), %[1]sname, %[1]sprefix, %[1]skind, %[1]screated_at,
	%[1]sexpires_at, %[1]srevoked_at, %[1]skey_hash IS NULL`, table)
}

// keyTargets points at the fields keyColumnsOf fills, in its order.
func keyTargets(k *KeyInfo) []any {
	return []any{&k.ID, &k.OrgID, &k.ProjectID, &k.UserID, &k.Name, &k.Prefix, &k.Kind,
		&k.CreatedAt, &k.ExpiresAt, &k.RevokedAt, &k.Pending}
}

// State is the one word a screen shows for a key at now: revoked, expired,
// pending or active. Revocation wins over expiry, because somebody did it on
// purpose.
func (k KeyInfo) State(now time.Time) string {
	switch {
	case k.RevokedAt != nil:
		return "revoked"
	case k.ExpiresAt != nil && k.ExpiresAt.Before(now):
		return "expired"
	case k.Pending:
		return "pending"
	default:
		return "active"
	}
}

// OrgTemplate is what a new organisation starts with: a copy of each model
// and sandbox class, from the template files.
type OrgTemplate struct {
	Models         []policy.Model
	SandboxClasses []policy.SandboxClass
}

// CreateOrg inserts an organisation, with its email domain if o has one, and
// what it starts with. The domain is stored as given, so the caller cleans it
// with policy.CleanEmailDomain. It is one transaction, so an organisation
// never exists without its template.
func (s *Store) CreateOrg(ctx context.Context, o Org, tmpl OrgTemplate) (Org, error) {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		o, err = createOrg(ctx, tx, o, tmpl)
		return err
	})
	return o, err
}

// createOrg is CreateOrg inside a transaction the caller commits.
func createOrg(ctx context.Context, tx pgx.Tx, o Org, tmpl OrgTemplate) (Org, error) {
	id := o.ID
	domain := nullable(o.EmailDomain)
	// The domain is set in the same insert, so a taken one creates nothing.
	// Setting it afterwards left an organisation behind that a retry then
	// created a second time.
	if err := tx.QueryRow(ctx,
		"INSERT INTO orgs (id, name, email_domain, limited) VALUES ($1,$2,$3,$4) RETURNING created_at",
		id, o.Name, domain, o.Limited,
	).Scan(&o.CreatedAt); err != nil {
		return o, orgConflict(err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO projects (id, org_id, name, description) VALUES ($1,$2,$3,$4)`,
		keeraid.New("project"), id, defaultProjectName,
		"The default project of "+o.Name); err != nil {
		return o, err
	}
	for _, m := range tmpl.Models {
		m.OrgID = id
		if err := upsertModel(ctx, tx, m, Credential{}); err != nil {
			return o, err
		}
	}
	for _, c := range tmpl.SandboxClasses {
		c.OrgID = id
		if err := upsertSandboxClass(ctx, tx, &c); err != nil {
			return o, err
		}
	}
	return o, nil
}

// ErrOrgNameTaken means another organisation already has the name, ignoring
// case.
var ErrOrgNameTaken = errors.New("store: another organisation already has that name")

// orgConflict names which of an organisation's unique columns a write
// collided on.
func orgConflict(err error) error {
	switch {
	case uniqueOn(err, "orgs_name_key"):
		return ErrOrgNameTaken
	case uniqueOn(err, "orgs_email_domain_key"):
		return ErrDomainTaken
	}
	return err
}

// OrgChange is what UpdateOrg changes. A nil field is left as it is, and an
// empty EmailDomain clears it. Like CreateOrg, it stores the domain as given.
type OrgChange struct {
	Name        *string
	EmailDomain *string
	Limited     *bool
}

// UpdateOrg renames an organisation, changes its email domain, or both, in
// one statement, so a refused domain does not leave the rename behind.
// Everything else refers to an organisation by id, so the name is only a
// label. The old one is kept in the audit log.
func (s *Store) UpdateOrg(ctx context.Context, orgID string, c OrgChange) (Org, error) {
	var domain *string
	if c.EmailDomain != nil {
		domain = nullable(*c.EmailDomain)
	}
	o, err := scanOrg(s.pool.QueryRow(ctx, `UPDATE orgs SET
			name = COALESCE($2, name),
			email_domain = CASE WHEN $3 THEN $4 ELSE email_domain END,
			limited = COALESCE($5, limited)
		WHERE id = $1
		RETURNING `+orgFields,
		orgID, c.Name, c.EmailDomain != nil, domain, c.Limited))
	return o, notFound(orgConflict(err))
}

// LimitedOrgs maps each organisation that is still limited to its name, for
// the gateway to hold their models to what they may use, and to say whose
// they are when it refuses one.
func (s *Store) LimitedOrgs(ctx context.Context) (map[string]string, error) {
	return s.names(ctx, "SELECT id, name FROM orgs WHERE limited")
}

// DeletedOrg is what a deletion took with it. The counts are read in the same
// transaction as the delete, so they describe what was actually removed.
type DeletedOrg struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Projects int    `json:"projects"`
	Users    int    `json:"users"`
	Keys     int    `json:"keys"`
	// LiveSandboxes is set only when the delete was refused for them.
	LiveSandboxes int `json:"live_sandboxes,omitempty"`
	// CardToken is the saved card the delete removed, zero for none, so
	// PostFinance can forget it too.
	CardToken int64 `json:"-"`
}

// ErrOrgHasSandboxes refuses to delete an organisation whose sandboxes still
// hold machines. Deleting their rows would leave the machines running with
// nothing left to stop them.
var ErrOrgHasSandboxes = errors.New("store: the organisation still has live sandboxes")

// DeleteOrg removes an organisation and everything scoped to it.
//
// Everything else goes through the foreign keys. Guardrails and spend are
// cleared first, while the project and key ids that name them still exist.
//
// The request log, billing lines, payments, the credit account and the audit
// log stay: finance invoices from them, and the audit log must keep the record
// of this deletion. The saved card goes, so nobody can charge it again.
func (s *Store) DeleteOrg(ctx context.Context, orgID string) (DeletedOrg, error) {
	var gone DeletedOrg
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT o.name,
				(SELECT count(*) FROM projects WHERE org_id = o.id),
				(SELECT count(*) FROM users    WHERE org_id = o.id),
				(SELECT count(*) FROM api_keys WHERE org_id = o.id)
			FROM orgs o WHERE o.id = $1 FOR UPDATE`, orgID,
		).Scan(&gone.Name, &gone.Projects, &gone.Users, &gone.Keys)
		if err != nil {
			return notFound(err)
		}
		gone.ID = orgID

		// The row lock above holds off a sandbox being created meanwhile: its
		// insert has to lock the same row.
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM sandboxes WHERE org_id = $1 AND "+liveSandbox,
			orgID).Scan(&gone.LiveSandboxes); err != nil {
			return err
		}
		if gone.LiveSandboxes > 0 {
			return ErrOrgHasSandboxes
		}

		// An organisation spans three scopes, and each is named explicitly.
		const scoped = `(scope_type = 'org'     AND scope_id = $1)
			OR (scope_type = 'project' AND scope_id IN (SELECT id FROM projects WHERE org_id = $1))
			OR (scope_type = 'key'     AND scope_id IN (SELECT id FROM api_keys WHERE org_id = $1))`
		if err := deleteScoped(ctx, tx, scoped, orgID); err != nil {
			return err
		}
		if gone.CardToken, err = forgetCard(ctx, tx, orgID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "DELETE FROM orgs WHERE id = $1", orgID)
		return err
	})
	return gone, err
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

func (s *Store) ListOrgs(ctx context.Context) ([]Org, error) {
	return queryAll(ctx, s.pool, scanOrg, orgColumns+" FROM orgs ORDER BY created_at")
}

// orgFields are the columns scanOrg reads, in its order.
const orgFields = "id, name, COALESCE(email_domain, ''), limited, created_at"

const orgColumns = "SELECT " + orgFields

func scanOrg(r row) (Org, error) {
	var o Org
	err := r.Scan(&o.ID, &o.Name, &o.EmailDomain, &o.Limited, &o.CreatedAt)
	return o, err
}

func (s *Store) CreateProject(ctx context.Context, p Project) (Project, error) {
	err := s.pool.QueryRow(ctx,
		`INSERT INTO projects (id, org_id, name, description) VALUES ($1,$2,$3,$4)
			RETURNING created_at`,
		p.ID, p.OrgID, p.Name, p.Description,
	).Scan(&p.CreatedAt)
	if isUnique(err) {
		return p, ErrProjectNameTaken
	}
	return p, err
}

// ErrProjectNameTaken means another project in the organisation already has
// the name. The control plane turns it into a 409 with a readable message.
var ErrProjectNameTaken = errors.New("store: a project of that name already exists in this organisation")

// ProjectChange is what UpdateProject changes. A nil field is left as it is.
type ProjectChange struct {
	Name        *string
	Description *string
}

// UpdateProject renames a project, changes its description, or both.
// Everything else refers to a project by id, so the name is only a label.
// The old one is kept in the audit log.
func (s *Store) UpdateProject(ctx context.Context, projectID string, c ProjectChange) (Project, error) {
	var p Project
	err := s.pool.QueryRow(ctx,
		`UPDATE projects SET
			name = COALESCE($2, name),
			description = COALESCE($3, description)
		WHERE id = $1
		RETURNING id, org_id, name, description, created_at`,
		projectID, c.Name, c.Description,
	).Scan(&p.ID, &p.OrgID, &p.Name, &p.Description, &p.CreatedAt)
	if isUnique(err) {
		return p, ErrProjectNameTaken
	}
	return p, notFound(err)
}

// ProjectInUseError is a project that still has working keys. It lists their
// names, so somebody can decide which to revoke before the project can go.
type ProjectInUseError struct {
	Project string
	Keys    []string
}

func (e *ProjectInUseError) Error() string {
	return fmt.Sprintf("store: project %q still has %d key(s) that have not been revoked",
		e.Project, len(e.Keys))
}

// DeletedProject is what a project deletion took with it, read inside the same
// transaction as the delete.
type DeletedProject struct {
	ID    string `json:"id"`
	OrgID string `json:"org_id"`
	Name  string `json:"name"`
	// DetachedKeys is how many revoked keys stayed behind, no longer naming any
	// project. They are kept as history.
	DetachedKeys int `json:"detached_keys"`
	// LiveSandboxes is set when ErrProjectHasSandboxes refused the delete.
	LiveSandboxes int `json:"-"`
}

// ErrProjectHasSandboxes refuses to delete a project whose sandboxes still
// hold resources. An expired one counts: it can be resumed.
var ErrProjectHasSandboxes = errors.New("store: the project still has live sandboxes")

// oldestProject selects the oldest project of the organisation named by the
// parameter orgParam, such as "$1". A key or sandbox that names no project
// goes there. That is the one the organisation was created with, until it is
// deleted.
func oldestProject(orgParam string) string {
	return "SELECT id FROM projects WHERE org_id = " + orgParam + " ORDER BY created_at, id LIMIT 1"
}

// FirstProject returns the id of an organisation's oldest project.
// ErrNotFound means there is no project.
func (s *Store) FirstProject(ctx context.Context, orgID string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, oldestProject("$1"), orgID).Scan(&id)
	return id, notFound(err)
}

// DeleteProject removes a project that no live key uses.
//
// Deleting it with a working key would move that key to another project's
// guardrails, budget and rate limit. The check runs against the row locked
// FOR UPDATE, so a key issued in between cannot slip through.
//
// Revoked keys are detached, not deleted, because usage rows still name them.
// Guardrails and spend have no foreign key, so they are cleared here.
func (s *Store) DeleteProject(ctx context.Context, projectID string) (DeletedProject, error) {
	var gone DeletedProject
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			"SELECT org_id, name FROM projects WHERE id = $1 FOR UPDATE", projectID,
		).Scan(&gone.OrgID, &gone.Name)
		if err != nil {
			return notFound(err)
		}
		gone.ID = projectID

		// A sandbox's project decides its budget, quota and models. The row
		// lock above holds off one being created meanwhile: its insert checks
		// the foreign key, which waits for it.
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM sandboxes WHERE project_id = $1 AND "+liveSandbox,
			projectID).Scan(&gone.LiveSandboxes); err != nil {
			return err
		}
		if gone.LiveSandboxes > 0 {
			return ErrProjectHasSandboxes
		}

		rows, err := tx.Query(ctx, `SELECT name FROM api_keys
			WHERE project_id = $1 AND revoked_at IS NULL ORDER BY name`, projectID)
		if err != nil {
			return err
		}
		live, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if len(live) > 0 {
			return &ProjectInUseError{Project: gone.Name, Keys: live}
		}

		if err := deleteScoped(ctx, tx, "scope_type = 'project' AND scope_id = $1", projectID); err != nil {
			return err
		}
		// The foreign key would detach them too; this is here for the count.
		tag, err := tx.Exec(ctx, "UPDATE api_keys SET project_id = NULL WHERE project_id = $1", projectID)
		if err != nil {
			return err
		}
		gone.DetachedKeys = int(tag.RowsAffected())
		_, err = tx.Exec(ctx, "DELETE FROM projects WHERE id = $1", projectID)
		return err
	})
	return gone, err
}

// ErrUserExists is returned when the organisation already has someone with
// that address. Adding must not quietly change who they are: their role
// changes through SetUserRole, and their subject only at sign-in.
var ErrUserExists = errors.New("store: that address is already in the organisation")

// ErrExternalIDTaken is returned when someone in the organisation already has
// the subject.
var ErrExternalIDTaken = errors.New("store: that subject already belongs to another person")

// AddUser creates a person, so they are placed in the right organisation
// before their first sign-in. It never touches someone who already exists.
func (s *Store) AddUser(ctx context.Context, newID, orgID, email, externalID, role string) (User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx, `INSERT INTO users (id, org_id, email, external_id, role)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (org_id, email) DO NOTHING
		RETURNING `+userColumns,
		newID, orgID, email, nullable(externalID), role))
	switch {
	case errors.Is(notFound(err), ErrNotFound):
		return User{}, ErrUserExists
	case isUnique(err):
		return User{}, ErrExternalIDTaken
	}
	return u, err
}

// ListUsers returns the users of one org. An empty orgID means every
// organisation, as for projects.
func (s *Store) ListUsers(ctx context.Context, orgID string) ([]User, error) {
	// GREATEST skips nulls, so either one alone is enough. The last request
	// comes from usage_daily, which outlives the request log.
	return queryAll(ctx, s.pool, func(r row) (User, error) {
		var u User
		err := r.Scan(append(userTargets(&u), &u.LastActiveAt)...)
		return u, err
	}, `SELECT `+userColumnsOf("u.")+`, GREATEST(u.signed_in_at, d.last_at)
		FROM users u
		LEFT JOIN (
		    SELECT user_id, max(last_at) AS last_at FROM usage_daily
		    WHERE $1 = '' OR org_id = $1 GROUP BY user_id
		) d ON d.user_id = u.id
		WHERE ($1 = '' OR u.org_id = $1) ORDER BY u.email`, orgID)
}

// DisableUser turns a person off: it revokes every key attributed to them,
// ends every session and removes their passkeys, in one transaction. It
// returns how many keys it revoked.
//
// Disabling somebody twice keeps the first date, and still revokes any key
// issued in between.
func (s *Store) DisableUser(ctx context.Context, userID string) (int, error) {
	var revoked int
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			"UPDATE users SET disabled_at = COALESCE(disabled_at, now()) WHERE id = $1", userID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		tag, err = tx.Exec(ctx,
			"UPDATE api_keys SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL", userID)
		if err != nil {
			return err
		}
		revoked = int(tag.RowsAffected())
		if err := deleteUserSessions(ctx, tx, userID); err != nil {
			return err
		}
		// Like keys, passkeys do not come back with 'enable': a new set-up link
		// does.
		return deletePasskeys(ctx, tx, userID)
	})
	if err != nil {
		return 0, err
	}
	return revoked, nil
}

// EnableUser lets a disabled person sign in again. Their old keys stay
// revoked: they get new ones.
func (s *Store) EnableUser(ctx context.Context, userID string) error {
	return s.execOne(ctx, "UPDATE users SET disabled_at = NULL WHERE id = $1", userID)
}

// CreateKey stores an issued key. The caller holds the only copy of the secret.
// A nil hash stores a pending key, which ActivateKey gives its secret later.
func (s *Store) CreateKey(ctx context.Context, k KeyInfo, hash []byte) (KeyInfo, error) {
	return insertKey(ctx, s.pool, k, hash)
}

// ErrNoProject refuses a key in an organisation that has no projects.
var ErrNoProject = errors.New("store: the organisation has no projects, and every key is in one")

// insertKey puts a key without a project into its organisation's oldest one.
func insertKey(ctx context.Context, db querier, k KeyInfo, hash []byte) (KeyInfo, error) {
	if k.Kind == "" {
		k.Kind = policy.KeyStandard
	}
	k.Pending = hash == nil
	err := db.QueryRow(ctx, `INSERT INTO api_keys
		(id, org_id, project_id, user_id, name, key_hash, prefix, kind, expires_at)
		VALUES ($1, $2,
			COALESCE($3, (`+oldestProject("$2")+`)),
			$4, $5, $6, $7, $8, $9)
		RETURNING project_id, created_at`,
		k.ID, k.OrgID, nullable(k.ProjectID), nullable(k.UserID), k.Name, hash, k.Prefix,
		string(k.Kind), k.ExpiresAt,
	).Scan(&k.ProjectID, &k.CreatedAt)
	if violates(err, "api_keys_project_check") {
		return k, ErrNoProject
	}
	return k, err
}

// RevokeKey marks a key unusable. It is kept rather than deleted so its usage
// history keeps resolving. A key already revoked gives policy.ErrKeyRevoked,
// so a second revoke is not a second event.
func (s *Store) RevokeKey(ctx context.Context, id string) error {
	var found, revoked bool
	err := s.pool.QueryRow(ctx, `WITH revoked AS (
			UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL RETURNING id)
		SELECT EXISTS (SELECT 1 FROM api_keys WHERE id = $1), EXISTS (SELECT 1 FROM revoked)`,
		id).Scan(&found, &revoked)
	switch {
	case err != nil:
		return err
	case !found:
		return ErrNotFound
	case !revoked:
		return policy.ErrKeyRevoked
	}
	return nil
}

// RenameKey changes what a key is called and returns the name it had. Revoked
// keys can be renamed too: their name still heads their usage history.
func (s *Store) RenameKey(ctx context.Context, id, name string) (old string, err error) {
	err = s.pool.QueryRow(ctx, `UPDATE api_keys k SET name = $2
		FROM api_keys was WHERE k.id = $1 AND was.id = k.id
		RETURNING was.name`, id, name).Scan(&old)
	return old, notFound(err)
}

// ErrKeyActive is a key that was activated already. Its secret was shown
// then, so a second activation would be a rotation in disguise.
var ErrKeyActive = errors.New("store: the key is already activated")

// ActivateKey gives a pending key its secret, and a new name when name is
// not empty. It returns the key as it now is.
//
// The update only matches a pending key, so two activations racing each
// other cannot both get a secret.
func (s *Store) ActivateKey(ctx context.Context, id, name string, hash []byte, prefix string,
) (KeyInfo, error) {
	var k KeyInfo
	err := s.pool.QueryRow(ctx, `UPDATE api_keys
		SET key_hash = $2, prefix = $3, name = COALESCE(NULLIF($4, ''), name)
		WHERE id = $1 AND key_hash IS NULL AND revoked_at IS NULL
			AND (expires_at IS NULL OR expires_at > now())
		RETURNING `+keyColumnsOf(""), id, hash, prefix, name,
	).Scan(keyTargets(&k)...)
	if !errors.Is(err, pgx.ErrNoRows) {
		return k, err
	}
	// Nothing matched: say why, so the caller is told what to do instead.
	// A pending key whose expiry passed would be refused at once.
	var pending, revoked, expired bool
	err = s.pool.QueryRow(ctx, `SELECT key_hash IS NULL, revoked_at IS NOT NULL,
		expires_at IS NOT NULL AND expires_at <= now()
		FROM api_keys WHERE id = $1`, id).Scan(&pending, &revoked, &expired)
	switch {
	case err != nil:
		return KeyInfo{}, notFound(err)
	case revoked:
		return KeyInfo{}, policy.ErrKeyRevoked
	case pending && expired:
		return KeyInfo{}, policy.ErrKeyExpired
	default:
		return KeyInfo{}, ErrKeyActive
	}
}

// RotateKey replaces the key oldID with next, in one transaction: next gets
// the old key's organisation, project, person, kind and own guardrails, and
// the old key is revoked. An empty next.Name keeps the old name. A nil
// next.ExpiresAt gives the new key the old key's lifetime, counted from now.
//
// Doing it in steps can leave both keys live, or a new key without the old
// one's limits, which quietly loosens a guardrail.
func (s *Store) RotateKey(ctx context.Context, oldID string, next KeyInfo, hash []byte) (KeyInfo, error) {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var old KeyInfo
		err := tx.QueryRow(ctx, `SELECT `+keyColumnsOf("")+` FROM api_keys
			WHERE id = $1 FOR UPDATE`, oldID).Scan(keyTargets(&old)...)
		if err != nil {
			return notFound(err)
		}
		// Revoked, there is nothing to rotate. Pending, there is no secret to
		// replace, so it is activated instead.
		if old.RevokedAt != nil {
			return policy.ErrKeyRevoked
		}
		if old.Pending {
			return policy.ErrKeyPending
		}
		next.OrgID, next.ProjectID, next.UserID, next.Kind = old.OrgID, old.ProjectID, old.UserID, old.Kind
		if next.Name == "" {
			next.Name = old.Name
		}
		// The lifetime, not the date: a key rotated a week before it lapses should
		// not be replaced by one that lapses in a week.
		if next.ExpiresAt == nil && old.ExpiresAt != nil {
			t := time.Now().Add(max(old.ExpiresAt.Sub(old.CreatedAt).Round(time.Hour), time.Hour))
			next.ExpiresAt = &t
		}

		if next, err = insertKey(ctx, tx, next, hash); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, copyKeyGuardrailSQL, oldID, next.ID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "UPDATE api_keys SET revoked_at = now() WHERE id = $1", oldID)
		return err
	})
	if err != nil {
		return KeyInfo{}, err
	}
	return next, nil
}

// LookupKey finds a key by the hash of what the client presented, with the
// guardrails of its whole chain. It is one round trip, because it runs on a
// cache miss in the request path.
func (s *Store) LookupKey(ctx context.Context, hash []byte) (*policy.Resolved, error) {
	return s.lookupKey(ctx, "k.key_hash = $1", hash)
}

// LookupKeyByID is LookupKey for a key named by its id, for the panel's
// playground, which never holds the key itself.
func (s *Store) LookupKeyByID(ctx context.Context, id string) (*policy.Resolved, error) {
	return s.lookupKey(ctx, "k.id = $1", id)
}

// lookupKey finds the key that where matches. where is a constant, never
// input.
func (s *Store) lookupKey(ctx context.Context, where string, arg any) (*policy.Resolved, error) {
	// The project join also requires the project to be in the key's own org. The
	// control plane already refuses anything else; this is a second check, so a
	// bad row gives a key with no project rather than another tenant's guardrails.
	// A disabled holder counts as a revoked key, which is also a second check:
	// disabling somebody revokes their keys.
	rows, err := s.pool.Query(ctx, `
		SELECT k.id, k.org_id, COALESCE(pr.id,''), COALESCE(k.user_id,''), k.kind,
		       k.expires_at, COALESCE(k.revoked_at, u.disabled_at), k.key_hash IS NULL,
		       p.scope_type, `+limitColumns+`
		FROM api_keys k
		LEFT JOIN users u ON u.id = k.user_id
		LEFT JOIN projects pr ON pr.id = k.project_id AND pr.org_id = k.org_id
		LEFT JOIN guardrails p ON
			(p.scope_type = 'org'     AND p.scope_id = k.org_id) OR
			(p.scope_type = 'project' AND p.scope_id = pr.id) OR
			(p.scope_type = 'key'     AND p.scope_id = k.id)
		WHERE `+where, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var (
		key                  policy.Key
		expires, revoked     *time.Time
		orgL, projectL, ownL *policy.Limits
		found, pending       bool
	)
	for rows.Next() {
		var (
			scopeType *string
			lim       policy.Limits
			period    *string
		)
		dest := append([]any{&key.ID, &key.OrgID, &key.ProjectID, &key.UserID, &key.Kind,
			&expires, &revoked, &pending, &scopeType}, limitTargets(&lim, &period)...)
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
		case policy.ScopeProject:
			projectL = &lim
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
	case pending:
		return nil, policy.ErrKeyPending
	}
	return policy.Resolve(key, orgL, projectL, ownL), nil
}
