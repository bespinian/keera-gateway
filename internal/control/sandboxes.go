package control

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/catalog"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/sandbox"
	"github.com/bespinian/keera-gateway/internal/store"
)

// The sandbox half of the control API. The real work lives in
// internal/sandbox; this decides who may do what and maps refusals to status
// codes.
//
// The rule differs from the rest of the control plane: an administrator may
// see and terminate every sandbox in their organisation (it is their quota and
// their bill), but may not open a shell in one. See canAttach.

// requireSandboxes refuses every route that needs a driver when there is none.
func (s *Server) requireSandboxes(w http.ResponseWriter) bool {
	if s.opts.Sandboxes == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "invalid_request_error",
			"sandboxes_disabled",
			"this deployment runs no sandbox driver; set KEERA_SANDBOX_DRIVER to 'kubernetes' "+
				"or 'podman' to switch sandboxes on")
		return false
	}
	return true
}

/* --------------------------------------------------------------- the catalogue */

// listSandboxClasses is readable by anyone in the organisation: a developer
// needs the class names to ask for a machine. The caller's own limits come
// with it, so a client can grey out what they may not use.
func (s *Server) listSandboxClasses(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.queryOrg(w, r, p)
	if !ok {
		return
	}
	classes, err := s.st.ListSandboxClasses(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := map[string]any{"data": classes}
	if s.opts.Sandboxes != nil {
		caps := s.opts.Sandboxes.Driver().Capabilities()
		out["driver"] = map[string]any{
			"name": s.opts.Sandboxes.Driver().Name(),
			// What the driver can deliver, so the panel does not offer a
			// class the driver would refuse.
			"isolation": caps.Isolation,
			"tiers":     caps.Tiers,
		}
	}
	limits, err := s.sandboxLimits(r.Context(), orgID, "")
	if err != nil {
		s.fail(w, err)
		return
	}
	out["limits"] = limits
	httpx.WriteJSON(w, http.StatusOK, out)
}

// putSandboxClass creates or replaces one of an organisation's classes.
func (s *Server) putSandboxClass(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.adminOrg(w, r, p)
	if !ok {
		return
	}
	// The shape the list returns, so a class read can be written back.
	var in policy.SandboxClass
	if err := httpx.ReadJSON(r, &in); err != nil {
		badRequest(w, err.Error())
		return
	}
	in.Name = r.PathValue("name")

	class, err := catalog.CheckSandboxClass(in)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	class.OrgID = orgID
	if err := s.st.UpsertSandboxClass(r.Context(), &class); err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, orgID, "sandbox_class.put", "sandbox_class", class.Name, class)
	// No cache to clear: the gateway never holds sandbox classes, and each new
	// sandbox reads its class from the database.
	httpx.WriteJSON(w, http.StatusOK, class)
}

func (s *Server) deleteSandboxClass(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.adminOrg(w, r, p)
	if !ok {
		return
	}
	name := r.PathValue("name")
	// Unlike a filter, a class in use can be deleted: running sandboxes keep
	// their own copy of it. The answer says how many are still running.
	live, err := s.st.SandboxClassInUse(r.Context(), orgID, name)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.st.DeleteSandboxClass(r.Context(), orgID, name); err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, orgID, "sandbox_class.delete", "sandbox_class", name,
		map[string]any{"live_sandboxes": live})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"name": name, "deleted": true, "live_sandboxes": live,
	})
}

/* --------------------------------------------------------------- the sandboxes */

// sandboxLimits resolves what a sandbox may be: the organisation's guardrail,
// narrowed by the project's. The caller is a person, not an API key, so there
// is no key level.
func (s *Server) sandboxLimits(ctx context.Context, orgID, projectID string) (
	policy.ResolvedSandbox, error,
) {
	org, err := s.storedLimits(ctx, policy.ScopeOrg, orgID)
	if err != nil {
		return policy.ResolvedSandbox{}, err
	}
	var project *policy.Limits
	if projectID != "" {
		lim, err := s.storedLimits(ctx, policy.ScopeProject, projectID)
		if err != nil {
			return policy.ResolvedSandbox{}, err
		}
		project = &lim
	}
	return policy.Resolve(policy.Key{OrgID: orgID, ProjectID: projectID}, &org, project, nil).Sandbox, nil
}

func (s *Server) listSandboxes(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	v := r.URL.Query()
	orgID, ok := s.scopeOrg(w, p, v.Get("org_id"))
	if !ok {
		return
	}
	q := store.SandboxQuery{
		OrgID:     orgID,
		ProjectID: v.Get("project_id"),
		Class:     v.Get("class"),
		Purpose:   policy.Purpose(v.Get("purpose")),
		All:       httpx.Flag(v, "all"),
	}
	// A member sees only their own. An administrator sees the whole
	// organisation's, but still cannot attach to them.
	if !p.CanAdminOrg(orgID) {
		if p.UserID == "" {
			forbid(w, "this account is not attached to a person, so it has no sandboxes of "+
				"its own to list")
			return
		}
		q.UserID = p.UserID
	} else if u := v.Get("user_id"); u != "" {
		q.UserID = u
	}

	list, err := s.st.ListSandboxes(r.Context(), q)
	if err != nil {
		s.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": list})
}

func (s *Server) getSandbox(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	sb, ok := s.sandboxToChange(w, r, p)
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, sb)
}

// mayHandleSandbox is the rule for reading and changing a sandbox. An
// administrator may see, terminate, suspend and extend any sandbox in their
// organisation, as those are quota and cost decisions. A member may do that
// with their own. Getting inside one is stricter: see canAttach.
func mayHandleSandbox(p *authn.Principal, sb store.Sandbox) bool {
	return p.CanAdminOrg(sb.OrgID) || (sb.UserID != "" && sb.UserID == p.UserID)
}

// canSeeSandbox applies mayHandleSandbox, and answers for a sandbox the
// caller may not handle.
func (s *Server) canSeeSandbox(w http.ResponseWriter, p *authn.Principal, sb store.Sandbox) bool {
	if mayHandleSandbox(p, sb) {
		return true
	}
	// 404, not 403, so nobody can learn which sandboxes a colleague has.
	s.fail(w, store.ErrNotFound)
	return false
}

// sandboxToChange resolves the sandbox a request names and checks that the
// caller may see and change it.
func (s *Server) sandboxToChange(w http.ResponseWriter, r *http.Request, p *authn.Principal) (
	store.Sandbox, bool,
) {
	sb, ok := s.resolveSandbox(w, r, p)
	if !ok || !s.canSeeSandbox(w, p, sb) {
		return store.Sandbox{}, false
	}
	return sb, true
}

// sandboxTTL reads a requested lifetime. Empty means the default, which the
// manager decides.
func sandboxTTL(w http.ResponseWriter, v string) (time.Duration, bool) {
	if v == "" {
		return 0, true
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		badRequest(w, "'ttl' must be a positive duration such as 4h")
		return 0, false
	}
	return d, true
}

func (s *Server) createSandbox(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if !s.requireSandboxes(w) {
		return
	}
	var in struct {
		OrgID     string         `json:"org_id"`
		ProjectID string         `json:"project_id"`
		Name      string         `json:"name"`
		Class     string         `json:"class"`
		Purpose   policy.Purpose `json:"purpose"`
		TTL       string         `json:"ttl"`
		Repo      string         `json:"repo"`
		Branch    string         `json:"branch"`
		Task      string         `json:"task"`
		// AuthorizedKeys are the ssh public keys that may open a shell in it.
		AuthorizedKeys []string          `json:"authorized_keys"`
		Env            map[string]string `json:"env"`
	}
	if err := httpx.ReadJSON(r, &in); err != nil {
		badRequest(w, err.Error())
		return
	}
	orgID, ok := s.requireOrg(w, p, in.OrgID,
		"choose an organisation first; a sandbox belongs to one, and so do its quota "+
			"and its bill")
	if !ok {
		return
	}
	// As with keys, a member may not pick the project: the project holds the
	// guardrail, the budget and the sandbox quota.
	if in.ProjectID != "" && !p.CanAdminOrg(orgID) {
		forbid(w, "a member cannot choose the project a sandbox belongs to; it is created "+
			"in this organisation's oldest project")
		return
	}
	// A sandbox gets a key. On another tenant's project, that key would use their
	// guardrails, budget, rate limit and quota.
	if in.ProjectID != "" && !s.requireProjectInOrg(w, r, in.ProjectID, orgID) {
		return
	}
	// Its key needs a project, and the sandbox has to count against the same
	// one, so it is resolved here rather than when the key is stored.
	if in.ProjectID == "" {
		if in.ProjectID, ok = s.firstProject(w, r, orgID); !ok {
			return
		}
	}
	ttl, ok := sandboxTTL(w, in.TTL)
	if !ok {
		return
	}
	limits, err := s.sandboxLimits(r.Context(), orgID, in.ProjectID)
	if err != nil {
		s.fail(w, err)
		return
	}

	sb, err := s.opts.Sandboxes.Create(r.Context(), sandbox.CreateRequest{
		OrgID: orgID, ProjectID: in.ProjectID, UserID: p.UserID, Owner: p.Email,
		Name: strings.TrimSpace(in.Name), Class: strings.TrimSpace(in.Class),
		Purpose: in.Purpose, TTL: ttl,
		Repo: strings.TrimSpace(in.Repo), Branch: strings.TrimSpace(in.Branch),
		Task: in.Task, AuthorizedKeys: in.AuthorizedKeys, Limits: limits, Env: in.Env,
	})
	if err != nil {
		s.failSandbox(w, err)
		return
	}
	// The task stays out of the audit log. It describes the developer's own
	// code, which this product promises not to keep, and audit entries are
	// kept for years.
	s.auditf(r, p, orgID, "sandbox.create", "sandbox", sb.ID, map[string]any{
		"name": sb.Name, "class": sb.Class, "purpose": sb.Purpose,
		"isolation": sb.Isolation, "expires_at": sb.ExpiresAt, "repo": sb.Repo,
	})
	httpx.WriteJSON(w, http.StatusCreated, sb)
}

// terminateSandbox ends one sandbox. The route is a DELETE, but the words say
// terminate, because the row outlives the machine.
func (s *Server) terminateSandbox(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if !s.requireSandboxes(w) {
		return
	}
	sb, ok := s.sandboxToChange(w, r, p)
	if !ok {
		return
	}
	if err := s.opts.Sandboxes.Terminate(r.Context(), sb); err != nil {
		s.failSandbox(w, err)
		return
	}
	s.auditf(r, p, sb.OrgID, "sandbox.terminate", "sandbox", sb.ID, map[string]any{
		"name": sb.Name, "class": sb.Class,
		"running_seconds": sb.RunningSeconds, "core_seconds": sb.CoreSeconds(),
	})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id": sb.ID, "name": sb.Name, "terminated": true,
	})
}

func (s *Server) extendSandbox(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if !s.requireSandboxes(w) {
		return
	}
	var in struct {
		TTL string `json:"ttl"`
	}
	// An empty body is fine: `keera sandbox extend <name>` sends none.
	if err := httpx.ReadJSON(r, &in); err != nil && !errors.Is(err, io.EOF) {
		badRequest(w, err.Error())
		return
	}
	sb, ok := s.sandboxToChange(w, r, p)
	if !ok {
		return
	}
	ttl, ok := sandboxTTL(w, in.TTL)
	if !ok {
		return
	}
	limits, err := s.sandboxLimits(r.Context(), sb.OrgID, sb.ProjectID)
	if err != nil {
		s.fail(w, err)
		return
	}
	until, err := s.opts.Sandboxes.Extend(r.Context(), sb, ttl, limits)
	if err != nil {
		s.failSandbox(w, err)
		return
	}
	s.auditf(r, p, sb.OrgID, "sandbox.extend", "sandbox", sb.ID, map[string]any{
		"name": sb.Name, "expires_at": until,
	})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id": sb.ID, "name": sb.Name, "expires_at": until,
	})
}

func (s *Server) suspendSandbox(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	s.changeSandboxState(w, r, p, "suspend")
}

func (s *Server) resumeSandbox(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	s.changeSandboxState(w, r, p, "resume")
}

func (s *Server) changeSandboxState(w http.ResponseWriter, r *http.Request, p *authn.Principal,
	action string,
) {
	if !s.requireSandboxes(w) {
		return
	}
	sb, ok := s.sandboxToChange(w, r, p)
	if !ok {
		return
	}
	var err error
	if action == "suspend" {
		err = s.opts.Sandboxes.Suspend(r.Context(), sb)
	} else {
		// An expired sandbox is held to the guardrail again, as it is now.
		var limits policy.ResolvedSandbox
		if limits, err = s.sandboxLimits(r.Context(), sb.OrgID, sb.ProjectID); err != nil {
			s.fail(w, err)
			return
		}
		err = s.opts.Sandboxes.Resume(r.Context(), sb, limits)
	}
	if err != nil {
		s.failSandbox(w, err)
		return
	}
	s.auditf(r, p, sb.OrgID, "sandbox."+action, "sandbox", sb.ID,
		map[string]any{"name": sb.Name})

	// Both actions finish in the background, so the answer is the state read
	// back now, not the state asked for.
	fresh, err := s.st.Sandbox(r.Context(), sb.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, fresh)
}

// sandboxUsage is what sandboxes have cost, grouped by project, person or class.
func (s *Server) sandboxUsage(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	q := r.URL.Query()
	orgID, ok := s.scopeOrg(w, p, q.Get("org_id"))
	if !ok {
		return
	}
	if orgID != "" && !p.CanAdminOrg(orgID) {
		forbid(w, "sandbox usage spans everybody in the organisation; reading it is an "+
			"administrator's")
		return
	}
	from, to, ok := queryWindow(w, q)
	if !ok {
		return
	}
	// An unknown grouping is refused, as on /v1/usage, rather than silently
	// replaced while the caller's string is echoed back.
	groupBy := q.Get("group_by")
	if groupBy == "" {
		groupBy = "class"
	}
	if !store.ValidSandboxGroupBy(groupBy) {
		badRequest(w, "'group_by' must be one of "+strings.Join(store.SandboxGroupBys(), ", "))
		return
	}
	rows, err := s.st.SandboxUsageBy(r.Context(), orgID, groupBy, from, to)
	if err != nil {
		s.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"data": rows, "group_by": groupBy, "from": from, "to": to,
	})
}
