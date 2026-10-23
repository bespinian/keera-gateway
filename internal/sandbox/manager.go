package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bespinian/keera-gateway/internal/auth"
	"github.com/bespinian/keera-gateway/internal/id"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

type ManagerOptions struct {
	// PublicURL is the gateway's base URL as reached from inside a sandbox.
	// Empty leaves inference unconfigured: fine for an engineer, useless for
	// an agent. It is a startup warning, not an error.
	PublicURL string
	// IdleSuspend is how long an engineer's sandbox may sit unused before it
	// is suspended. Zero never suspends. The volume is kept, so resuming takes
	// seconds. Agent sandboxes are never suspended: they wait for nothing.
	IdleSuspend time.Duration
	// Git mints the credential a sandbox checks out with. Nil makes asking
	// for a repository a clear refusal.
	Git GitMinter
	// OnKeyChange is called with a key's id after it is minted or revoked, so
	// the gateway's cache drops it at once and a new sandbox works right away.
	OnKeyChange func(keyID string)
	Log         *slog.Logger
}

// Manager owns the sandboxes of one deployment. It sits between the control
// plane and the driver: it checks a request against the class and the
// guardrail, mints the key and repository credential, writes the row and calls
// the driver, and undoes what it minted if the driver fails. Its sweep ends
// sandboxes whose time is up and keeps the running time current.
type Manager struct {
	st     *store.Store
	driver Driver
	opts   ManagerOptions
	log    *slog.Logger
	// pruneOnce clears the pools left from when warm pools were on. See
	// reconcilePools.
	pruneOnce sync.Once
}

func NewManager(st *store.Store, driver Driver, opts ManagerOptions) *Manager {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	opts.PublicURL = strings.TrimRight(opts.PublicURL, "/")
	return &Manager{st: st, driver: driver, opts: opts, log: opts.Log}
}

// Driver returns this deployment's driver, for showing what is available.
func (m *Manager) Driver() Driver { return m.driver }

// ErrRefused is a request this deployment will not carry out, with the reason.
// There is one type because every refusal ends up as a sentence in a
// developer's terminal.
type ErrRefused struct {
	Reason string
	// Invalid marks a request that can never succeed as sent, such as a bad
	// name. Otherwise the refusal depends on state, which can change.
	Invalid bool
}

func (e *ErrRefused) Error() string { return e.Reason }

// Refuse is an *ErrRefused with a formatted reason.
func Refuse(format string, args ...any) error {
	return &ErrRefused{Reason: fmt.Sprintf(format, args...)}
}

// Invalid is Refuse for a request that is wrong in itself.
func Invalid(format string, args ...any) error {
	return &ErrRefused{Reason: fmt.Sprintf(format, args...), Invalid: true}
}

// CreateRequest is one authenticated request for a sandbox.
type CreateRequest struct {
	OrgID     string
	ProjectID string
	UserID    string
	// Owner is the address the sandbox belongs to. It is stamped into the
	// cluster and copied onto the row.
	Owner string
	Name  string
	Class string
	// Purpose decides the lifecycle. Empty means an engineer's sandbox.
	Purpose policy.Purpose
	// TTL is the lifetime asked for. Zero means the class default. Anything
	// above the class's or the policy's ceiling is clamped, not refused.
	TTL time.Duration
	// Repo and Branch are what to check out. Empty means an empty sandbox.
	Repo   string
	Branch string
	// Task is the instruction an agent sandbox starts on. It is passed in the
	// environment and never stored: it is the developer's own prose about
	// their code.
	Task string
	// AuthorizedKeys are the ssh public keys that may open a shell, in
	// practice the caller's own. The attach surface authenticates first, so
	// this is a second lock against anything else the network policy lets
	// reach the pod. An engineer's sandbox with none is refused.
	AuthorizedKeys []string
	// Limits is the caller's resolved sandbox guardrail: the organisation's,
	// narrowed by the project's.
	Limits policy.ResolvedSandbox
	// Env is extra environment from the caller. A resume after expiry drops
	// it. It cannot name a KEERA_ variable, or a caller could point a sandbox
	// at another endpoint.
	Env map[string]string
}

// Create makes one sandbox. A refusal leaves nothing live behind: a key or
// repository credential made before a later check refuses is revoked at once.
func (m *Manager) Create(ctx context.Context, req CreateRequest) (store.Sandbox, error) {
	if req.Purpose == "" {
		req.Purpose = policy.PurposeEngineer
	}
	class, err := m.admit(ctx, req)
	if err != nil {
		return store.Sandbox{}, err
	}

	expires := time.Now().Add(clampTTL(class, req.TTL, req.Limits)).UTC()

	sbID := id.New("sbx")
	// Decided once and stored on the row (see Backing).
	backing := backingFor(class, req.Purpose, m.driver.Capabilities().Warm)
	ref := Ref{ID: sbID, Name: req.Name, Backing: backing}

	row := store.Sandbox{
		ID: sbID, OrgID: req.OrgID, ProjectID: req.ProjectID, UserID: req.UserID, Owner: req.Owner,
		Name: req.Name, Class: class.Name, Purpose: req.Purpose,
		State: policy.SandboxPending, Detail: "accepted",
		Image: class.Image, Isolation: class.Isolation,
		CPU: class.CPU, Memory: class.Memory, Disk: class.Disk,
		Repo: req.Repo, Branch: req.Branch, AuthorizedKeys: req.AuthorizedKeys,
		Backing:   string(backing),
		ExpiresAt: &expires,
	}
	s, err := m.start(ctx, req, row, expires)
	if err != nil {
		return store.Sandbox{}, err
	}
	row.KeyID = s.keyID
	if req.Repo != "" {
		row.Repo, row.GitCredentialID = s.git.Repo, s.git.ID
	}
	// An agent sandbox is one task, so it states its session instead of
	// leaving the gateway to infer one.
	if req.Purpose == policy.PurposeAgent {
		row.SessionKey = store.StatedSessionKeyFor(s.keyID, sbID)
	}

	row, err = m.st.CreateSandbox(ctx, row)
	if err != nil {
		m.undo(ctx, s)
		if errors.Is(err, store.ErrSandboxNameTaken) {
			return store.Sandbox{}, Refuse("you already have a live sandbox called %q; "+
				"`keera sandbox terminate %s` frees the name, or pick another",
				req.Name, req.Name)
		}
		return store.Sandbox{}, err
	}
	// The key is usable from now on, so the cache must drop before the
	// sandbox starts.
	m.changed(s.keyID)

	// Not the caller's context: a client that hangs up during, say, an image
	// pull would leave a machine behind that its row does not know about. Each
	// driver bounds its own calls.
	dctx := context.WithoutCancel(ctx)
	status, err := m.driver.Create(dctx, specFor(ref, class, req, s.env, expires))
	if err != nil {
		// Keep the row, marked failed: why a sandbox could not be made is the
		// most useful thing to record. The key is still revoked.
		m.fail(ctx, row.ID, err)
		m.undo(ctx, s)
		// A failed agent sandbox is not swept again, so whatever the create
		// left behind goes now. An engineer's stays live until it is
		// terminated, which removes the machine too.
		if req.Purpose == policy.PurposeAgent {
			if terr := m.driver.Terminate(dctx, ref); terr != nil && !errors.Is(terr, ErrNotFound) {
				m.log.Warn("sandbox: removing what a failed create left failed",
					"sandbox", row.ID, "error", terr)
			}
		}
		return store.Sandbox{}, fmt.Errorf("creating sandbox %s: %w", req.Name, err)
	}

	m.observe(ctx, row.ID, status)
	row.State, row.Detail, row.Node, row.Address = status.State, status.Detail, status.Node, status.Address
	return row, nil
}

// specFor is what the driver gets for a sandbox, at creation and at revival.
func specFor(ref Ref, class policy.SandboxClass, req CreateRequest, env map[string]string,
	expires time.Time,
) Spec {
	return Spec{
		Ref: ref, Class: class, Purpose: req.Purpose,
		Owner: req.Owner, Org: req.OrgID, Project: req.ProjectID,
		Env: env, Expires: expires,
	}
}

// backingFor decides whether a new sandbox is claimed from its class's warm
// pool. Only an engineer's is: pool members are started before anybody asks,
// as engineer sandboxes that wait for sshd, and an agent runs no sshd.
func backingFor(class policy.SandboxClass, p policy.Purpose, warm bool) Backing {
	if warm && class.Warm > 0 && p == policy.PurposeEngineer {
		return BackingClaim
	}
	return BackingSandbox
}

// admit runs the checks in the order of the questions: is the request valid,
// does the class exist, may this purpose and scope use it, can the driver
// deliver it, is there room. It returns the class.
func (m *Manager) admit(ctx context.Context, req CreateRequest) (policy.SandboxClass, error) {
	if !req.Purpose.Valid() {
		return policy.SandboxClass{}, Invalid("%q is not a purpose; a sandbox is for an 'engineer' "+
			"or for an 'agent'", req.Purpose)
	}
	if !policy.ValidSandboxName(req.Name) {
		return policy.SandboxClass{}, Invalid("%q is not a usable sandbox name; it becomes a hostname "+
			"in the cluster and half of an ssh config entry on your laptop, so it is lowercase "+
			"letters, digits and interior hyphens", req.Name)
	}
	// Some KEERA_ values are set only in some sandboxes, and the caller's would
	// fill the gap. KEERA_BASE_URL, for one, decides where the key is sent.
	for name := range req.Env {
		if strings.HasPrefix(name, "KEERA_") {
			return policy.SandboxClass{}, Invalid("%s is set by the gateway; a sandbox's own "+
				"environment cannot name a KEERA_ variable", name)
		}
	}
	if req.Purpose == policy.PurposeAgent {
		if strings.TrimSpace(req.Task) == "" {
			return policy.SandboxClass{}, Invalid("an agent sandbox needs a task: it is the " +
				"instruction the agent starts on")
		}
		if req.Repo == "" {
			return policy.SandboxClass{}, Invalid("an agent sandbox needs a repository: the " +
				"agent's work comes out of it only as a branch pushed there")
		}
	}

	class, err := m.st.SandboxClass(ctx, req.OrgID, req.Class)
	if errors.Is(err, store.ErrNotFound) {
		return policy.SandboxClass{}, Refuse("this organisation has no sandbox class %q; "+
			"`keera sandbox classes` lists them", req.Class)
	}
	if err != nil {
		return policy.SandboxClass{}, err
	}
	if !class.Allows(req.Purpose) {
		return policy.SandboxClass{}, Refuse("the sandbox class %q is not offered for %s sandboxes "+
			"in this organisation", class.Name, req.Purpose)
	}
	if err := req.Limits.Admits(class); err != nil {
		return policy.SandboxClass{}, Refuse("%v", err)
	}
	if caps := m.driver.Capabilities(); !slices.Contains(caps.Tiers, class.Isolation) {
		return policy.SandboxClass{}, noRuntime(class, m.driver.Name())
	}
	if req.Purpose == policy.PurposeEngineer && len(req.AuthorizedKeys) == 0 {
		return policy.SandboxClass{}, Invalid("no ssh public key was given, so nothing could open a " +
			"shell in this sandbox. `keera sandbox create` sends yours from ~/.ssh; if you have " +
			"none, `ssh-keygen -t ed25519` makes one")
	}
	if err := m.checkQuota(ctx, req); err != nil {
		return policy.SandboxClass{}, err
	}
	return class, nil
}

// clampTTL is the lifetime a sandbox gets: what was asked for, within the
// class's ceiling and the policy's.
func clampTTL(class policy.SandboxClass, want time.Duration, limits policy.ResolvedSandbox) time.Duration {
	ttl := class.TTLFor(want)
	if ceiling := limits.MaxTTL(); ceiling > 0 && ttl > ceiling {
		ttl = ceiling
	}
	return ttl
}

// checkQuota refuses a sandbox that would take the organisation or the project
// past its limit. There is no per-key quota: each key is minted for one
// sandbox.
//
// Each count is held to its own level's limit. req.Limits is the tightest of
// all levels, right for the project, but a project capped at two must not cap
// its whole organisation at two.
func (m *Manager) checkQuota(ctx context.Context, req CreateRequest) error {
	orgLimit := req.Limits.MaxSandboxes
	if req.ProjectID != "" {
		lim, err := m.st.GetGuardrail(ctx, policy.ScopeOrg, req.OrgID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		orgLimit = 0
		if lim.MaxSandboxes != nil {
			orgLimit = *lim.MaxSandboxes
		}
	}
	for _, scope := range []struct {
		typ   policy.ScopeType
		id    string
		limit int
		label string
	}{
		{policy.ScopeOrg, req.OrgID, orgLimit, "your organisation"},
		{policy.ScopeProject, req.ProjectID, req.Limits.MaxSandboxes, "your project"},
	} {
		if scope.id == "" || scope.limit <= 0 {
			continue
		}
		n, err := m.st.CountLiveSandboxes(ctx, scope.typ, scope.id)
		if err != nil {
			return err
		}
		if n >= scope.limit {
			return Refuse("%s is already running %d sandboxes, which is its limit. "+
				"`keera sandbox ls` shows them, and terminating one you have finished with "+
				"frees the slot immediately", scope.label, n)
		}
	}
	return nil
}

// mintKey issues the sandbox's API key, attributed to whoever asked for it,
// so its usage lands on their budget and reports like anything else they do.
func (m *Manager) mintKey(ctx context.Context, req CreateRequest, expires time.Time) (
	secret, keyID string, err error,
) {
	secret, hash, prefix := auth.Generate()
	info := store.KeyInfo{
		ID: id.New("key"), OrgID: req.OrgID, ProjectID: req.ProjectID, UserID: req.UserID,
		// Named after the sandbox, so a request in the log leads back to it.
		Name:   "sandbox/" + req.Name,
		Prefix: prefix,
		// Expires with the sandbox, so even a missed revocation is bounded.
		ExpiresAt: &expires,
	}
	if _, err := m.st.CreateKey(ctx, info, hash); err != nil {
		return "", "", err
	}
	return secret, info.ID, nil
}

// started is what a starting sandbox was given.
type started struct {
	keyID string
	env   map[string]string
	git   GitCredential
}

// start gives a starting sandbox, new or revived, its key, its environment
// and, with a repository, its repository credential. On a failure nothing is
// left live; after it, the caller runs undo if a later step fails.
func (m *Manager) start(ctx context.Context, req CreateRequest, row store.Sandbox,
	expires time.Time,
) (started, error) {
	if req.Repo != "" && m.opts.Git == nil {
		return started{}, Refuse("this deployment has no forge configured "+
			"(KEERA_SANDBOX_GIT_FORGE), so a sandbox cannot check out %s; create an "+
			"empty one and bring the code in yourself", req.Repo)
	}
	// Minted now, so it expires and is revoked with the sandbox. Nobody sees
	// it: it goes into the sandbox's environment only.
	secret, keyID, err := m.mintKey(ctx, req, expires)
	if err != nil {
		return started{}, err
	}
	s := started{keyID: keyID, env: m.environment(ctx, req, row, secret, expires)}
	if req.Repo != "" {
		if s.git, err = m.checkOut(ctx, s.env, req, expires); err != nil {
			m.undo(ctx, s)
			return started{}, err
		}
	}
	return s, nil
}

// undo revokes what a sandbox that did not start was given.
func (m *Manager) undo(ctx context.Context, s started) {
	ctx = context.WithoutCancel(ctx)
	if err := m.st.RevokeKey(ctx, s.keyID); err != nil {
		m.log.Warn("sandbox: revoking the key of a sandbox that did not start failed",
			"key_id", s.keyID, "error", err)
	}
	m.changed(s.keyID)
	m.revokeCredential(ctx, s.git.Repo, s.git.ID)
}

// Dial opens a connection to a port on a running sandbox, for the attach
// surface.
func (m *Manager) Dial(ctx context.Context, sb store.Sandbox, port int) (net.Conn, error) {
	if !sb.Purpose.Attachable() {
		return nil, Refuse("sandbox %s is an agent's; nothing attaches to one. Its whole claim "+
			"is that only a branch came out of it, and a shell nobody recorded would make that "+
			"claim unprovable", sb.Name)
	}
	if sb.State == policy.SandboxSuspended || sb.State == policy.SandboxExpired {
		return nil, Refuse("sandbox %s is %s; `keera sandbox resume %s` brings it back, "+
			"with its volume as you left it", sb.Name, sb.State, sb.Name)
	}
	if sb.State != policy.SandboxReady {
		return nil, Refuse("sandbox %s is %s: %s", sb.Name, sb.State, sb.Detail)
	}
	return m.driver.Dial(ctx, refOf(sb), port)
}

func (m *Manager) observe(ctx context.Context, sbID string, status Status) {
	if err := m.st.ObserveSandbox(ctx, sbID, store.SandboxObservation{
		State: status.State, Detail: status.Detail,
		Address: status.Address, Node: status.Node,
	}); err != nil {
		m.log.Warn("sandbox: writing back a sandbox's state failed", "sandbox", sbID, "error", err)
	}
}

// observeFrom is observe for what the driver said about sb as it was read. A
// row somebody changed in the meantime keeps their change.
func (m *Manager) observeFrom(ctx context.Context, sb store.Sandbox, status Status) {
	if err := m.st.ObserveSandboxFrom(ctx, sb.ID, sb.State, store.SandboxObservation{
		State: status.State, Detail: status.Detail,
		Address: status.Address, Node: status.Node,
	}); err != nil {
		m.log.Warn("sandbox: writing back a sandbox's state failed", "sandbox", sb.ID, "error", err)
	}
}

func (m *Manager) fail(ctx context.Context, sbID string, cause error) {
	if err := m.st.ObserveSandbox(context.WithoutCancel(ctx), sbID, store.SandboxObservation{
		State: policy.SandboxFailed, Detail: cause.Error(),
	}); err != nil {
		m.log.Warn("sandbox: recording a failure failed", "sandbox", sbID, "error", err)
	}
}

// release gives back what a sandbox held while it ran: its running clock, its
// key and its repository credential. It comes before the new state is
// recorded, since the clock only counts a sandbox the row still shows running.
func (m *Manager) release(ctx context.Context, sb store.Sandbox) {
	m.stopClock(ctx, sb.ID)
	m.revokeKey(ctx, sb)
	m.revokeGit(ctx, sb)
}

func (m *Manager) stopClock(ctx context.Context, sbID string) {
	if err := m.st.StopAccounting(ctx, sbID, time.Now()); err != nil {
		m.log.Warn("sandbox: freezing the running clock failed", "sandbox", sbID, "error", err)
	}
}

// revokeKey revokes a sandbox's key, if it has one. A key already revoked or
// gone is fine; any other failure is logged.
func (m *Manager) revokeKey(ctx context.Context, sb store.Sandbox) {
	if sb.KeyID == "" {
		return
	}
	if err := m.st.RevokeKey(ctx, sb.KeyID); err != nil &&
		!errors.Is(err, policy.ErrKeyRevoked) && !errors.Is(err, store.ErrNotFound) {
		m.log.Warn("sandbox: revoking a sandbox's key failed", "sandbox", sb.ID,
			"key_id", sb.KeyID, "state", sb.State, "error", err)
	}
	m.changed(sb.KeyID)
}

func (m *Manager) changed(keyID string) {
	if m.opts.OnKeyChange != nil {
		m.opts.OnKeyChange(keyID)
	}
}

func refOf(sb store.Sandbox) Ref {
	return Ref{ID: sb.ID, Name: sb.Name, Backing: Backing(sb.Backing)}
}
