package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

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
	// Username and Token make an HTTPS credential.
	Username string
	Token    string
	// ID is the forge's id for the credential, for a minter that can revoke
	// it. It is stored; the credential is not.
	ID string
	// Expires is when the forge stops accepting it. Zero means never, or
	// unknown.
	Expires time.Time
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
	return cred, nil
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
	env["KEERA_REPO"] = cred.Repo
	if req.Branch != "" {
		env["KEERA_BRANCH"] = req.Branch
	}
	env["KEERA_GIT_USERNAME"] = cred.Username
	env["KEERA_GIT_TOKEN"] = cred.Token
	if !cred.Expires.IsZero() {
		env["KEERA_GIT_TOKEN_EXPIRES"] = strconv.FormatInt(cred.Expires.Unix(), 10)
	}
	// Where the sandbox gets a fresh token when this one runs out. A GitHub
	// token lasts an hour and an engineer's sandbox a working day.
	if base := m.opts.PublicURL; base != "" {
		env["KEERA_GIT_CREDENTIAL_URL"] = base + httpx.SandboxPrefix + GitCredentialPath
	}
	return cred, nil
}

// GitCredentialPath is where a sandbox asks for a fresh repository credential,
// under httpx.SandboxPrefix.
const GitCredentialPath = "/v1/git-credential"

// allowRepo checks a repository against a scope's allowed_repos.
func allowRepo(limits policy.ResolvedSandbox) func(string) error {
	return func(path string) error {
		if err := limits.AdmitsRepo(path); err != nil {
			return Refuse("%v", err)
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
		return GitCredential{}, Refuse("sandbox %s was created without a repository, so it "+
			"has no repository credential", sb.Name)
	case m.opts.Git == nil:
		return GitCredential{}, Refuse("this deployment has no way to mint a repository credential")
	case !sb.State.Running():
		return GitCredential{}, Refuse("sandbox %s is %s; only a running sandbox gets a "+
			"repository credential", sb.Name, sb.State)
	case sb.ExpiresAt == nil || !sb.ExpiresAt.After(time.Now()):
		return GitCredential{}, Refuse("sandbox %s has run out of time", sb.Name)
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
