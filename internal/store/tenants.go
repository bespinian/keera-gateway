package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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

// DefaultProjectName is what the project an organisation is created with is
// called. It is an ordinary project, and its name can be changed.
const DefaultProjectName = "default"

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

// userColumnsOf is the select list userTargets fills, in its order, with each
// column prefixed by table ("u." or ""). A join needs the prefix, because the
// tables it joins share column names.
func userColumnsOf(table string) string {
	return fmt.Sprintf(`%[1]sid, %[1]sorg_id, %[1]semail, COALESCE(%[1]sexternal_id,''),
	%[1]srole, %[1]screated_at, %[1]sdisabled_at`, table)
}

// userColumns is the select list scanUser reads, from users alone.
var userColumns = userColumnsOf("")

// userTargets points at the fields userColumnsOf fills, in its order.
func userTargets(u *User) []any {
	return []any{&u.ID, &u.OrgID, &u.Email, &u.ExternalID, &u.Role, &u.CreatedAt, &u.DisabledAt}
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
}

// State is the one word a screen shows for a key at now: revoked, expired or
// active. Revocation wins over expiry, because somebody did it on purpose.
func (k KeyInfo) State(now time.Time) string {
	switch {
	case k.RevokedAt != nil:
		return "revoked"
	case k.ExpiresAt != nil && k.ExpiresAt.Before(now):
		return "expired"
	default:
		return "active"
	}
}

// OrgTemplate is what a new organisation starts with: a copy of each model
// and sandbox class, from the catalogue files.
type OrgTemplate struct {
	Models         []policy.Model
	SandboxClasses []policy.SandboxClass
}

// CreateOrg inserts an organisation, with its email domain if o has one, and
// what it starts with. It is one transaction, so an organisation never exists
// without its template.
func (s *Store) CreateOrg(ctx context.Context, o Org, tmpl OrgTemplate) (Org, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return o, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if o, err = createOrg(ctx, tx, o, tmpl); err != nil {
		return o, err
	}
	return o, tx.Commit(ctx)
}

// createOrg is CreateOrg inside a transaction the caller commits.
func createOrg(ctx context.Context, tx pgx.Tx, o Org, tmpl OrgTemplate) (Org, error) {
	id := o.ID
	o.EmailDomain = strings.TrimSpace(o.EmailDomain)
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
		keeraid.New("project"), id, DefaultProjectName,
		"The default project of "+o.Name); err != nil {
		return o, err
	}
	for _, m := range tmpl.Models {
		m.OrgID = id
		if err := upsertModel(ctx, tx, m); err != nil {
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
// empty EmailDomain clears it.
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
		domain = nullable(strings.TrimSpace(*c.EmailDomain))
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

// LimitedOrgs lists the organisations that are still limited, for the
// gateway to hold their models to what they may use.
func (s *Store) LimitedOrgs(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, "SELECT id FROM orgs WHERE limited")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
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
// Usage events, the credit account and the audit log stay: finance invoices
// from them, and the audit log must keep the record of this deletion. The
// saved card goes, so nobody can charge it again.
func (s *Store) DeleteOrg(ctx context.Context, orgID string) (DeletedOrg, error) {
	var gone DeletedOrg
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return gone, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	err = tx.QueryRow(ctx, `SELECT o.name,
			(SELECT count(*) FROM projects WHERE org_id = o.id),
			(SELECT count(*) FROM users    WHERE org_id = o.id),
			(SELECT count(*) FROM api_keys WHERE org_id = o.id)
		FROM orgs o WHERE o.id = $1 FOR UPDATE`, orgID,
	).Scan(&gone.Name, &gone.Projects, &gone.Users, &gone.Keys)
	if err != nil {
		return gone, notFound(err)
	}
	gone.ID = orgID

	// The row lock above holds off a sandbox being created meanwhile: its
	// insert has to lock the same row.
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM sandboxes WHERE org_id = $1 AND "+liveSandbox,
		orgID).Scan(&gone.LiveSandboxes); err != nil {
		return gone, err
	}
	if gone.LiveSandboxes > 0 {
		return gone, ErrOrgHasSandboxes
	}

	// An organisation spans three scopes, and each is named explicitly.
	const scoped = `(scope_type = 'org'     AND scope_id = $1)
		OR (scope_type = 'project' AND scope_id IN (SELECT id FROM projects WHERE org_id = $1))
		OR (scope_type = 'key'     AND scope_id IN (SELECT id FROM api_keys WHERE org_id = $1))`
	if err := deleteScoped(ctx, tx, scoped, orgID); err != nil {
		return gone, err
	}
	if gone.CardToken, err = forgetCard(ctx, tx, orgID); err != nil {
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

// CreateProject inserts a project.
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
}

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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return gone, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	err = tx.QueryRow(ctx,
		"SELECT org_id, name FROM projects WHERE id = $1 FOR UPDATE", projectID,
	).Scan(&gone.OrgID, &gone.Name)
	if err != nil {
		return gone, notFound(err)
	}
	gone.ID = projectID

	rows, err := tx.Query(ctx, `SELECT name FROM api_keys
		WHERE project_id = $1 AND revoked_at IS NULL ORDER BY name`, projectID)
	if err != nil {
		return gone, err
	}
	live, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return gone, err
	}
	if len(live) > 0 {
		return gone, &ProjectInUseError{Project: gone.Name, Keys: live}
	}

	if err := deleteScoped(ctx, tx, "scope_type = 'project' AND scope_id = $1", projectID); err != nil {
		return gone, err
	}
	// The foreign key would detach them too; this is here for the count.
	tag, err := tx.Exec(ctx, "UPDATE api_keys SET project_id = NULL WHERE project_id = $1", projectID)
	if err != nil {
		return gone, err
	}
	gone.DetachedKeys = int(tag.RowsAffected())
	if _, err := tx.Exec(ctx, "DELETE FROM projects WHERE id = $1", projectID); err != nil {
		return gone, err
	}
	return gone, tx.Commit(ctx)
}

// ErrUserExists is returned when the organisation already has someone with
// that address. Adding must not quietly change who they are: their role
// changes through SetUserRole, and their subject only at sign-in.
var ErrUserExists = errors.New("store: that address is already in the organisation")

// ErrExternalIDTaken is returned when another person already has the subject.
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
	return queryAll(ctx, s.pool, scanUser, `SELECT `+userColumns+`
		FROM users WHERE ($1 = '' OR org_id = $1) ORDER BY email`, orgID)
}

// DisableUser turns a person off: it revokes every key attributed to them,
// ends every session and removes their passkeys, in one transaction. It
// returns how many keys it revoked.
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
	if err := deleteUserSessions(ctx, tx, userID); err != nil {
		return 0, err
	}
	// Like keys, passkeys do not come back with 'enable': a new set-up link
	// does.
	if err := deletePasskeys(ctx, tx, userID); err != nil {
		return 0, err
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
	return insertKey(ctx, s.pool, k, hash)
}

// ErrNoProject refuses a key in an organisation that has no projects.
var ErrNoProject = errors.New("store: the organisation has no projects, and every key is in one")

// insertKey puts a key without a project into its organisation's oldest one.
func insertKey(ctx context.Context, db querier, k KeyInfo, hash []byte) (KeyInfo, error) {
	if k.Kind == "" {
		k.Kind = policy.KeyStandard
	}
	err := db.QueryRow(ctx, `INSERT INTO api_keys
		(id, org_id, project_id, user_id, name, key_hash, prefix, kind, expires_at)
		VALUES ($1, $2,
			COALESCE($3, (`+oldestProject("$2")+`)),
			$4, $5, $6, $7, $8, $9)
		RETURNING project_id, created_at`,
		k.ID, k.OrgID, nullable(k.ProjectID), nullable(k.UserID), k.Name, hash, k.Prefix,
		string(k.Kind), k.ExpiresAt,
	).Scan(&k.ProjectID, &k.CreatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.ConstraintName == "api_keys_project_check" {
		return k, ErrNoProject
	}
	return k, err
}

// RevokeKey marks a key unusable. It is kept rather than deleted so its usage
// history keeps resolving.
func (s *Store) RevokeKey(ctx context.Context, id string) error {
	return s.execOne(ctx,
		"UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL", id)
}

// RenameKey changes what a key is called and returns the name it had. Revoked
// keys can be renamed too: their name still heads their usage history.
func (s *Store) RenameKey(ctx context.Context, id, name string) (old string, err error) {
	err = s.pool.QueryRow(ctx, `UPDATE api_keys k SET name = $2
		FROM api_keys was WHERE k.id = $1 AND was.id = k.id
		RETURNING was.name`, id, name).Scan(&old)
	return old, notFound(err)
}

// ErrKeyRevoked is a key that was already revoked, so there is nothing to
// rotate.
var ErrKeyRevoked = errors.New("store: the key was already revoked")

// RotateKey replaces the key oldID with next, in one transaction: next gets
// the old key's organisation, project, person, kind and own guardrails, and the old key
// is revoked. An empty next.Name keeps the old name. A nil next.ExpiresAt
// gives the new key the old key's lifetime, counted from now.
//
// Doing it in steps can leave both keys live, or a new key without the old
// one's limits, which quietly loosens a guardrail.
func (s *Store) RotateKey(ctx context.Context, oldID string, next KeyInfo, hash []byte) (KeyInfo, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return KeyInfo{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var old KeyInfo
	err = tx.QueryRow(ctx, `SELECT org_id, COALESCE(project_id,''), COALESCE(user_id,''), name,
		kind, created_at, expires_at, revoked_at FROM api_keys WHERE id = $1 FOR UPDATE`, oldID,
	).Scan(&old.OrgID, &old.ProjectID, &old.UserID, &old.Name, &old.Kind, &old.CreatedAt,
		&old.ExpiresAt, &old.RevokedAt)
	if err != nil {
		return KeyInfo{}, notFound(err)
	}
	if old.RevokedAt != nil {
		return KeyInfo{}, ErrKeyRevoked
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
		return KeyInfo{}, err
	}
	if _, err := tx.Exec(ctx, copyKeyGuardrailSQL, oldID, next.ID); err != nil {
		return KeyInfo{}, err
	}
	if _, err := tx.Exec(ctx,
		"UPDATE api_keys SET revoked_at = now() WHERE id = $1", oldID); err != nil {
		return KeyInfo{}, err
	}
	return next, tx.Commit(ctx)
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
		       k.expires_at, COALESCE(k.revoked_at, u.disabled_at), p.scope_type, `+limitColumns+`
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
		found                bool
	)
	for rows.Next() {
		var (
			scopeType *string
			lim       policy.Limits
			period    *string
		)
		dest := append([]any{&key.ID, &key.OrgID, &key.ProjectID, &key.UserID, &key.Kind,
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
	}
	return policy.Resolve(key, orgL, projectL, ownL), nil
}
