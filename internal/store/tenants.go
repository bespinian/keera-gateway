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
	// Limited is set on a self-signed-up organisation until it pays once or an
	// operator lifts it. Until then its models may not run on the deployment's
	// own machines, and it gets no sandboxes.
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

// defaultProjectName names a new organisation's first project.
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
// column prefixed by table ("u." or ""), since joined tables share column names.
func userColumnsOf(table string) string {
	return fmt.Sprintf(`%[1]sid, %[1]sorg_id, %[1]semail, %[1]sname, COALESCE(%[1]sexternal_id,''),
	%[1]srole, %[1]screated_at, %[1]sdisabled_at`, table)
}

// userColumns is the select list scanUser reads.
var userColumns = userColumnsOf("")

// userTargets points at the fields userColumnsOf fills, in its order.
func userTargets(u *User) []any {
	return []any{
		&u.ID, &u.OrgID, &u.Email, &u.Name, &u.ExternalID, &u.Role, &u.CreatedAt,
		&u.DisabledAt,
	}
}

func scanUser(r row) (User, error) {
	var u User
	err := r.Scan(userTargets(&u)...)
	return u, err
}

// KeyInfo is an issued key without its secret. The secret is shown once and
// never stored, so screens, reports and audit entries use the key's name.
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
	return []any{
		&k.ID, &k.OrgID, &k.ProjectID, &k.UserID, &k.Name, &k.Prefix, &k.Kind,
		&k.CreatedAt, &k.ExpiresAt, &k.RevokedAt, &k.Pending,
	}
}

// State is the word a screen shows for a key at now: revoked, expired,
// pending or active. Revoked wins over expired, because it was done on purpose.
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

// CreateOrg inserts an organisation with its email domain and template in one
// transaction. The caller cleans the domain with policy.CleanEmailDomain.
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
	// Set the domain in the same insert, so a taken one creates nothing and a
	// retry cannot create the organisation twice.
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

// UpdateOrg changes an organisation's name, email domain and limited flag in
// one statement, so a refused domain changes nothing. The old name stays in
// the audit log.
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

// LimitedOrgs maps each limited organisation to its name, so the gateway can
// restrict their models and name the owner when it refuses one.
func (s *Store) LimitedOrgs(ctx context.Context) (map[string]string, error) {
	return s.names(ctx, "SELECT id, name FROM orgs WHERE limited")
}

// DeletedOrg is what a deletion removed, counted in the same transaction.
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
// hold machines, which would then keep running with nothing to stop them.
var ErrOrgHasSandboxes = errors.New("store: the organisation still has live sandboxes")

// DeleteOrg removes an organisation and everything scoped to it. Guardrails
// and spend are cleared first, while the ids that name them still exist; the
// foreign keys remove the rest.
//
// The request log, billing lines, payments, credit account and audit log stay,
// for invoicing and as the record of this deletion. The saved card goes, so
// nobody can charge it again.
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

		scoped := func(owner string) string {
			return `(scope_type = 'org'     AND ` + owner + ` = $1)
			OR (scope_type = 'project' AND ` + owner + ` IN (SELECT id FROM projects WHERE org_id = $1))
			OR (scope_type = 'key'     AND ` + owner + ` IN (SELECT id FROM api_keys WHERE org_id = $1))`
		}
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

// scopeOwner is the scope a spend row belongs to. A rate limit per day or
// month counts under the scope's id with the meter after a '|'; see
// policy.Resolved.Quotas.
const scopeOwner = "split_part(scope_id, '|', 1)"

// deleteScoped clears the guardrails and spend rows that match where. These
// tables name their scope by plain id with no foreign key, so no cascade
// reaches them. Only spend ids carry a meter.
func deleteScoped(ctx context.Context, tx pgx.Tx, where func(owner string) string, id string) error {
	for table, owner := range map[string]string{"guardrails": "scope_id", "spend": scopeOwner} {
		if _, err := tx.Exec(ctx, "DELETE FROM "+table+" WHERE "+where(owner), id); err != nil {
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
// the name.
var ErrProjectNameTaken = errors.New("store: a project of that name already exists in this organisation")

// ProjectChange is what UpdateProject changes. A nil field is left as it is.
type ProjectChange struct {
	Name        *string
	Description *string
}

// AnyOrg is the orgID for a write that may touch any organisation, by the
// operator or the gateway itself. Other callers pass their own organisation,
// and a row outside it is ErrNotFound: a second check behind the control
// plane's.
const AnyOrg = "*"

// inOrg is the condition that holds a write to the organisation in param, such
// as "$2". column is the row's organisation column.
func inOrg(column, param string) string {
	return "(" + param + " = '" + AnyOrg + "' OR " + column + " = " + param + ")"
}

// UpdateProject renames a project, changes its description, or both. The old
// name stays in the audit log.
func (s *Store) UpdateProject(ctx context.Context, orgID, projectID string, c ProjectChange) (Project, error) {
	var p Project
	err := s.pool.QueryRow(ctx,
		`UPDATE projects SET
			name = COALESCE($2, name),
			description = COALESCE($3, description)
		WHERE id = $1 AND `+inOrg("org_id", "$4")+`
		RETURNING id, org_id, name, description, created_at`,
		projectID, c.Name, c.Description, orgID,
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

// oldestProject selects the oldest project of the organisation in orgParam,
// such as "$1". Keys and sandboxes without a project go there.
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

// DeleteProject removes a project that no live key uses, since a live key
// would move to another project's guardrails, budget and rate limit. The check
// runs on the row locked FOR UPDATE, so a key issued meanwhile cannot slip by.
//
// Revoked keys are detached, not deleted, because usage rows still name them.
// Guardrails and spend have no foreign key, so they are cleared here.
func (s *Store) DeleteProject(ctx context.Context, orgID, projectID string) (DeletedProject, error) {
	var gone DeletedProject
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			"SELECT org_id, name FROM projects WHERE id = $1 AND "+inOrg("org_id", "$2")+" FOR UPDATE",
			projectID, orgID,
		).Scan(&gone.OrgID, &gone.Name)
		if err != nil {
			return notFound(err)
		}
		gone.ID = projectID

		// The row lock above holds off a new sandbox: its insert checks the
		// foreign key, which waits for the lock.
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

		project := func(owner string) string { return "scope_type = 'project' AND " + owner + " = $1" }
		if err := deleteScoped(ctx, tx, project, projectID); err != nil {
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

// ErrUserExists means the organisation already has someone with that address.
// Adding never changes an existing person: the role changes through
// SetUserRole, and the subject only at sign-in.
var ErrUserExists = errors.New("store: that address is already in the organisation")

// ErrExternalIDTaken means someone in the organisation already has the subject.
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

// DisableUser revokes a person's keys, ends their sessions and removes their
// passkeys in one transaction, and returns how many keys it revoked.
// Disabling twice keeps the first date but still revokes newer keys.
func (s *Store) DisableUser(ctx context.Context, orgID, userID string) (int, error) {
	var revoked int
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			"UPDATE users SET disabled_at = COALESCE(disabled_at, now()) WHERE id = $1 AND "+
				inOrg("org_id", "$2"), userID, orgID)
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
		// Like keys, passkeys do not come back on enable: a new set-up link does.
		return deletePasskeys(ctx, tx, userID)
	})
	if err != nil {
		return 0, err
	}
	return revoked, nil
}

// EnableUser lets a disabled person sign in again. Their old keys stay
// revoked: they get new ones.
func (s *Store) EnableUser(ctx context.Context, orgID, userID string) error {
	return s.execOne(ctx, "UPDATE users SET disabled_at = NULL WHERE id = $1 AND "+inOrg("org_id", "$2"),
		userID, orgID)
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
func (s *Store) RevokeKey(ctx context.Context, orgID, id string) error {
	var found, revoked bool
	err := s.pool.QueryRow(ctx, `WITH revoked AS (
			UPDATE api_keys SET revoked_at = now()
			WHERE id = $1 AND revoked_at IS NULL AND `+inOrg("org_id", "$2")+` RETURNING id)
		SELECT EXISTS (SELECT 1 FROM api_keys WHERE id = $1 AND `+inOrg("org_id", "$2")+`),
			EXISTS (SELECT 1 FROM revoked)`,
		id, orgID).Scan(&found, &revoked)
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
func (s *Store) RenameKey(ctx context.Context, orgID, id, name string) (old string, err error) {
	err = s.pool.QueryRow(ctx, `UPDATE api_keys k SET name = $2
		FROM api_keys was WHERE k.id = $1 AND was.id = k.id AND `+inOrg("k.org_id", "$3")+`
		RETURNING was.name`, id, name, orgID).Scan(&old)
	return old, notFound(err)
}

// ErrKeyActive is a key that was activated already. Its secret was shown
// then, so a second activation would be a rotation in disguise.
var ErrKeyActive = errors.New("store: the key is already activated")

// ActivateKey gives a pending key its secret, and a new name when name is
// not empty. The update only matches a pending key, so two racing
// activations cannot both get a secret.
func (s *Store) ActivateKey(ctx context.Context, orgID, id, name string, hash []byte, prefix string,
) (KeyInfo, error) {
	var k KeyInfo
	err := s.pool.QueryRow(ctx, `UPDATE api_keys
		SET key_hash = $2, prefix = $3, name = COALESCE(NULLIF($4, ''), name)
		WHERE id = $1 AND key_hash IS NULL AND revoked_at IS NULL
			AND (expires_at IS NULL OR expires_at > now()) AND `+inOrg("org_id", "$5")+`
		RETURNING `+keyColumnsOf(""), id, hash, prefix, name, orgID,
	).Scan(keyTargets(&k)...)
	if !errors.Is(err, pgx.ErrNoRows) {
		return k, err
	}
	// Nothing matched: find out why, so the caller can say what to do. An
	// expired pending key would be refused at once, so it counts as expired.
	var pending, revoked, expired bool
	err = s.pool.QueryRow(ctx, `SELECT key_hash IS NULL, revoked_at IS NOT NULL,
		expires_at IS NOT NULL AND expires_at <= now()
		FROM api_keys WHERE id = $1 AND `+inOrg("org_id", "$2"), id, orgID).Scan(&pending, &revoked, &expired)
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

// ErrSandboxKey is a key minted for a sandbox. It lives and dies with the
// sandbox, so it is not rotated.
var ErrSandboxKey = errors.New("store: the key belongs to a sandbox")

// RotateKey replaces the key oldID with next in one transaction: next gets the
// old key's organisation, project, person, kind and guardrails, and the old
// key is revoked. An empty next.Name keeps the old name. A nil next.ExpiresAt
// keeps the old key's lifetime, counted from now.
//
// Doing it in steps could leave both keys live, or the new one without the
// old one's limits.
func (s *Store) RotateKey(ctx context.Context, orgID, oldID string, next KeyInfo, hash []byte,
) (KeyInfo, error) {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var old KeyInfo
		err := tx.QueryRow(ctx, `SELECT `+keyColumnsOf("")+` FROM api_keys
			WHERE id = $1 AND `+inOrg("org_id", "$2")+` FOR UPDATE`, oldID, orgID).Scan(keyTargets(&old)...)
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
		// A sandbox revokes its key by id when it ends, so a rotated copy would
		// outlive it, and could be rotated on for ever.
		var sandboxed bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM sandboxes WHERE key_id = $1)",
			oldID).Scan(&sandboxed); err != nil {
			return err
		}
		if sandboxed {
			return ErrSandboxKey
		}
		next.OrgID, next.ProjectID, next.UserID, next.Kind = old.OrgID, old.ProjectID, old.UserID, old.Kind
		if next.Name == "" {
			next.Name = old.Name
		}
		// Keep the lifetime, not the date: a key rotated a week before it lapses
		// should not get a replacement that lapses in a week.
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

// LookupKey finds a key by the hash the client presented, with the guardrails
// of its whole chain. It is one round trip, as it runs on a cache miss in the
// request path.
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
	// Second checks behind the control plane: the project must be in the key's
	// own org, so a bad row never gives another tenant's guardrails, and a
	// disabled holder counts as a revoked key.
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
		)
		dest := append([]any{
			&key.ID, &key.OrgID, &key.ProjectID, &key.UserID, &key.Kind,
			&expires, &revoked, &pending, &scopeType,
		}, limitTargets(&lim)...)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		found = true
		if scopeType == nil {
			continue
		}
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
