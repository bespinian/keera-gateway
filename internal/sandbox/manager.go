package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bespinian/keera-gateway/internal/auth"
	"github.com/bespinian/keera-gateway/internal/catalog"
	"github.com/bespinian/keera-gateway/internal/connect"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/id"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// The manager sits between the control plane and the driver. It resolves the
// class, applies the guardrail, clamps the lifetime, mints the key, writes the
// row, calls the driver, and undoes all of it if the driver fails. The control
// plane only authenticates the caller and returns JSON.
//
// It also runs the sweep, which ends sandboxes whose time is up and keeps the
// running-time column current.

// ManagerOptions configures the manager.
type ManagerOptions struct {
	// PublicURL is the gateway as the sandbox sees it: the base URL its agent
	// sends inference to, reachable from inside the cluster.
	//
	// Empty creates sandboxes with no inference configured: fine for an
	// engineer, useless for an agent. It is a startup warning, not an error.
	PublicURL string
	// IdleSuspend is how long an engineer's sandbox may sit unused before it
	// is suspended. Zero never suspends it.
	//
	// Suspending keeps the volume, so a long lunch costs a few seconds of
	// resume. Agent sandboxes are never suspended: they have nothing to wait
	// for.
	IdleSuspend time.Duration
	// Git mints the credential a sandbox checks out with. Nil makes asking
	// for a repository a clear refusal.
	Git GitMinter
	// OnChange is called after a key is minted or revoked, so the gateway's
	// cache drops it at once. Otherwise a new sandbox could not make requests
	// for its first few seconds.
	OnChange func()
	Log      *slog.Logger
}

// GitMinter issues the credential a sandbox checks out with. internal/forge
// has one for GitHub and one for GitLab.
//
// The rules are fixed: one repository, as short-lived as the forge allows, and
// never the developer's own credential. Forwarding an ssh agent into a machine
// that runs a model's output is exactly what sandboxes exist to avoid.
type GitMinter interface {
	// Mint returns a credential for one repository. It goes into the
	// sandbox's environment and nowhere else. A repository this minter cannot
	// serve is an *ErrRefused that says why.
	Mint(ctx context.Context, req GitRequest) (GitCredential, error)
}

// GitRevoker is a minter whose credentials can be taken back before they
// expire. The manager revokes a sandbox's credential when the sandbox ends.
type GitRevoker interface {
	Revoke(ctx context.Context, repo, id string) error
}

// GitRequest is what a credential is for.
type GitRequest struct {
	Repo string
	// Until is when the sandbox ends. The credential should not outlive it by
	// more than the forge forces.
	Until time.Time
	// Sandbox names the sandbox, so the forge's list of tokens says what each
	// one is for.
	Sandbox string
	// Allow checks the repository's path on the forge against the caller's
	// guardrail, and says why not. Only the forge can read the path, so it
	// calls this. Nil allows nothing.
	Allow func(path string) error
}

// GitCredential is what a sandbox checks out with.
type GitCredential struct {
	// Repo is the address to clone. It can differ from the one asked for: a
	// token only works over HTTPS, so an ssh address is rewritten.
	Repo string
	// Username and Token make an HTTPS credential. A forge that wants only a
	// token leaves Username empty.
	Username string
	Token    string
	// ID is the forge's id for the credential, for a minter that can revoke
	// it. It is stored; the credential is not.
	ID string
	// Expires is when the forge stops accepting it. Zero means never, or
	// unknown.
	Expires time.Time
}

// Manager owns the sandboxes of one deployment.
type Manager struct {
	st     *store.Store
	driver Driver
	opts   ManagerOptions
	log    *slog.Logger
	// pruneOnce clears the pools left from when warm pools were on. See
	// reconcilePools.
	pruneOnce sync.Once
}

// NewManager builds the manager.
func NewManager(st *store.Store, driver Driver, opts ManagerOptions) *Manager {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
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

func refuse(format string, args ...any) error {
	return &ErrRefused{Reason: fmt.Sprintf(format, args...)}
}

// invalid is refuse for a request that is wrong in itself.
func invalid(format string, args ...any) error {
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
	// practice the caller's own.
	//
	// The attach surface authenticates first, so this is a second lock, for
	// anything else the network policy lets reach the pod. An engineer's
	// sandbox with none is refused.
	AuthorizedKeys []string
	// Limits is the caller's resolved sandbox guardrail: the organisation's,
	// narrowed by the project's.
	Limits policy.ResolvedSandbox
	// Env is extra environment from the caller, for the first start only. It
	// cannot name a KEERA_ variable, or a caller could point a sandbox at
	// another endpoint.
	Env map[string]string
}

// Create makes one sandbox.
//
// Every check runs before anything is created, so a refusal leaves no API key
// or half-made pod behind.
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
	// Decided once and stored on the row, so the sandbox stays addressable if
	// warm pools are later turned off.
	backing := backingFor(class, req.Purpose, m.driver.Capabilities().Warm)
	ref := Ref{ID: sbID, Name: req.Name, Backing: backing}

	// Minted now, so it expires and is revoked with the sandbox. Nobody sees
	// it: it goes into the sandbox's environment only.
	secret, keyID, err := m.mintKey(ctx, req, expires)
	if err != nil {
		return store.Sandbox{}, err
	}

	// Revokes the key and the repository credential on every failure below,
	// so nothing live is left for a sandbox that does not exist.
	var git GitCredential
	rollback := m.undoStart(ctx, keyID, &git)

	row := store.Sandbox{
		ID: sbID, OrgID: req.OrgID, ProjectID: req.ProjectID, UserID: req.UserID, Owner: req.Owner,
		Name: req.Name, Class: class.Name, Purpose: req.Purpose,
		State: policy.SandboxPending, Detail: "accepted",
		Image: class.Image, Isolation: class.Isolation,
		CPU: class.CPU, Memory: class.Memory, Disk: class.Disk,
		KeyID: keyID, Repo: req.Repo, Branch: req.Branch, AuthorizedKeys: req.AuthorizedKeys,
		Backing:   string(backing),
		ExpiresAt: &expires,
	}
	// An agent sandbox is one task, so it states its session instead of
	// leaving the gateway to infer one. The session key follows from the key
	// id and the sandbox id, so it can be recorded now.
	if req.Purpose == policy.PurposeAgent {
		row.SessionKey = store.StatedSessionKeyFor(keyID, sbID)
	}

	env := m.environment(ctx, req, row, secret, expires)
	if req.Repo != "" {
		git, err = m.checkOut(ctx, env, req, expires)
		if err != nil {
			rollback()
			return store.Sandbox{}, err
		}
		row.Repo, row.GitCredentialID = git.Repo, git.ID
	}

	row, err = m.st.CreateSandbox(ctx, row)
	if err != nil {
		rollback()
		if errors.Is(err, store.ErrSandboxNameTaken) {
			return store.Sandbox{}, refuse("you already have a live sandbox called %q; "+
				"`keera sandbox terminate %s` frees the name, or pick another",
				req.Name, req.Name)
		}
		return store.Sandbox{}, err
	}
	// The key is usable from now on, so the cache must drop before the
	// sandbox starts.
	m.changed()

	status, err := m.driver.Create(ctx, Spec{
		Ref: ref, Class: class, Purpose: req.Purpose,
		Owner: req.Owner, Org: req.OrgID, Project: req.ProjectID,
		Env: env, Expires: expires,
	})
	if err != nil {
		// Keep the row, marked failed: why a sandbox could not be made is the
		// most useful thing to record. The key is still revoked.
		m.fail(ctx, row.ID, err)
		rollback()
		return store.Sandbox{}, fmt.Errorf("creating sandbox %s: %w", req.Name, err)
	}

	m.observe(ctx, row.ID, status)
	row.State, row.Detail, row.Node, row.Address = status.State, status.Detail, status.Node, status.Address
	return row, nil
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
		return policy.SandboxClass{}, invalid("%q is not a purpose; a sandbox is for an 'engineer' "+
			"or for an 'agent'", req.Purpose)
	}
	if !policy.ValidSandboxName(req.Name) {
		return policy.SandboxClass{}, invalid("%q is not a usable sandbox name; it becomes a hostname "+
			"in the cluster and half of an ssh config entry on your laptop, so it is lowercase "+
			"letters, digits and interior hyphens", req.Name)
	}
	// Some KEERA_ values are set only in some sandboxes, and one from the
	// caller would stand in for them where they are not: KEERA_BASE_URL, for
	// one, decides where the sandbox's key is sent.
	for name := range req.Env {
		if strings.HasPrefix(name, "KEERA_") {
			return policy.SandboxClass{}, invalid("%s is set by the gateway; a sandbox's own "+
				"environment cannot name a KEERA_ variable", name)
		}
	}
	if req.Purpose == policy.PurposeAgent {
		if strings.TrimSpace(req.Task) == "" {
			return policy.SandboxClass{}, invalid("an agent sandbox needs a task: it is the " +
				"instruction the agent starts on")
		}
		if req.Repo == "" {
			return policy.SandboxClass{}, invalid("an agent sandbox needs a repository: the " +
				"agent's work comes out of it only as a branch pushed there")
		}
	}

	class, err := m.st.SandboxClass(ctx, req.OrgID, req.Class)
	if errors.Is(err, store.ErrNotFound) {
		return policy.SandboxClass{}, refuse("this organisation has no sandbox class %q; "+
			"`keera sandbox classes` lists them", req.Class)
	}
	if err != nil {
		return policy.SandboxClass{}, err
	}
	if !class.Allows(req.Purpose) {
		return policy.SandboxClass{}, refuse("the sandbox class %q is not offered for %s sandboxes "+
			"in this organisation", class.Name, req.Purpose)
	}
	if err := req.Limits.Admits(class); err != nil {
		return policy.SandboxClass{}, &ErrRefused{Reason: err.Error()}
	}
	if caps := m.driver.Capabilities(); !slices.Contains(caps.Tiers, class.Isolation) {
		return policy.SandboxClass{}, refuse("the sandbox class %q asks for %s isolation and the %s "+
			"driver in this deployment has no runtime for it; set KEERA_SANDBOX_RUNTIME_%s, or "+
			"move the class to a tier it has. A sandbox that claimed a kernel of its own and "+
			"did not have one would be worse than this refusal",
			class.Name, class.Isolation, m.driver.Name(), strings.ToUpper(string(class.Isolation)))
	}
	if req.Purpose == policy.PurposeEngineer && len(req.AuthorizedKeys) == 0 {
		return policy.SandboxClass{}, invalid("no ssh public key was given, so nothing could open a " +
			"shell in this sandbox. `keera sandbox create` sends yours from ~/.ssh; if you have " +
			"none, `ssh-keygen -t ed25519` makes one")
	}
	if req.Repo != "" && m.opts.Git == nil {
		return policy.SandboxClass{}, refuse("this deployment has no forge configured " +
			"(KEERA_SANDBOX_GIT_FORGE), so a sandbox cannot check anything out; create an " +
			"empty one and bring the code in yourself")
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
// past its limit.
//
// There is no per-key quota: the key is minted for one sandbox, so it would
// always be a limit of one.
//
// Each count is held to its own level's limit. req.Limits is the tightest of
// every level, which is right for the project, but a project capped at two must not
// cap its whole organisation at two.
func (m *Manager) checkQuota(ctx context.Context, req CreateRequest) error {
	orgLimit := req.Limits.MaxSandboxes
	if req.ProjectID != "" {
		lim, err := m.st.GetPolicy(ctx, policy.ScopeOrg, req.OrgID)
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
			return refuse("%s is already running %d sandboxes, which is its limit. "+
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

// environment is what the sandbox is told.
//
// The caller's Env goes in first and the manager's values over it, so a caller
// cannot redirect a sandbox's gateway address or key.
func (m *Manager) environment(ctx context.Context, req CreateRequest, row store.Sandbox,
	secret string, expires time.Time,
) map[string]string {
	env := map[string]string{}
	maps.Copy(env, req.Env)

	env["KEERA_SANDBOX_ID"] = row.ID
	env["KEERA_SANDBOX_NAME"] = row.Name
	env["KEERA_SANDBOX_CLASS"] = row.Class
	env["KEERA_SANDBOX_PURPOSE"] = string(row.Purpose)
	env["KEERA_SANDBOX_EXPIRES"] = expires.Format(time.RFC3339)
	env["KEERA_API_KEY"] = secret

	if len(req.AuthorizedKeys) > 0 {
		env["KEERA_AUTHORIZED_KEYS"] = strings.Join(req.AuthorizedKeys, "\n")
	}
	// Commits are by the person the work was done for, not by "keera".
	if req.Owner != "" {
		env["KEERA_GIT_AUTHOR_EMAIL"] = req.Owner
		env["KEERA_GIT_AUTHOR_NAME"] = req.Owner
	}

	// An agent sandbox names its own session (docs/sessions.md): as the
	// header Claude Code reads itself, and in Pi's configuration below.
	if row.Purpose == policy.PurposeAgent {
		env["KEERA_SESSION"] = row.ID
		env["ANTHROPIC_CUSTOM_HEADERS"] = httpx.SessionHeader + ": " + row.ID
	}

	m.inferenceEnv(env)
	m.agentEnv(ctx, req, env)
	// Passed once at start and never stored.
	if req.Task != "" {
		env["KEERA_TASK"] = req.Task
	}
	return env
}

// inferenceEnv points the sandbox's clients at this gateway.
func (m *Manager) inferenceEnv(env map[string]string) {
	base := strings.TrimRight(m.opts.PublicURL, "/")
	if base == "" {
		return
	}
	inference := base + httpx.InferencePrefix
	env["KEERA_BASE_URL"] = inference
	// Both spellings, because OpenAI and Anthropic clients disagree on where
	// the version goes.
	env["OPENAI_BASE_URL"] = inference + "/v1"
	env["ANTHROPIC_BASE_URL"] = inference
	// The key is only in KEERA_API_KEY, never under a vendor's name. Pi arms
	// its built-in providers from ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN and
	// OPENAI_API_KEY; setting one would fill /model with models this key
	// cannot use and could send the key and prompts to that vendor.
	//
	// An image that adds such a client sets its variable from KEERA_API_KEY
	// in its own entrypoint (docs/sandboxes.md), and so re-arms that vendor.
}

// agentEnv configures Pi with every model the key may reach, and starts it on
// the first of them.
func (m *Manager) agentEnv(ctx context.Context, req CreateRequest, env map[string]string) {
	models := m.reachableModels(ctx, req)
	if len(models) == 0 && env["KEERA_BASE_URL"] != "" {
		m.log.Warn("sandbox: this key can reach no chat model, so the sandbox is created "+
			"with no agent configuration; check the organisation's and the project's "+
			"allowed models", "org", req.OrgID, "project", req.ProjectID)
	}
	if cfg := m.piConfig(env["KEERA_BASE_URL"], models, env["KEERA_SESSION"]); cfg != "" {
		env["KEERA_PI_CONFIG"] = cfg
		// Also make this gateway Pi's default provider. Pi prefers an armed
		// built-in provider, so without this an image that exports a vendor
		// variable would send the key to that vendor. This is sandbox-only:
		// `keera connect` must not override a laptop's own default.
		env["KEERA_PI_SETTINGS"] = piSettings(models[0].Alias)
	}
	if len(models) > 0 {
		// For the agent runner's --model.
		env["KEERA_MODEL"] = models[0].Alias
		env["ANTHROPIC_MODEL"] = models[0].Alias
	}
}

// mintGit asks the minter for a credential. A refusal is passed on as it is,
// so the developer reads the forge's reason and not a wrapper around it.
func (m *Manager) mintGit(ctx context.Context, req GitRequest) (GitCredential, error) {
	cred, err := m.opts.Git.Mint(ctx, req)
	var refused *ErrRefused
	switch {
	case errors.As(err, &refused):
		return GitCredential{}, refused
	case err != nil:
		return GitCredential{}, fmt.Errorf("minting a repository credential for %s: %w", req.Repo, err)
	}
	if cred.Repo == "" {
		cred.Repo = req.Repo
	}
	return cred, nil
}

// undoStart revokes what a sandbox that did not start was given: its key and,
// once *git is set, its repository credential.
func (m *Manager) undoStart(ctx context.Context, keyID string, git *GitCredential) func() {
	return func() {
		ctx := context.WithoutCancel(ctx)
		if err := m.st.RevokeKey(ctx, keyID); err != nil {
			m.log.Warn("sandbox: revoking the key of a sandbox that did not start failed",
				"key_id", keyID, "error", err)
		}
		m.changed()
		m.revokeCredential(ctx, git.Repo, git.ID)
	}
}

// checkOut mints the credential for the repository a starting sandbox checks
// out, and tells the sandbox what to check out and how.
func (m *Manager) checkOut(ctx context.Context, env map[string]string, req CreateRequest,
	until time.Time,
) (GitCredential, error) {
	cred, err := m.mintGit(ctx, GitRequest{Repo: req.Repo, Until: until, Sandbox: req.Name,
		Allow: allowRepo(req.Limits)})
	if err != nil {
		return cred, err
	}
	m.repoEnv(env, cred, req.Branch)
	return cred, nil
}

// repoEnv tells the sandbox what to check out and how.
func (m *Manager) repoEnv(env map[string]string, cred GitCredential, branch string) {
	env["KEERA_REPO"] = cred.Repo
	if branch != "" {
		env["KEERA_BRANCH"] = branch
	}
	env["KEERA_GIT_USERNAME"] = cred.Username
	env["KEERA_GIT_TOKEN"] = cred.Token
	if !cred.Expires.IsZero() {
		env["KEERA_GIT_TOKEN_EXPIRES"] = strconv.FormatInt(cred.Expires.Unix(), 10)
	}
	// Where the sandbox gets a fresh token when this one runs out. A GitHub
	// token lasts an hour and an engineer's sandbox a working day.
	if base := strings.TrimRight(m.opts.PublicURL, "/"); base != "" {
		env["KEERA_GIT_CREDENTIAL_URL"] = base + httpx.SandboxPrefix + GitCredentialPath
	}
}

// GitCredentialPath is where a sandbox asks for a fresh repository credential,
// under httpx.SandboxPrefix.
const GitCredentialPath = "/v1/git-credential"

// allowRepo checks a repository against a scope's allowed_repos.
func allowRepo(limits policy.ResolvedSandbox) func(string) error {
	return func(path string) error {
		if err := limits.AdmitsRepo(path); err != nil {
			return &ErrRefused{Reason: err.Error()}
		}
		return nil
	}
}

// RefreshGit mints a fresh repository credential for a live sandbox and
// revokes the one it replaces. The sandbox asks for it with its own key.
//
// limits is the sandbox's guardrail as it is now, not as it was at creation,
// so taking a repository off the list stops the next refresh.
func (m *Manager) RefreshGit(ctx context.Context, sb store.Sandbox,
	limits policy.ResolvedSandbox,
) (GitCredential, error) {
	switch {
	case sb.Repo == "":
		return GitCredential{}, refuse("sandbox %s was created without a repository, so it "+
			"has no repository credential", sb.Name)
	case m.opts.Git == nil:
		return GitCredential{}, refuse("this deployment has no way to mint a repository credential")
	case !sb.State.Running():
		return GitCredential{}, refuse("sandbox %s is %s; only a running sandbox gets a "+
			"repository credential", sb.Name, sb.State)
	case sb.ExpiresAt == nil || !sb.ExpiresAt.After(time.Now()):
		return GitCredential{}, refuse("sandbox %s has run out of time", sb.Name)
	}
	cred, err := m.mintGit(ctx, GitRequest{Repo: sb.Repo, Until: *sb.ExpiresAt, Sandbox: sb.Name,
		Allow: allowRepo(limits)})
	if err != nil {
		return GitCredential{}, err
	}
	if err := m.st.SetSandboxGitCredential(ctx, sb.ID, cred.ID); err != nil {
		// Not recorded means never revoked, so it goes now.
		m.revokeCredential(context.WithoutCancel(ctx), cred.Repo, cred.ID)
		return GitCredential{}, err
	}
	if sb.GitCredentialID != cred.ID {
		m.revokeCredential(ctx, sb.Repo, sb.GitCredentialID)
	}
	return cred, nil
}

// revokeGit takes back a sandbox's repository credential, if the forge can.
func (m *Manager) revokeGit(ctx context.Context, sb store.Sandbox) {
	if sb.GitCredentialID == "" {
		return
	}
	m.revokeCredential(ctx, sb.Repo, sb.GitCredentialID)
	if err := m.st.SetSandboxGitCredential(ctx, sb.ID, ""); err != nil &&
		!errors.Is(err, store.ErrNotFound) {
		m.log.Warn("sandbox: clearing a revoked repository credential failed",
			"sandbox", sb.ID, "error", err)
	}
}

// revokeCredential revokes one credential. A failure only logs: the
// credential still expires by itself.
func (m *Manager) revokeCredential(ctx context.Context, repo, id string) {
	revoker, ok := m.opts.Git.(GitRevoker)
	if id == "" || !ok {
		return
	}
	if err := revoker.Revoke(ctx, repo, id); err != nil {
		m.log.Warn("sandbox: revoking a repository credential failed; it still expires "+
			"by itself", "repo", repo, "credential", id, "error", err)
	}
}

/* ------------------------------------------------------------- the changes */

// Suspend releases a sandbox's compute and keeps its volume.
func (m *Manager) Suspend(ctx context.Context, sb store.Sandbox) error {
	if sb.State == policy.SandboxSuspended {
		return nil
	}
	// Resuming starts an agent's task again from the start.
	if sb.Purpose == policy.PurposeAgent {
		return refuse("sandbox %s is an agent's; it runs its task to the end and is not "+
			"suspended. Terminate it to stop it", sb.Name)
	}
	if !sb.State.Running() {
		return refuse("sandbox %s is %s; only a running sandbox can be suspended",
			sb.Name, sb.State)
	}
	if err := m.driver.Suspend(ctx, refOf(sb)); err != nil {
		return err
	}
	// Stop the clock now, not at the next sweep, so the time in between is
	// not charged.
	m.stopClock(ctx, sb.ID)
	return m.st.ObserveSandbox(ctx, sb.ID, store.SandboxObservation{
		State: policy.SandboxSuspended, Detail: "suspended; its volume is kept",
	})
}

// Resume brings a suspended or expired sandbox back. limits is the owner's
// sandbox guardrail as it is now, which an expired sandbox is held to again.
func (m *Manager) Resume(ctx context.Context, sb store.Sandbox, limits policy.ResolvedSandbox) error {
	switch {
	case sb.State == policy.SandboxReady || sb.State == policy.SandboxPending:
		return nil
	case sb.State.Final():
		return refuse("sandbox %s is %s; there is nothing left to resume", sb.Name, sb.State)
	case sb.State == policy.SandboxExpired:
		return m.revive(ctx, sb, limits)
	}
	if err := m.driver.Resume(ctx, refOf(sb)); err != nil {
		if errors.Is(err, ErrNotFound) {
			m.release(ctx, sb)
			m.fail(ctx, sb.ID, errors.New("the sandbox is no longer in the cluster"))
			return refuse("sandbox %s is no longer in the cluster; its volume went with it",
				sb.Name)
		}
		return err
	}
	// Read the state back instead of writing "resuming": a podman sandbox is
	// already running here, while a pod still has to be scheduled.
	status, err := m.driver.Status(ctx, refOf(sb))
	if err != nil {
		// The resume itself worked; the sweep will correct the row.
		m.log.Warn("sandbox: reading a resumed sandbox's state failed",
			"sandbox", sb.ID, "error", err)
		return m.st.ObserveSandbox(ctx, sb.ID, store.SandboxObservation{
			State: policy.SandboxPending, Detail: "resuming",
		})
	}
	m.observe(ctx, sb.ID, status)
	return nil
}

// revive starts an expired sandbox again, as if it were new but on the volume
// it kept: a new key, a new repository credential and the class's default
// lifetime, all under the guardrail as it is now.
func (m *Manager) revive(ctx context.Context, sb store.Sandbox, limits policy.ResolvedSandbox) error {
	if sb.Purpose != policy.PurposeEngineer {
		return refuse("sandbox %s is an agent's; its task is over, so it is not resumed", sb.Name)
	}
	// The machine is the one it was, so it keeps its own copy of the class.
	// The class as it is now only decides the lifetime.
	machine := standIn(sb)
	lifetime, err := m.lifetimeClass(ctx, sb)
	if err != nil {
		return err
	}
	if err := limits.Admits(machine); err != nil {
		return &ErrRefused{Reason: err.Error()}
	}
	if sb.Repo != "" && m.opts.Git == nil {
		return refuse("sandbox %s checks out %s and this deployment no longer has a forge "+
			"configured (KEERA_SANDBOX_GIT_FORGE), so it could not get a new credential",
			sb.Name, sb.Repo)
	}
	expires := time.Now().Add(clampTTL(lifetime, 0, limits)).UTC()

	req := CreateRequest{
		OrgID: sb.OrgID, ProjectID: sb.ProjectID, UserID: sb.UserID, Owner: sb.Owner,
		Name: sb.Name, Class: sb.Class, Purpose: sb.Purpose,
		Repo: sb.Repo, Branch: sb.Branch, AuthorizedKeys: sb.AuthorizedKeys, Limits: limits,
	}
	secret, keyID, err := m.mintKey(ctx, req, expires)
	if err != nil {
		return err
	}
	var git GitCredential
	rollback := m.undoStart(ctx, keyID, &git)

	row := sb
	row.KeyID = keyID
	env := m.environment(ctx, req, row, secret, expires)
	if sb.Repo != "" {
		if git, err = m.checkOut(ctx, env, req, expires); err != nil {
			rollback()
			return err
		}
	}

	if err := m.st.ReviveSandbox(ctx, sb.ID, keyID, git.ID, expires); err != nil {
		rollback()
		if errors.Is(err, store.ErrNotFound) {
			return refuse("sandbox %s is no longer expired; somebody else resumed or "+
				"terminated it", sb.Name)
		}
		return err
	}
	m.changed()

	err = m.driver.Revive(ctx, Spec{
		Ref: refOf(sb), Class: machine, Purpose: sb.Purpose,
		Owner: sb.Owner, Org: sb.OrgID, Project: sb.ProjectID,
		Env: env, Expires: expires,
	})
	if err != nil {
		rollback()
		if errors.Is(err, ErrNotFound) {
			m.fail(ctx, sb.ID, errors.New("the sandbox is no longer there; its volume went with it"))
			return refuse("sandbox %s is no longer there; its volume went with it", sb.Name)
		}
		// Back to expired, with its volume, and without the new key.
		if oerr := m.st.ObserveSandbox(context.WithoutCancel(ctx), sb.ID, store.SandboxObservation{
			State: policy.SandboxExpired, Detail: "resuming failed: " + err.Error(),
		}); oerr != nil {
			m.log.Warn("sandbox: recording a failed resume failed", "sandbox", sb.ID, "error", oerr)
		}
		return fmt.Errorf("resuming sandbox %s: %w", sb.Name, err)
	}
	status, err := m.driver.Status(ctx, refOf(sb))
	if err != nil {
		// The resume itself worked; the sweep will correct the row.
		m.log.Warn("sandbox: reading a resumed sandbox's state failed",
			"sandbox", sb.ID, "error", err)
		return nil
	}
	m.observe(ctx, sb.ID, stillStarting(status, expires, time.Now()))
	return nil
}

// lifetimeClass is the class that decides a sandbox's new lifetime: its class
// as it is now, or standIn if the class was deleted since.
func (m *Manager) lifetimeClass(ctx context.Context, sb store.Sandbox) (policy.SandboxClass, error) {
	class, err := m.st.SandboxClass(ctx, sb.OrgID, sb.Class)
	if errors.Is(err, store.ErrNotFound) {
		return standIn(sb), nil
	}
	return class, err
}

// standIn is the row's own copy of its class: the machine the sandbox is. It
// also stands in for a class deleted while the sandbox lived, which is
// allowed. The row does not record the class's lifetimes, so it gets those of
// a class that states none. Anything based on the sandbox's own age would
// grow with each extend or resume.
func standIn(sb store.Sandbox) policy.SandboxClass {
	return policy.SandboxClass{
		OrgID: sb.OrgID, Name: sb.Class, Image: sb.Image, Isolation: sb.Isolation,
		CPU: sb.CPU, Memory: sb.Memory, Disk: sb.Disk,
		DefaultTTL: catalog.DefaultSandboxTTL, MaxTTL: catalog.DefaultSandboxMaxTTL,
	}
}

// stillStarting reads a driver's "expired" for a sandbox whose new end is
// still ahead as the cluster not having caught up yet, rather than as the end.
func stillStarting(status Status, expires, now time.Time) Status {
	if status.State == policy.SandboxExpired && expires.After(now) {
		status.State, status.Detail = policy.SandboxPending, "waiting for the cluster to start it again"
	}
	return status
}

// Extend moves when a sandbox ends.
//
// The new lifetime counts from now, not from the old end, and is clamped by
// the same ceilings as creation. So extending again and again never adds up
// to a sandbox that lives for ever.
func (m *Manager) Extend(ctx context.Context, sb store.Sandbox, want time.Duration,
	limits policy.ResolvedSandbox,
) (time.Time, error) {
	if sb.State == policy.SandboxExpired {
		return time.Time{}, refuse("sandbox %s has expired; `keera sandbox resume %s` brings "+
			"it back with a new lifetime", sb.Name, sb.Name)
	}
	if !sb.State.Live() {
		return time.Time{}, refuse("sandbox %s is %s; its lifetime cannot be extended",
			sb.Name, sb.State)
	}
	class, err := m.lifetimeClass(ctx, sb)
	if err != nil {
		return time.Time{}, err
	}

	until := time.Now().Add(clampTTL(class, want, limits)).UTC()

	if err := m.driver.Extend(ctx, refOf(sb), until); err != nil {
		return time.Time{}, err
	}
	if err := m.st.SetSandboxExpiry(ctx, sb.ID, until); err != nil {
		return time.Time{}, err
	}
	return until, nil
}

// Terminate removes a sandbox and revokes its key.
//
// The driver goes first, so a failure leaves a working sandbox with a working
// key. A failed revoke only logs: the sandbox is gone and the key expires by
// itself.
func (m *Manager) Terminate(ctx context.Context, sb store.Sandbox) error {
	return m.terminate(ctx, sb, "terminated")
}

// terminate is Terminate, with the detail to leave on the row.
func (m *Manager) terminate(ctx context.Context, sb store.Sandbox, detail string) error {
	if sb.State == policy.SandboxTerminated {
		return nil
	}
	if err := m.driver.Terminate(ctx, refOf(sb)); err != nil {
		return err
	}
	m.release(ctx, sb)
	return m.st.ObserveSandbox(ctx, sb.ID, store.SandboxObservation{
		State: policy.SandboxTerminated, Detail: detail,
	})
}

// finish terminates an agent's sandbox whose task is done.
func (m *Manager) finish(ctx context.Context, sb store.Sandbox) {
	if err := m.terminate(ctx, sb, "the agent finished its task"); err != nil {
		m.log.Warn("sandbox: removing a finished agent sandbox failed",
			"sandbox", sb.ID, "error", err)
	}
}

// failed records a sandbox that stopped by itself and will not come back,
// and gives back what it held.
//
// An agent's is removed from the driver: its result is the branch it pushed,
// and nothing sweeps its row again. An engineer's keeps its volume until
// somebody terminates it, since it may hold work. It stays live meanwhile: it
// counts towards the quota and keeps its name, and the sweep notices if its
// volume vanishes. Either way the key and the repository credential are
// revoked.
func (m *Manager) failed(ctx context.Context, sb store.Sandbox, status Status) {
	if sb.Purpose == policy.PurposeAgent {
		if err := m.driver.Terminate(ctx, refOf(sb)); err != nil && !errors.Is(err, ErrNotFound) {
			// The row stays live, so the next sweep tries again.
			m.log.Warn("sandbox: removing a failed agent sandbox failed",
				"sandbox", sb.ID, "error", err)
			return
		}
	}
	m.release(ctx, sb)
	m.observe(ctx, sb.ID, status)
}

// Offboarded is what Offboard did.
type Offboarded struct {
	Terminated int `json:"terminated"`
	Suspended  int `json:"suspended"`
	// Failed is how many could not be stopped. The log says why.
	Failed int `json:"failed"`
}

// Offboard stops everything a person's sandboxes can still do, for when they
// are disabled. Their keys are already revoked by then.
//
// An agent's sandbox, or one that has not started, is terminated. An
// engineer's is suspended, because its volume may hold work somebody else
// needs; an administrator can terminate it later. Either way its repository
// credential is revoked.
func (m *Manager) Offboard(ctx context.Context, userID string) (Offboarded, error) {
	var done Offboarded
	live, err := m.st.ListSandboxes(ctx, store.SandboxQuery{UserID: userID, Limit: 500})
	if err != nil {
		return done, err
	}
	for _, sb := range live {
		keep := sb.Purpose == policy.PurposeEngineer && sb.State != policy.SandboxPending
		var err error
		switch {
		case !keep:
			if err = m.Terminate(ctx, sb); err == nil {
				done.Terminated++
			}
		default:
			if sb.State == policy.SandboxReady {
				err = m.Suspend(ctx, sb)
			}
			if err == nil {
				m.revokeGit(ctx, sb)
				// An expired or failed one is kept too, but is not suspended.
				if sb.State == policy.SandboxReady || sb.State == policy.SandboxSuspended {
					done.Suspended++
				}
			}
		}
		if err != nil {
			done.Failed++
			m.log.Warn("sandbox: stopping a disabled person's sandbox failed",
				"sandbox", sb.ID, "name", sb.Name, "error", err)
		}
	}
	return done, nil
}

// Dial opens a connection to a port on a running sandbox, for the attach
// surface.
func (m *Manager) Dial(ctx context.Context, sb store.Sandbox, port int) (net.Conn, error) {
	if !sb.Purpose.Attachable() {
		return nil, refuse("sandbox %s is an agent's; nothing attaches to one. Its whole claim "+
			"is that only a branch came out of it, and a shell nobody recorded would make that "+
			"claim unprovable", sb.Name)
	}
	if sb.State == policy.SandboxSuspended || sb.State == policy.SandboxExpired {
		return nil, refuse("sandbox %s is %s; `keera sandbox resume %s` brings it back, "+
			"with its volume as you left it", sb.Name, sb.State, sb.Name)
	}
	if sb.State != policy.SandboxReady {
		return nil, refuse("sandbox %s is %s: %s", sb.Name, sb.State, sb.Detail)
	}
	return m.driver.Dial(ctx, refOf(sb), port)
}

/* ---------------------------------------------------------------- the sweep */

// Sweep reconciles every live row with the driver, ends the expired ones,
// suspends idle ones and updates the running time.
//
// On Kubernetes the controller also enforces expiry; on podman this is the
// only thing that does.
func (m *Manager) Sweep(ctx context.Context, now time.Time) {
	// Accounting first, so a sandbox ended below is charged for its last
	// interval.
	if _, err := m.st.AccountSandboxes(ctx, now); err != nil {
		m.log.Warn("sandbox: accounting for running time failed", "error", err)
	}

	expired, err := m.st.SandboxesPastExpiry(ctx, now, 100)
	if err != nil {
		m.log.Warn("sandbox: reading expired sandboxes failed", "error", err)
	}
	for _, sb := range expired {
		m.endExpired(ctx, sb)
	}

	m.reconcilePools(ctx)

	const page = 500
	for after := ""; ; {
		live, err := m.st.LiveSandboxes(ctx, after, page)
		if err != nil {
			m.log.Warn("sandbox: reading live sandboxes failed", "error", err)
			return
		}
		for _, sb := range live {
			if ctx.Err() != nil {
				return
			}
			m.reconcile(ctx, sb, now)
		}
		if len(live) < page {
			return
		}
		after = live[len(live)-1].ID
	}
}

// endExpired ends a sandbox whose time ran out.
//
// An agent's is terminated: the task is over and its result is a branch. An
// engineer's is suspended and marked expired, because its volume holds work
// in progress. Its key and repository credential are revoked; resuming it
// gives it new ones.
func (m *Manager) endExpired(ctx context.Context, sb store.Sandbox) {
	if sb.Purpose == policy.PurposeAgent {
		if err := m.Terminate(ctx, sb); err != nil {
			m.log.Warn("sandbox: terminating an expired agent sandbox failed",
				"sandbox", sb.ID, "error", err)
		}
		return
	}
	if sb.State.Running() {
		if err := m.driver.Suspend(ctx, refOf(sb)); err != nil && !errors.Is(err, ErrNotFound) {
			m.log.Warn("sandbox: suspending an expired sandbox failed",
				"sandbox", sb.ID, "error", err)
		}
	}
	// The key goes now, not at termination: nothing legitimate uses it while
	// the sandbox is stopped.
	m.release(ctx, sb)
	if err := m.st.ObserveSandbox(ctx, sb.ID, store.SandboxObservation{
		State:  policy.SandboxExpired,
		Detail: "its lifetime ran out; resuming it brings it back, terminating it frees the volume",
	}); err != nil {
		m.log.Warn("sandbox: recording an expiry failed", "sandbox", sb.ID, "error", err)
	}
}

// reconcile writes back what the driver says about one sandbox.
//
// An expired or failed sandbox is only checked for having vanished. Its state
// is settled: to a driver an expired one looks suspended, and a failed one
// has already given back its key.
func (m *Manager) reconcile(ctx context.Context, sb store.Sandbox, now time.Time) {
	status, err := m.driver.Status(ctx, refOf(sb))
	settled := sb.State == policy.SandboxExpired || sb.State == policy.SandboxFailed
	if settled && !errors.Is(err, ErrNotFound) {
		return
	}
	switch {
	case errors.Is(err, ErrNotFound):
		// Gone. On Kubernetes that is usually the controller expiring it;
		// otherwise somebody removed it by hand. Either way, record the end.
		m.release(ctx, sb)
		m.observe(ctx, sb.ID, Status{Ref: refOf(sb), State: policy.SandboxTerminated,
			Detail: "no longer in the cluster"})
		return
	case err != nil:
		// An unreachable driver is not a dead sandbox. Leave the row alone, so
		// an API server hiccup does not mark everyone's machines as gone.
		m.log.Warn("sandbox: reading a sandbox's state failed", "sandbox", sb.ID, "error", err)
		return
	}
	// An agent that exited cleanly has done its task. It is never suspended,
	// so on podman a clean stop cannot be mistaken for one.
	if status.Exited && sb.Purpose == policy.PurposeAgent {
		m.finish(ctx, sb)
		return
	}
	if status.State == policy.SandboxFailed {
		m.failed(ctx, sb, status)
		return
	}
	if sb.ExpiresAt != nil {
		status = stillStarting(status, *sb.ExpiresAt, now)
	}
	m.observe(ctx, sb.ID, status)

	// Idle suspension, for an engineer's sandbox. It has to have been ready
	// when the row was read too: a sandbox that has only now come back is
	// not idle from before, and its row does not say so yet.
	if m.opts.IdleSuspend > 0 && status.State == policy.SandboxReady &&
		sb.State == policy.SandboxReady && sb.Purpose == policy.PurposeEngineer &&
		idleSince(sb).Add(m.opts.IdleSuspend).Before(now) {
		m.log.Info("sandbox: suspending an idle sandbox",
			"sandbox", sb.ID, "name", sb.Name, "idle_for", now.Sub(idleSince(sb)).Round(time.Minute))
		if err := m.Suspend(ctx, sb); err != nil {
			m.log.Warn("sandbox: suspending an idle sandbox failed", "sandbox", sb.ID, "error", err)
		}
	}
}

// idleSince is when a sandbox counts as idle from: the last time somebody
// was connected or it came back to ready, whichever is latest.
func idleSince(sb store.Sandbox) time.Time {
	since := sb.CreatedAt
	for _, t := range []*time.Time{sb.ReadyAt, sb.ActiveAt} {
		if t != nil && t.After(since) {
			since = *t
		}
	}
	return since
}

func (m *Manager) observe(ctx context.Context, sbID string, status Status) {
	if err := m.st.ObserveSandbox(ctx, sbID, store.SandboxObservation{
		State: status.State, Detail: status.Detail,
		Address: status.Address, Node: status.Node,
	}); err != nil {
		m.log.Warn("sandbox: writing back a sandbox's state failed", "sandbox", sbID, "error", err)
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

// stopClock stops a sandbox's running-time accounting.
func (m *Manager) stopClock(ctx context.Context, sbID string) {
	if err := m.st.StopAccounting(ctx, sbID, time.Now()); err != nil {
		m.log.Warn("sandbox: freezing the running clock failed", "sandbox", sbID, "error", err)
	}
}

// revokeKey revokes a sandbox's key, if it has one. A key already gone is
// fine; any other failure is logged.
func (m *Manager) revokeKey(ctx context.Context, sb store.Sandbox) {
	if sb.KeyID == "" {
		return
	}
	if err := m.st.RevokeKey(ctx, sb.KeyID); err != nil && !errors.Is(err, store.ErrNotFound) {
		m.log.Warn("sandbox: revoking a sandbox's key failed", "sandbox", sb.ID,
			"key_id", sb.KeyID, "state", sb.State, "error", err)
	}
	m.changed()
}

func (m *Manager) changed() {
	if m.opts.OnChange != nil {
		m.opts.OnChange()
	}
}

func refOf(sb store.Sandbox) Ref {
	return Ref{ID: sb.ID, Name: sb.Name, Backing: Backing(sb.Backing)}
}

// reconcilePools makes the warm pools match the catalogue. It runs on every
// sweep, so catalogue edits apply without a restart, a deleted pool comes back
// and the pool of a deleted class goes. Drivers without warm pools skip it.
func (m *Manager) reconcilePools(ctx context.Context) {
	pooler, ok := m.driver.(interface {
		EnsurePool(context.Context, policy.SandboxClass) error
		PrunePools(context.Context, map[string]bool) error
	})
	if !ok {
		return
	}
	if !m.driver.Capabilities().Warm {
		// Pools left from when warm pools were on would hold their CPU and
		// memory for ever. Turning them off takes a restart, so one try is
		// enough. Without the extension there is nothing to clear.
		m.pruneOnce.Do(func() {
			if err := pooler.PrunePools(ctx, nil); err != nil && !isNotFound(err) {
				m.log.Warn("sandbox: removing the warm pools failed; "+
					"they keep holding their CPU and memory", "error", err)
			}
		})
		return
	}
	classes, err := m.st.LoadSandboxClasses(ctx)
	if err != nil {
		m.log.Warn("sandbox: reading the catalogue to reconcile warm pools failed", "error", err)
		return
	}
	keep := make(map[string]bool, len(classes))
	for _, c := range classes {
		// A class with no warm pool is left out of keep, so PrunePools deletes
		// a pool it had once. Asking for its deletion every minute would be
		// two needless API calls per class and organisation.
		if c.Warm <= 0 {
			continue
		}
		keep[PoolKey(c.OrgID, c.Name)] = true
		if err := pooler.EnsurePool(ctx, c); err != nil {
			// Not fatal: a pool that already exists keeps serving claims, and
			// the next sweep tries again. Create does not fall back to a cold
			// start, so a class whose pool was never made cannot be claimed.
			m.log.Warn("sandbox: reconciling a warm pool failed",
				"org", c.OrgID, "class", c.Name, "warm", c.Warm, "error", err)
		}
	}
	if err := pooler.PrunePools(ctx, keep); err != nil {
		m.log.Warn("sandbox: removing the warm pools of deleted classes failed", "error", err)
	}
}

// piSettings is the Pi settings file that makes this gateway the provider a
// sandbox starts on.
func piSettings(alias string) string {
	raw, err := json.Marshal(map[string]any{
		"defaultProvider": piProvider,
		"defaultModel":    alias,
		// No install ping from inside a customer's cluster, as with Claude
		// Code's telemetry in internal/connect.
		"enableInstallTelemetry": false,
	})
	if err != nil {
		return ""
	}
	return string(raw)
}

// The fields of a sandbox's Pi configuration that must match what
// `keera connect pi` prints for a laptop. A test compares them with that
// template.
const (
	piProvider = "keera"
	piAPI      = "openai-completions"
	// piAPIKeyRef is expanded by Pi from the environment, so the file keeps
	// working when a resumed sandbox gets a new key.
	piAPIKeyRef = "${KEERA_API_KEY}"
)

type piFile struct {
	Providers map[string]piProviderConfig `json:"providers"`
}

type piProviderConfig struct {
	BaseURL string            `json:"baseUrl"`
	API     string            `json:"api"`
	APIKey  string            `json:"apiKey"`
	Headers map[string]string `json:"headers,omitempty"`
	Models  []piModel         `json:"models"`
}

type piModel struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextWindow int    `json:"contextWindow"`
}

// piConfig renders Pi's models.json for a sandbox: one entry per model the key
// may reach, so /model offers exactly what the guardrail allows.
//
// A session id, given for an agent's sandbox, is sent with every request, so
// the task is one session in `keera session list`.
//
// It is empty with no base URL or no models: no configuration is better than
// a wrong one.
func (m *Manager) piConfig(base string, models []policy.Model, session string) string {
	if base == "" || len(models) == 0 {
		return ""
	}
	entries := make([]piModel, 0, len(models))
	for _, mod := range models {
		// The window the gateway enforces, so Pi does not assume a bigger one.
		window, _ := connect.Limits(mod.MaxContext)
		entries = append(entries, piModel{
			ID: mod.Alias, Name: connect.Title(mod.Alias), ContextWindow: window,
		})
	}
	var headers map[string]string
	if session != "" {
		headers = map[string]string{httpx.SessionHeader: session}
	}
	raw, err := json.MarshalIndent(piFile{Providers: map[string]piProviderConfig{
		piProvider: {
			BaseURL: strings.TrimRight(base, "/") + "/v1",
			API:     piAPI,
			APIKey:  piAPIKeyRef,
			Headers: headers,
			Models:  entries,
		},
	}}, "", "  ")
	if err != nil {
		return ""
	}
	return string(raw)
}

// reachableModels is every enabled chat model this sandbox's key may use, in
// catalogue order.
//
// It resolves the same allow-list the inference path enforces (the
// organisation's, narrowed by the project's), so the sandbox offers only models
// that will answer. Embedding models are left out: a coding agent cannot use
// them.
func (m *Manager) reachableModels(ctx context.Context, req CreateRequest) []policy.Model {
	limits := func(scope policy.ScopeType, id string) *policy.Limits {
		if id == "" {
			return nil
		}
		lim, err := m.st.GetPolicy(ctx, scope, id)
		if err != nil {
			return nil
		}
		return &lim
	}
	// The key mintKey issues is a standard one, so it cannot reach a
	// subscription model either.
	resolved := policy.Resolve(
		policy.Key{OrgID: req.OrgID, ProjectID: req.ProjectID, UserID: req.UserID, Kind: policy.KeyStandard},
		limits(policy.ScopeOrg, req.OrgID),
		limits(policy.ScopeProject, req.ProjectID),
		nil,
	)

	catalogue, err := m.st.ListModels(ctx, req.OrgID)
	if err != nil {
		m.log.Warn("sandbox: reading the catalogue for the agent configuration failed",
			"error", err)
		return nil
	}
	return chatModelsFor(resolved, catalogue)
}

// chatModelsFor is every enabled chat model of catalogue that resolved may use.
func chatModelsFor(resolved *policy.Resolved, catalogue []policy.Model) []policy.Model {
	out := make([]policy.Model, 0, len(catalogue))
	for _, mod := range catalogue {
		if !mod.Enabled || mod.Kind != policy.KindChat || !resolved.MayUse(mod) {
			continue
		}
		out = append(out, mod)
	}
	return out
}
