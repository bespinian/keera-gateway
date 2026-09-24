package store

import (
	"context"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// The sandbox tables: a catalogue of classes that works like the model
// catalogue, and a log of the machines actually lent out.
//
// A sandbox row is kept after the machine is gone, because its cost, owner and
// repository still matter later. So the log grows, and it has the same
// retention as the usage log.

// liveSandbox is the condition for a sandbox that still holds resources. It
// matches policy.SandboxState.Live.
const liveSandbox = "state IN ('pending','ready','suspended')"

const sandboxClassColumns = `SELECT name, description, image, isolation, runtime_class,
	cpu_millis, memory_mib, disk_mib, default_ttl_seconds, max_ttl_seconds, warm,
	egress, purposes, managed, created_at, updated_at`

func scanSandboxClass(r row) (policy.SandboxClass, error) {
	var (
		c              policy.SandboxClass
		defTTL, maxTTL int
		purposes       []string
	)
	if err := r.Scan(&c.Name, &c.Description, &c.Image, &c.Isolation, &c.RuntimeClass,
		&c.CPU, &c.Memory, &c.Disk, &defTTL, &maxTTL, &c.Warm,
		&c.Egress, &purposes, &c.Managed, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return policy.SandboxClass{}, err
	}
	c.DefaultTTL = time.Duration(defTTL) * time.Second
	c.MaxTTL = time.Duration(maxTTL) * time.Second
	for _, p := range purposes {
		c.Purposes = append(c.Purposes, policy.Purpose(p))
	}
	return c, nil
}

// ListSandboxClasses reads the whole catalogue. Unlike models it is not cached,
// because only the control API reads it, and that already talks to Postgres.
func (s *Store) ListSandboxClasses(ctx context.Context) ([]policy.SandboxClass, error) {
	rows, err := s.pool.Query(ctx, sandboxClassColumns+" FROM sandbox_classes ORDER BY name")
	if err != nil {
		return nil, err
	}
	return collect(rows, scanSandboxClass)
}

// SandboxClass reads one entry, or ErrNotFound.
func (s *Store) SandboxClass(ctx context.Context, name string) (policy.SandboxClass, error) {
	c, err := scanSandboxClass(s.pool.QueryRow(ctx,
		sandboxClassColumns+" FROM sandbox_classes WHERE name = $1", name))
	if err != nil {
		return policy.SandboxClass{}, notFound(err)
	}
	return c, nil
}

// UpsertSandboxClass creates or replaces one entry. It writes the saved
// timestamps back onto c, so the control plane can answer a PUT with what was
// stored.
func (s *Store) UpsertSandboxClass(ctx context.Context, c *policy.SandboxClass) error {
	purposes := make([]string, 0, len(c.Purposes))
	for _, p := range c.Purposes {
		purposes = append(purposes, string(p))
	}
	egress := c.Egress
	if egress == nil {
		egress = []string{}
	}
	return s.pool.QueryRow(ctx, `INSERT INTO sandbox_classes
		(name, description, image, isolation, runtime_class, cpu_millis, memory_mib, disk_mib,
		 default_ttl_seconds, max_ttl_seconds, warm, egress, purposes, managed, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14, now())
		ON CONFLICT (name) DO UPDATE SET description = EXCLUDED.description,
			image = EXCLUDED.image, isolation = EXCLUDED.isolation,
			runtime_class = EXCLUDED.runtime_class, cpu_millis = EXCLUDED.cpu_millis,
			memory_mib = EXCLUDED.memory_mib, disk_mib = EXCLUDED.disk_mib,
			default_ttl_seconds = EXCLUDED.default_ttl_seconds,
			max_ttl_seconds = EXCLUDED.max_ttl_seconds, warm = EXCLUDED.warm,
			egress = EXCLUDED.egress, purposes = EXCLUDED.purposes,
			managed = EXCLUDED.managed, updated_at = now()
		RETURNING created_at, updated_at`,
		c.Name, c.Description, c.Image, string(c.Isolation), c.RuntimeClass,
		c.CPU, c.Memory, c.Disk,
		int(c.DefaultTTL/time.Second), int(c.MaxTTL/time.Second), c.Warm,
		egress, purposes, c.Managed,
	).Scan(&c.CreatedAt, &c.UpdatedAt)
}

// UnmanageSandboxClasses hands back every class the catalogue file no longer
// names, as UnmanageModels does. The row is kept, because repository
// configurations may still name the class.
func (s *Store) UnmanageSandboxClasses(ctx context.Context, except []string) error {
	_, err := s.pool.Exec(ctx,
		"UPDATE sandbox_classes SET managed = false, updated_at = now() "+
			"WHERE managed AND name <> ALL($1)", except)
	return err
}

// DeleteSandboxClass removes one entry.
func (s *Store) DeleteSandboxClass(ctx context.Context, name string) error {
	return s.execOne(ctx, "DELETE FROM sandbox_classes WHERE name = $1", name)
}

// SandboxClassInUse counts the live sandboxes of one class.
//
// It is read before a deletion. Running sandboxes keep their own copy of the
// class's settings, so deleting it breaks nothing: the control plane warns
// with the count rather than refusing.
func (s *Store) SandboxClassInUse(ctx context.Context, name string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM sandboxes WHERE class = $1 AND "+liveSandbox, name).Scan(&n)
	return n, err
}

// Sandbox is one machine that was lent out.
type Sandbox struct {
	ID     string `json:"id"`
	OrgID  string `json:"org_id"`
	TeamID string `json:"team_id,omitempty"`
	UserID string `json:"user_id,omitempty"`
	// Owner is the address the sandbox was created for. It is copied, not
	// joined, so it survives the person being deleted.
	Owner   string              `json:"owner,omitempty"`
	Name    string              `json:"name"`
	Class   string              `json:"class"`
	Purpose policy.Purpose      `json:"purpose"`
	State   policy.SandboxState `json:"state"`
	Detail  string              `json:"detail,omitempty"`

	// The machine as it actually ran, copied from the class at creation. The
	// class may change later, and the bill must describe the real machine.
	Image     string           `json:"image,omitempty"`
	Isolation policy.Isolation `json:"isolation,omitempty"`
	CPU       int              `json:"cpu_millis"`
	Memory    int              `json:"memory_mib"`
	Disk      int              `json:"disk_mib"`

	// KeyID is the API key minted for this sandbox. Its secret went only into
	// the sandbox's environment.
	KeyID string `json:"key_id,omitempty"`
	// SessionKey is the task an agent sandbox's requests belong to, so the
	// sandbox and its session can be shown together.
	SessionKey string `json:"session_key,omitempty"`

	Repo   string `json:"repo,omitempty"`
	Branch string `json:"branch,omitempty"`
	// GitCredentialID is the forge's id for the repository credential this
	// sandbox holds, so it can be revoked when the sandbox ends. The
	// credential itself is not stored.
	GitCredentialID string `json:"-"`
	Node            string `json:"node,omitempty"`
	Address         string `json:"-"`
	// Backing is what kind of cluster object holds this sandbox: one the driver
	// created, or a claim on a warm pool. See sandbox.Backing.
	Backing string `json:"backing,omitempty"`

	CreatedAt    time.Time  `json:"created_at"`
	ReadyAt      *time.Time `json:"ready_at,omitempty"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	SuspendedAt  *time.Time `json:"suspended_at,omitempty"`
	TerminatedAt *time.Time `json:"terminated_at,omitempty"`

	// RunningSeconds is how long this sandbox has actually held compute. With
	// the tokens, it is what a task cost.
	RunningSeconds int64      `json:"running_seconds"`
	AccountedAt    *time.Time `json:"-"`
}

// Live reports whether this sandbox still holds resources.
func (s Sandbox) Live() bool { return s.State.Live() }

// CoreSeconds is running time weighted by the machine's size, which is what a
// chargeback needs.
func (s Sandbox) CoreSeconds() int64 {
	return s.RunningSeconds * int64(s.CPU) / 1000
}

const sandboxColumns = `SELECT id, org_id, COALESCE(team_id,''), COALESCE(user_id,''), owner,
	name, class, purpose, state, detail, image, isolation, cpu_millis, memory_mib, disk_mib,
	COALESCE(key_id,''), COALESCE(session_key,''), repo, branch, git_credential_id,
	node, address, backing, created_at, ready_at, expires_at, suspended_at, terminated_at, running_seconds, accounted_at`

func scanSandbox(r row) (Sandbox, error) {
	var sb Sandbox
	err := r.Scan(&sb.ID, &sb.OrgID, &sb.TeamID, &sb.UserID, &sb.Owner,
		&sb.Name, &sb.Class, &sb.Purpose, &sb.State, &sb.Detail,
		&sb.Image, &sb.Isolation, &sb.CPU, &sb.Memory, &sb.Disk,
		&sb.KeyID, &sb.SessionKey, &sb.Repo, &sb.Branch, &sb.GitCredentialID,
		&sb.Node, &sb.Address, &sb.Backing,
		&sb.CreatedAt, &sb.ReadyAt, &sb.ExpiresAt, &sb.SuspendedAt, &sb.TerminatedAt,
		&sb.RunningSeconds, &sb.AccountedAt)
	return sb, err
}

// sandboxWhere reads the one sandbox the rest of the query picks.
func (s *Store) sandboxWhere(ctx context.Context, rest string, args ...any) (Sandbox, error) {
	sb, err := scanSandbox(s.pool.QueryRow(ctx, sandboxColumns+" FROM sandboxes "+rest, args...))
	if err != nil {
		return Sandbox{}, notFound(err)
	}
	return sb, nil
}

// sandboxesWhere reads every sandbox the rest of the query picks.
func (s *Store) sandboxesWhere(ctx context.Context, rest string, args ...any) ([]Sandbox, error) {
	rows, err := s.pool.Query(ctx, sandboxColumns+" FROM sandboxes "+rest, args...)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanSandbox)
}

// CreateSandbox inserts one row.
//
// The one collision a caller can act on, a live sandbox with the same name, is
// ErrSandboxNameTaken. Any other unique violation would be a random id
// colliding, which is not worth a message.
func (s *Store) CreateSandbox(ctx context.Context, sb Sandbox) (Sandbox, error) {
	err := s.pool.QueryRow(ctx, `INSERT INTO sandboxes
		(id, org_id, team_id, user_id, owner, name, class, purpose, state, detail,
		 image, isolation, cpu_millis, memory_mib, disk_mib, key_id, session_key,
		 repo, branch, git_credential_id, backing, expires_at, accounted_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,
		        now())
		RETURNING created_at, accounted_at`,
		sb.ID, sb.OrgID, nullable(sb.TeamID), nullable(sb.UserID), sb.Owner,
		sb.Name, sb.Class, string(sb.Purpose), string(sb.State), sb.Detail,
		sb.Image, string(sb.Isolation), sb.CPU, sb.Memory, sb.Disk,
		nullable(sb.KeyID), nullable(sb.SessionKey), sb.Repo, sb.Branch, sb.GitCredentialID,
		defaultBacking(sb.Backing), sb.ExpiresAt,
	).Scan(&sb.CreatedAt, &sb.AccountedAt)
	if isUnique(err) {
		return sb, ErrSandboxNameTaken
	}
	return sb, err
}

// ErrSandboxNameTaken means a live sandbox in the organisation already has the
// name. The control plane turns it into a 409; the name is free again once
// that sandbox is gone.
var ErrSandboxNameTaken = errSandboxNameTaken{}

type errSandboxNameTaken struct{}

func (errSandboxNameTaken) Error() string {
	return "store: a live sandbox of that name already exists in this organisation"
}

// Sandbox reads one row by id.
func (s *Store) Sandbox(ctx context.Context, id string) (Sandbox, error) {
	return s.sandboxWhere(ctx, "WHERE id = $1", id)
}

// LiveSandboxByName finds the live sandbox of that name in an organisation,
// which is how `keera sandbox ssh <name>` resolves a name. A name used only by
// finished sandboxes is ErrNotFound.
func (s *Store) LiveSandboxByName(ctx context.Context, orgID, name string) (Sandbox, error) {
	return s.sandboxWhere(ctx, "WHERE org_id = $1 AND name = $2 AND "+liveSandbox, orgID, name)
}

// LiveSandboxByKey finds the live sandbox a presented key was minted for, if
// that key still works. It is how a sandbox asks for a fresh repository
// credential with nothing but its own key.
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

// SandboxQuery narrows the list. The zero value is one organisation's live
// sandboxes, newest first.
type SandboxQuery struct {
	OrgID  string
	TeamID string
	UserID string
	Class  string
	// Purpose narrows to engineer or agent sandboxes.
	Purpose policy.Purpose
	// All includes finished sandboxes. It is off by default, because the usual
	// question is "what is running now".
	All   bool
	Limit int
}

// ListSandboxes reads one organisation's sandboxes.
func (s *Store) ListSandboxes(ctx context.Context, q SandboxQuery) ([]Sandbox, error) {
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	return s.sandboxesWhere(ctx, `
		WHERE ($1 = '' OR org_id = $1)
		  AND ($2 = '' OR team_id = $2)
		  AND ($3 = '' OR user_id = $3)
		  AND ($4 = '' OR class = $4)
		  AND ($5 = '' OR purpose = $5)
		  AND ($6 OR `+liveSandbox+`)
		ORDER BY created_at DESC
		LIMIT $7`,
		q.OrgID, q.TeamID, q.UserID, q.Class, string(q.Purpose), q.All, limit)
}

// SandboxObservation is what the driver saw, written back onto the row.
//
// It is applied all at once, because a partial update could leave a row that
// disagrees with itself, such as a ready sandbox with no address.
type SandboxObservation struct {
	State   policy.SandboxState
	Detail  string
	Address string
	Node    string
	// Expires is the expiry the driver reports, so an extension that never
	// reached the cluster shows up as wrong. Zero leaves the stored value.
	Expires time.Time
}

// ObserveSandbox writes back what the driver saw.
//
// ready_at is set the first time a sandbox is ready and never again: a resume
// is not when the machine became usable. suspended_at is reset on each
// suspend, because it means "idle since".
//
// A row that is gone or already terminated is left alone without an error.
// Usually somebody ended the sandbox after the sweep read it.
func (s *Store) ObserveSandbox(ctx context.Context, id string, obs SandboxObservation) error {
	_, err := s.pool.Exec(ctx, `UPDATE sandboxes SET
			state = $2,
			detail = $3,
			address = $4,
			node = $5,
			expires_at = COALESCE($6, expires_at),
			ready_at = CASE WHEN ready_at IS NULL AND $2 = 'ready' THEN now() ELSE ready_at END,
			suspended_at = CASE WHEN $2 = 'suspended' THEN COALESCE(suspended_at, now())
			                    ELSE NULL END,
			terminated_at = CASE WHEN $2 = 'terminated' THEN COALESCE(terminated_at, now())
			                     ELSE terminated_at END
		WHERE id = $1 AND state <> 'terminated'`,
		id, string(obs.State), obs.Detail, obs.Address, obs.Node, nullableTime(obs.Expires))
	return err
}

// SetSandboxExpiry moves when a sandbox ends.
func (s *Store) SetSandboxExpiry(ctx context.Context, id string, at time.Time) error {
	return s.execOne(ctx,
		"UPDATE sandboxes SET expires_at = $2 WHERE id = $1 AND state <> 'terminated'", id, at)
}

// CountLiveSandboxes counts what one scope is holding, for the quota.
//
// scopeType is org, team or key. At key level it counts by the key minted for
// the sandbox, which makes a per-key quota a per-sandbox quota. Nothing offers
// one; the level exists so the three stay symmetrical.
func (s *Store) CountLiveSandboxes(ctx context.Context, scopeType policy.ScopeType, scopeID string) (int, error) {
	column := "key_id"
	switch scopeType {
	case policy.ScopeOrg:
		column = "org_id"
	case policy.ScopeTeam:
		column = "team_id"
	}
	var n int
	err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM sandboxes WHERE "+column+" = $1 AND "+liveSandbox, scopeID).Scan(&n)
	return n, err
}

// SandboxesPastExpiry reads the live sandboxes whose time is up, for the sweep.
func (s *Store) SandboxesPastExpiry(ctx context.Context, now time.Time, limit int) ([]Sandbox, error) {
	if limit <= 0 {
		limit = 100
	}
	return s.sandboxesWhere(ctx, `
		WHERE expires_at IS NOT NULL AND expires_at <= $1
		  AND `+liveSandbox+`
		ORDER BY expires_at
		LIMIT $2`, now, limit)
}

// LiveSandboxes reads every sandbox the drivers should still know about, for
// the sweep that reconciles rows with reality.
func (s *Store) LiveSandboxes(ctx context.Context, limit int) ([]Sandbox, error) {
	if limit <= 0 {
		limit = 500
	}
	return s.sandboxesWhere(ctx, "WHERE "+liveSandbox+" ORDER BY created_at LIMIT $1", limit)
}

// accrueRunning adds the time since accounted_at to running_seconds, as of $1,
// for sandboxes holding compute.
const accrueRunning = `UPDATE sandboxes SET
		running_seconds = running_seconds +
			GREATEST(0, EXTRACT(EPOCH FROM ($1::timestamptz - COALESCE(accounted_at, created_at)))::bigint),
		accounted_at = $1
	WHERE state IN ('pending','ready')`

// AccountSandboxes brings running_seconds up to date for every sandbox holding
// compute, and returns how many rows it touched.
//
// It is one statement, which keeps it correct across replicas: each sweep adds
// only the time since accounted_at and moves it forward. The clock is read from
// the column, so time a gateway was down is still charged.
func (s *Store) AccountSandboxes(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, accrueRunning, now)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// StopAccounting freezes a sandbox's clock when it stops holding compute
// (suspended, expired, terminated, failed), so the time until the next sweep
// is not charged.
func (s *Store) StopAccounting(ctx context.Context, id string, now time.Time) error {
	_, err := s.pool.Exec(ctx, accrueRunning+" AND id = $2", now, id)
	return err
}

// SandboxUsage is what one group of sandboxes came to over a window.
type SandboxUsage struct {
	// Key is the group: a team id, a user's address or a class name, as the
	// caller grouped by.
	Key   string `json:"key"`
	Label string `json:"label,omitempty"`
	// Count is how many sandboxes, Running the seconds they held compute, and
	// CoreSeconds the same weighted by machine size. Each tells a different
	// story, so all three are shown.
	Count       int64 `json:"count"`
	Running     int64 `json:"running_seconds"`
	CoreSeconds int64 `json:"core_seconds"`
	// Live is how many of them are still running now, which the other numbers
	// cannot give.
	Live int64 `json:"live"`
}

// sandboxGroupColumns maps the public group_by values to columns, which keeps
// the caller's string out of the SQL text. It mirrors groupColumns in usage.go.
var sandboxGroupColumns = map[string]string{
	"class": "s.class",
	"team":  "COALESCE(s.team_id, '')",
	"user":  "COALESCE(NULLIF(s.owner, ''), COALESCE(s.user_id, ''))",
}

// ValidSandboxGroupBy reports whether s is a grouping SandboxUsageBy
// understands. The empty string is not one: the handler picks the default.
func ValidSandboxGroupBy(s string) bool {
	_, ok := sandboxGroupColumns[s]
	return ok
}

// SandboxGroupBys lists the groupings, for the error a refused one earns.
func SandboxGroupBys() []string {
	return sortedKeys(sandboxGroupColumns)
}

// SandboxUsageBy groups sandbox time over a window.
//
// groupBy is "class", "team" or "user". The window applies to creation time,
// so a sandbox is counted whole in the window it started in. Splitting its
// seconds across days would need a row per interval.
func (s *Store) SandboxUsageBy(ctx context.Context, orgID, groupBy string, from, to time.Time) (
	[]SandboxUsage, error,
) {
	group, ok := sandboxGroupColumns[groupBy]
	if !ok {
		group = sandboxGroupColumns["class"]
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+group+` AS k,
		       count(*),
		       COALESCE(sum(s.running_seconds), 0),
		       COALESCE(sum(s.running_seconds * s.cpu_millis / 1000), 0),
		       count(*) FILTER (WHERE s.`+liveSandbox+`)
		FROM sandboxes s
		WHERE s.created_at >= $1 AND s.created_at < $2 AND ($3 = '' OR s.org_id = $3)
		GROUP BY 1
		ORDER BY 3 DESC`, from, to, orgID)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(r row) (SandboxUsage, error) {
		var u SandboxUsage
		err := r.Scan(&u.Key, &u.Count, &u.Running, &u.CoreSeconds, &u.Live)
		return u, err
	})
}

// SandboxForSession finds the sandbox a task ran in, if any.
//
// A session key is a hash and cannot be reversed. But an agent sandbox states
// its own session id, and the key it hashes to is stored at creation, so this
// is a plain lookup on that column.
func (s *Store) SandboxForSession(ctx context.Context, orgID, sessionKey string) (Sandbox, error) {
	return s.sandboxWhere(ctx,
		"WHERE org_id = $1 AND session_key = $2 ORDER BY created_at DESC LIMIT 1",
		orgID, sessionKey)
}

// defaultBacking fills in "sandbox" for a caller that did not say. That is what
// every sandbox was before warm pools, and what the podman driver always uses.
func defaultBacking(b string) string {
	if b == "" {
		return "sandbox"
	}
	return b
}
