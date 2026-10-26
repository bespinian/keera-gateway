package store

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// The sandbox tables hold each organisation's classes, which work like its
// models, and a log of the machines lent out. A sandbox row outlives its
// machine for cost and reports; usage retention deletes it.

// liveSandbox is the condition for a sandbox that still holds resources, as
// in Sandbox.Live. Expired sandboxes and failed engineer sandboxes count: they
// keep their volume.
const liveSandbox = "(state IN ('pending','ready','suspended','expired') " +
	"OR (state = 'failed' AND purpose = 'engineer'))"

const sandboxClassColumns = `SELECT org_id, name, description, image, isolation,
	cpu_millis, memory_mib, disk_mib, default_ttl_seconds, max_ttl_seconds, warm,
	purposes, created_at, updated_at`

func scanSandboxClass(r row) (policy.SandboxClass, error) {
	var (
		c        policy.SandboxClass
		purposes []string
	)
	if err := r.Scan(&c.OrgID, &c.Name, &c.Description, &c.Image, &c.Isolation,
		&c.CPU, &c.Memory, &c.Disk, &c.DefaultTTLSeconds, &c.MaxTTLSeconds, &c.Warm,
		&purposes, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return policy.SandboxClass{}, err
	}
	for _, p := range purposes {
		c.Purposes = append(c.Purposes, policy.Purpose(p))
	}
	return c, nil
}

// WarmSandboxClasses reads the classes that should have a warm pool, across
// every organisation. A limited organisation has none, because a pool holds
// the deployment's machines even when unused. A class only agents use has
// none either, since only engineers claim from a pool.
func (s *Store) WarmSandboxClasses(ctx context.Context) ([]policy.SandboxClass, error) {
	return queryAll(ctx, s.pool, scanSandboxClass, sandboxClassColumns+` FROM sandbox_classes
		WHERE warm > 0 AND org_id IN (SELECT id FROM orgs WHERE NOT limited)
			AND (purposes = '{}' OR 'engineer' = ANY(purposes))
		ORDER BY org_id, name`)
}

func (s *Store) ListSandboxClasses(ctx context.Context, orgID string) ([]policy.SandboxClass, error) {
	return queryAll(ctx, s.pool, scanSandboxClass,
		sandboxClassColumns+" FROM sandbox_classes WHERE org_id = $1 ORDER BY name", orgID)
}

func (s *Store) SandboxClass(ctx context.Context, orgID, name string) (policy.SandboxClass, error) {
	c, err := scanSandboxClass(s.pool.QueryRow(ctx,
		sandboxClassColumns+" FROM sandbox_classes WHERE org_id = $1 AND name = $2", orgID, name))
	return c, notFound(err)
}

// UpsertSandboxClass creates or replaces one of an organisation's classes, and
// writes the saved timestamps back onto c.
func (s *Store) UpsertSandboxClass(ctx context.Context, c *policy.SandboxClass) error {
	return upsertSandboxClass(ctx, s.pool, c)
}

func upsertSandboxClass(ctx context.Context, db querier, c *policy.SandboxClass) error {
	purposes := make([]string, 0, len(c.Purposes))
	for _, p := range c.Purposes {
		purposes = append(purposes, string(p))
	}
	return db.QueryRow(ctx, `INSERT INTO sandbox_classes
		(org_id, name, description, image, isolation, cpu_millis, memory_mib,
		 disk_mib, default_ttl_seconds, max_ttl_seconds, warm, purposes, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12, now())
		ON CONFLICT (org_id, name) DO UPDATE SET description = EXCLUDED.description,
			image = EXCLUDED.image, isolation = EXCLUDED.isolation,
			cpu_millis = EXCLUDED.cpu_millis,
			memory_mib = EXCLUDED.memory_mib, disk_mib = EXCLUDED.disk_mib,
			default_ttl_seconds = EXCLUDED.default_ttl_seconds,
			max_ttl_seconds = EXCLUDED.max_ttl_seconds, warm = EXCLUDED.warm,
			purposes = EXCLUDED.purposes, updated_at = now()
		RETURNING created_at, updated_at`,
		c.OrgID, c.Name, c.Description, c.Image, string(c.Isolation),
		c.CPU, c.Memory, c.Disk,
		c.DefaultTTLSeconds, c.MaxTTLSeconds, c.Warm,
		purposes,
	).Scan(&c.CreatedAt, &c.UpdatedAt)
}

func (s *Store) DeleteSandboxClass(ctx context.Context, orgID, name string) error {
	return s.execOne(ctx, "DELETE FROM sandbox_classes WHERE org_id = $1 AND name = $2", orgID, name)
}

// SandboxClassInUse counts the live sandboxes of a class, so a deletion can
// warn. Running sandboxes keep their own copy of the class, so deleting it
// breaks nothing.
func (s *Store) SandboxClassInUse(ctx context.Context, orgID, name string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM sandboxes WHERE org_id = $1 AND class = $2 AND "+liveSandbox,
		orgID, name).Scan(&n)
	return n, err
}

// Sandbox is one machine that was lent out.
type Sandbox struct {
	ID        string `json:"id"`
	OrgID     string `json:"org_id"`
	ProjectID string `json:"project_id,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	// Owner is the address the sandbox was created for, copied so it survives
	// the person being deleted.
	Owner   string              `json:"owner,omitempty"`
	Name    string              `json:"name"`
	Class   string              `json:"class"`
	Purpose policy.Purpose      `json:"purpose"`
	State   policy.SandboxState `json:"state"`
	Detail  string              `json:"detail,omitempty"`

	// The machine as it ran, copied from the class at creation, so the bill
	// stays right when the class changes.
	Image     string           `json:"image,omitempty"`
	Isolation policy.Isolation `json:"isolation,omitempty"`
	CPU       int              `json:"cpu_millis"`
	Memory    int              `json:"memory_mib"`
	Disk      int              `json:"disk_mib"`

	// KeyID is the API key minted for this sandbox. Its secret went only into
	// the sandbox's environment.
	KeyID string `json:"key_id,omitempty"`
	// SessionKey is the session of an agent sandbox's requests.
	SessionKey string `json:"session_key,omitempty"`

	Repo   string `json:"repo,omitempty"`
	Branch string `json:"branch,omitempty"`
	// AuthorizedKeys are the SSH public keys that may open a shell in it.
	AuthorizedKeys []string `json:"-"`
	// GitCredentialID is the forge's id for the sandbox's repository
	// credential, to revoke it when the sandbox ends. The credential itself is
	// not stored.
	GitCredentialID string `json:"-"`
	Node            string `json:"node,omitempty"`
	Address         string `json:"-"`
	// Backing is what holds this sandbox: an object the driver created, or a
	// claim on a warm pool.
	Backing string `json:"backing,omitempty"`

	CreatedAt time.Time  `json:"created_at"`
	ReadyAt   *time.Time `json:"ready_at,omitempty"`
	// LastActiveAt is when somebody last had a connection open, or when it last
	// became ready. Idle suspension counts from here.
	LastActiveAt *time.Time `json:"last_active_at,omitempty"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	SuspendedAt  *time.Time `json:"suspended_at,omitempty"`
	TerminatedAt *time.Time `json:"terminated_at,omitempty"`

	// RunningSeconds is how long this sandbox has held compute.
	RunningSeconds int64      `json:"running_seconds"`
	AccountedAt    *time.Time `json:"-"`
}

// Live reports whether this sandbox still holds resources. A failed engineer
// sandbox does, because its volume is kept in case it holds work.
func (s Sandbox) Live() bool {
	return s.State.Live() || (s.State == policy.SandboxFailed && s.Purpose == policy.PurposeEngineer)
}

// CoreSeconds is running time weighted by the machine's size, for chargeback.
func (s Sandbox) CoreSeconds() int64 {
	return s.RunningSeconds * int64(s.CPU) / 1000
}

const sandboxColumns = `SELECT id, org_id, COALESCE(project_id,''), COALESCE(user_id,''), owner,
	name, class, purpose, state, detail, image, isolation, cpu_millis, memory_mib, disk_mib,
	COALESCE(key_id,''), COALESCE(session_key,''), repo, branch, authorized_keys, git_credential_id,
	node, address, backing, created_at, ready_at, active_at, expires_at, suspended_at, terminated_at, running_seconds, accounted_at`

func scanSandbox(r row) (Sandbox, error) {
	var sb Sandbox
	err := r.Scan(&sb.ID, &sb.OrgID, &sb.ProjectID, &sb.UserID, &sb.Owner,
		&sb.Name, &sb.Class, &sb.Purpose, &sb.State, &sb.Detail,
		&sb.Image, &sb.Isolation, &sb.CPU, &sb.Memory, &sb.Disk,
		&sb.KeyID, &sb.SessionKey, &sb.Repo, &sb.Branch, &sb.AuthorizedKeys, &sb.GitCredentialID,
		&sb.Node, &sb.Address, &sb.Backing,
		&sb.CreatedAt, &sb.ReadyAt, &sb.LastActiveAt, &sb.ExpiresAt, &sb.SuspendedAt, &sb.TerminatedAt,
		&sb.RunningSeconds, &sb.AccountedAt)
	return sb, err
}

func (s *Store) sandboxWhere(ctx context.Context, rest string, args ...any) (Sandbox, error) {
	sb, err := scanSandbox(s.pool.QueryRow(ctx, sandboxColumns+" FROM sandboxes "+rest, args...))
	return sb, notFound(err)
}

func (s *Store) sandboxesWhere(ctx context.Context, rest string, args ...any) ([]Sandbox, error) {
	return queryAll(ctx, s.pool, scanSandbox, sandboxColumns+" FROM sandboxes "+rest, args...)
}

// CreateSandbox inserts a sandbox. A live sandbox with the same name gives
// ErrSandboxNameTaken; other collisions are too rare for their own error.
func (s *Store) CreateSandbox(ctx context.Context, sb Sandbox) (Sandbox, error) {
	err := s.pool.QueryRow(ctx, `INSERT INTO sandboxes
		(id, org_id, project_id, user_id, owner, name, class, purpose, state, detail,
		 image, isolation, cpu_millis, memory_mib, disk_mib, key_id, session_key,
		 repo, branch, authorized_keys, git_credential_id, backing, expires_at, accounted_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,
		        $23, now())
		RETURNING created_at, accounted_at`,
		sb.ID, sb.OrgID, nullable(sb.ProjectID), nullable(sb.UserID), sb.Owner,
		sb.Name, sb.Class, string(sb.Purpose), string(sb.State), sb.Detail,
		sb.Image, string(sb.Isolation), sb.CPU, sb.Memory, sb.Disk,
		nullable(sb.KeyID), nullable(sb.SessionKey), sb.Repo, sb.Branch,
		// Never nil, which pgx would send as NULL.
		append([]string{}, sb.AuthorizedKeys...), sb.GitCredentialID,
		sb.Backing, sb.ExpiresAt,
	).Scan(&sb.CreatedAt, &sb.AccountedAt)
	if isUnique(err) {
		return sb, ErrSandboxNameTaken
	}
	return sb, err
}

// ErrSandboxNameTaken means a live sandbox of the same person already has the
// name. The name is free again once that sandbox is gone.
var ErrSandboxNameTaken = errors.New("store: this person already has a live sandbox of that name")

func (s *Store) Sandbox(ctx context.Context, id string) (Sandbox, error) {
	return s.sandboxWhere(ctx, "WHERE id = $1", id)
}

// LiveSandboxesByName finds the live sandboxes of that name in an
// organisation, for `keera sandbox ssh <name>`. A name is unique per person,
// so there can be one for each.
func (s *Store) LiveSandboxesByName(ctx context.Context, orgID, name string) ([]Sandbox, error) {
	return s.sandboxesWhere(ctx, "WHERE org_id = $1 AND name = $2 AND "+liveSandbox+
		" ORDER BY created_at", orgID, name)
}

// LiveSandboxByKey finds the live sandbox a working key was minted for, so a
// sandbox can get a fresh repository credential with only its own key.
func (s *Store) LiveSandboxByKey(ctx context.Context, hash []byte) (Sandbox, error) {
	return s.sandboxWhere(ctx, `WHERE key_id = (
			SELECT k.id FROM api_keys k LEFT JOIN users u ON u.id = k.user_id
			WHERE k.key_hash = $1 AND k.revoked_at IS NULL AND u.disabled_at IS NULL
			  AND (k.expires_at IS NULL OR k.expires_at > now()))
		AND `+liveSandbox, hash)
}

// SetSandboxGitCredential records the id of the repository credential a
// sandbox now holds.
func (s *Store) SetSandboxGitCredential(ctx context.Context, id, credentialID string) error {
	return s.execOne(ctx, "UPDATE sandboxes SET git_credential_id = $2 WHERE id = $1",
		id, credentialID)
}

// SandboxQuery narrows the list. The zero value is every live sandbox.
type SandboxQuery struct {
	// OrgID empty lists every organisation's, for an operator.
	OrgID     string
	ProjectID string
	UserID    string
	Class     string
	Purpose   policy.Purpose
	// All includes finished sandboxes.
	All   bool
	Limit int
}

func (s *Store) ListSandboxes(ctx context.Context, q SandboxQuery) ([]Sandbox, error) {
	limit := pageLimit(q.Limit, 200, 500)
	return s.sandboxesWhere(ctx, `
		WHERE ($1 = '' OR org_id = $1)
		  AND ($2 = '' OR project_id = $2)
		  AND ($3 = '' OR user_id = $3)
		  AND ($4 = '' OR class = $4)
		  AND ($5 = '' OR purpose = $5)
		  AND ($6 OR `+liveSandbox+`)
		ORDER BY created_at DESC
		LIMIT $7`,
		q.OrgID, q.ProjectID, q.UserID, q.Class, string(q.Purpose), q.All, limit)
}

// SandboxObservation is what the driver saw, written back onto the row all at
// once, so a row never disagrees with itself, such as a ready sandbox with no
// address. It has no expiry: the row owns that.
type SandboxObservation struct {
	State   policy.SandboxState
	Detail  string
	Address string
	Node    string
}

// ObserveSandbox writes back what the driver saw.
//
// ready_at is set only the first time a sandbox is ready. active_at is set each
// time it becomes ready, so a resumed sandbox does not count as idle since
// before its suspension. suspended_at is reset on each suspend.
//
// A row that is gone or terminated is left alone without an error: usually
// somebody ended the sandbox after the sweep read it.
func (s *Store) ObserveSandbox(ctx context.Context, id string, obs SandboxObservation) error {
	_, err := s.observeSandbox(ctx, id, sandboxGuard{}, obs)
	return err
}

// ObserveSandboxFrom is ObserveSandbox for a row read in state from. A row
// that has changed state since is left alone, so an out-of-date observation
// cannot undo somebody's suspend, resume or end.
func (s *Store) ObserveSandboxFrom(ctx context.Context, id string, from policy.SandboxState,
	obs SandboxObservation,
) error {
	_, err := s.observeSandbox(ctx, id, sandboxGuard{from: from}, obs)
	return err
}

// sandboxGuard is what a row must still be for an observation to be written.
// Zero fields are not checked.
type sandboxGuard struct {
	from policy.SandboxState
	// expiresAt is the end the writer read, on a row not yet expired.
	expiresAt *time.Time
}

// observeSandbox is ObserveSandbox, held to guard. It returns the rows changed.
func (s *Store) observeSandbox(ctx context.Context, id string, guard sandboxGuard,
	obs SandboxObservation,
) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE sandboxes SET
			state = $2,
			detail = $3,
			address = $4,
			node = $5,
			ready_at = CASE WHEN ready_at IS NULL AND $2 = 'ready' THEN now() ELSE ready_at END,
			active_at = CASE WHEN $2 = 'ready' AND state <> 'ready' THEN now() ELSE active_at END,
			suspended_at = CASE WHEN $2 = 'suspended' THEN COALESCE(suspended_at, now())
			                    ELSE NULL END,
			terminated_at = CASE WHEN $2 = 'terminated' THEN COALESCE(terminated_at, now())
			                     ELSE terminated_at END,
			-- Coming back to compute starts the clock from now, so the time it
			-- spent suspended or expired is not charged.
			accounted_at = CASE WHEN $2 IN ('pending','ready') AND state NOT IN ('pending','ready')
			                    THEN now() ELSE accounted_at END
		WHERE id = $1 AND state <> 'terminated' AND ($6 = '' OR state = $6)
		  AND ($7::timestamptz IS NULL OR
		       (expires_at = $7 AND state IN ('pending','ready','suspended')))`,
		id, string(obs.State), obs.Detail, obs.Address, obs.Node, string(guard.from), guard.expiresAt)
	return tag.RowsAffected(), err
}

// ReviveSandbox gives an expired sandbox what it needs to start again: a new
// key, a new repository credential and a new end. It is ErrNotFound unless
// the sandbox is still expired, so two resumes cannot both win.
func (s *Store) ReviveSandbox(ctx context.Context, id, keyID, gitCredentialID string,
	expires time.Time,
) error {
	return s.execOne(ctx, `UPDATE sandboxes SET
			key_id = $2, git_credential_id = $3, expires_at = $4,
			state = 'pending', detail = 'resuming after it expired',
			suspended_at = NULL, accounted_at = now()
		WHERE id = $1 AND state = 'expired'`,
		id, keyID, gitCredentialID, expires)
}

// TouchSandbox records that somebody is connected to a sandbox, so idle
// suspension leaves it running.
func (s *Store) TouchSandbox(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx,
		"UPDATE sandboxes SET active_at = now() WHERE id = $1 AND state = 'ready'", id)
	return err
}

// SetSandboxExpiry moves when a sandbox ends, and its key's expiry with it, so
// the sandbox keeps inference access until the new time.
//
// An expired sandbox is ErrNotFound, like a terminated one: only a resume
// gives it a new end. See ExpireSandbox for the race this settles.
func (s *Store) SetSandboxExpiry(ctx context.Context, id string, at time.Time) error {
	var n int
	err := s.pool.QueryRow(ctx, `
		WITH sb AS (
			UPDATE sandboxes SET expires_at = $2
			WHERE id = $1 AND state NOT IN ('terminated','expired')
			RETURNING key_id
		), k AS (
			UPDATE api_keys SET expires_at = $2
			WHERE id = (SELECT key_id FROM sb) AND revoked_at IS NULL
		)
		SELECT count(*) FROM sb`, id, at).Scan(&n)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountLiveSandboxes counts the live sandboxes of an organisation or project,
// for the quota.
func (s *Store) CountLiveSandboxes(ctx context.Context, scopeType policy.ScopeType, scopeID string) (int, error) {
	column := "org_id"
	if scopeType == policy.ScopeProject {
		column = "project_id"
	}
	var n int
	err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM sandboxes WHERE "+column+" = $1 AND "+liveSandbox, scopeID).Scan(&n)
	return n, err
}

// SandboxesPastExpiry reads the live sandboxes whose time is up, for the sweep,
// leaving out those already expired. The states are spelled out to match the
// partial index.
func (s *Store) SandboxesPastExpiry(ctx context.Context, now time.Time, limit int) ([]Sandbox, error) {
	if limit <= 0 {
		limit = 100
	}
	return s.sandboxesWhere(ctx, `
		WHERE expires_at IS NOT NULL AND expires_at <= $1
		  AND state IN ('pending','ready','suspended')
		ORDER BY expires_at
		LIMIT $2`, now, limit)
}

// ExpireSandbox marks sb expired if its end is still the one the sweep read,
// and is ErrNotFound otherwise. SetSandboxExpiry refuses an expired row, so
// of an extension and an expiry exactly one wins.
func (s *Store) ExpireSandbox(ctx context.Context, sb Sandbox, detail string) error {
	if sb.ExpiresAt == nil {
		return ErrNotFound
	}
	n, err := s.observeSandbox(ctx, sb.ID, sandboxGuard{expiresAt: sb.ExpiresAt},
		SandboxObservation{State: policy.SandboxExpired, Detail: detail})
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}

// LiveSandboxes reads up to limit sandboxes the drivers should still know
// about, in id order after the id after, so the sweep can page through them.
func (s *Store) LiveSandboxes(ctx context.Context, after string, limit int) ([]Sandbox, error) {
	if limit <= 0 {
		limit = 500
	}
	return s.sandboxesWhere(ctx, "WHERE "+liveSandbox+" AND id > $1 ORDER BY id LIMIT $2",
		after, limit)
}

// accrueRunning adds the time since accounted_at to running_seconds, as of $1,
// for sandboxes holding compute. It stops at expires_at, even if no sweep has
// ended the sandbox yet.
const accrueRunning = `UPDATE sandboxes SET
		running_seconds = running_seconds +
			GREATEST(0, EXTRACT(EPOCH FROM (LEAST($1::timestamptz, COALESCE(expires_at, $1::timestamptz))
				- COALESCE(accounted_at, created_at)))::bigint),
		accounted_at = $1
	WHERE state IN ('pending','ready')`

// AccountSandboxes brings running_seconds up to date for every sandbox holding
// compute, and returns how many rows it touched. It is one statement, so it is
// safe across replicas. Time a gateway was down is still charged, up to expiry.
func (s *Store) AccountSandboxes(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, accrueRunning, now)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// StopAccounting freezes a sandbox's clock when it stops holding compute, so
// the time until the next sweep is not charged.
func (s *Store) StopAccounting(ctx context.Context, id string, now time.Time) error {
	_, err := s.pool.Exec(ctx, accrueRunning+" AND id = $2", now, id)
	return err
}

// SandboxUsage is what one group of sandboxes came to over a window.
type SandboxUsage struct {
	// Group is a project id, a user's address or a class name.
	Group string `json:"group"`
	// OrgID is set only when a report across every organisation groups by
	// class, since two organisations can have a class of the same name.
	OrgID string `json:"org_id,omitempty"`
	// Count is how many sandboxes, Running the seconds they held compute, and
	// CoreSeconds the same weighted by machine size.
	Count       int64 `json:"count"`
	Running     int64 `json:"running_seconds"`
	CoreSeconds int64 `json:"core_seconds"`
	// Live is how many of them still hold resources now.
	Live int64 `json:"live"`
}

// sandboxGroupColumns maps group_by values to columns, like groupColumns.
var sandboxGroupColumns = map[string]string{
	"class":   "s.class",
	"project": "COALESCE(s.project_id, '')",
	"user":    "COALESCE(NULLIF(s.owner, ''), COALESCE(s.user_id, ''))",
}

// ValidSandboxGroupBy reports whether s is a grouping SandboxUsageBy
// understands. The empty string is not one: the handler picks the default.
func ValidSandboxGroupBy(s string) bool {
	_, ok := sandboxGroupColumns[s]
	return ok
}

// SandboxGroupBys lists the groupings, for the error a refused one earns.
func SandboxGroupBys() []string {
	return slices.Sorted(maps.Keys(sandboxGroupColumns))
}

// SandboxUsageBy groups sandbox time over a window by "class", "project" or
// "user". A sandbox counts whole in the window it was created in.
func (s *Store) SandboxUsageBy(ctx context.Context, orgID, groupBy string, from, to time.Time) (
	[]SandboxUsage, error,
) {
	group, ok := sandboxGroupColumns[groupBy]
	if !ok {
		group = sandboxGroupColumns["class"]
	}
	org, by := "''", "1"
	if orgID == "" && group == sandboxGroupColumns["class"] {
		org, by = "s.org_id", "1, 2"
	}
	return queryAll(ctx, s.pool, scanSandboxUsage, `
		SELECT `+group+` AS k, `+org+`,
		       count(*),
		       COALESCE(sum(s.running_seconds), 0),
		       COALESCE(sum(s.running_seconds * s.cpu_millis / 1000), 0),
		       count(*) FILTER (WHERE `+liveSandbox+`)
		FROM sandboxes s
		WHERE s.created_at >= $1 AND s.created_at < $2 AND ($3 = '' OR s.org_id = $3)
		GROUP BY `+by+`
		ORDER BY 4 DESC`, from, to, orgID)
}

func scanSandboxUsage(r row) (SandboxUsage, error) {
	var u SandboxUsage
	err := r.Scan(&u.Group, &u.OrgID, &u.Count, &u.Running, &u.CoreSeconds, &u.Live)
	return u, err
}
