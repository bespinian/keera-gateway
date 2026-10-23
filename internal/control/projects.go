package control

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/id"
	"github.com/bespinian/keera-gateway/internal/store"
)

func (s *Server) createProject(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	var in struct {
		OrgID       string `json:"org_id"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	name, ok := requiredName(w, in.Name)
	if !ok {
		return
	}
	orgID, ok := s.orgToAdmin(w, p, in.OrgID)
	if !ok {
		return
	}
	project, err := s.st.CreateProject(r.Context(), store.Project{
		ID: id.New("project"), OrgID: orgID, Name: name,
		Description: strings.TrimSpace(in.Description),
	})
	if errors.Is(err, store.ErrProjectNameTaken) {
		projectNameTaken(w, name)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, orgID, "project.create", "project", project.ID, project)
	httpx.WriteJSON(w, http.StatusCreated, project)
}

// listProjects returns projects with their guardrails and current spend, which is
// what the panel shows on one screen.
func (s *Server) listProjects(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.scopeOrg(w, p, r.URL.Query().Get("org_id"))
	if !ok {
		return
	}
	projects, err := s.st.ProjectSummaries(r.Context(), orgID, time.Now())
	if err != nil {
		s.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": projects})
}

// projectOrg resolves the organisation of a project named in the path and checks
// that the caller administers it, before anything is written.
func (s *Server) projectOrg(w http.ResponseWriter, r *http.Request, p *authn.Principal,
	projectID string,
) (string, bool) {
	// A caller who administers nothing is refused before the read, so a
	// member cannot probe which project ids exist.
	if !s.requireAdmin(w, p) {
		return "", false
	}
	owner, err := s.st.ProjectOrg(r.Context(), projectID)
	if err != nil {
		s.fail(w, err)
		return "", false
	}
	if !s.requireOwnerAdmin(w, p, owner) {
		return "", false
	}
	return owner, true
}

// requireProjectInOrg refuses a project from another organisation. The foreign key
// only says that the project exists somewhere.
func (s *Server) requireProjectInOrg(w http.ResponseWriter, r *http.Request, projectID, orgID string) bool {
	owner, err := s.st.ProjectOrg(r.Context(), projectID)
	return s.inOrg(w, orgID, owner, err)
}

// updateProject renames a project, changes its description, or both: the
// only fields of a project that are not an id or a guardrail.
func (s *Server) updateProject(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	var in struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Name == nil && in.Description == nil {
		badRequest(w, "send 'name', 'description' or both; an empty 'description' clears it")
		return
	}
	if !renamed(w, in.Name) {
		return
	}
	if in.Description != nil {
		description := strings.TrimSpace(*in.Description)
		in.Description = &description
	}
	projectID := r.PathValue("id")
	orgID, ok := s.projectOrg(w, r, p, projectID)
	if !ok {
		return
	}
	project, err := s.st.UpdateProject(r.Context(), writeOrg(p), projectID,
		store.ProjectChange{Name: in.Name, Description: in.Description})
	if errors.Is(err, store.ErrProjectNameTaken) {
		projectNameTaken(w, *in.Name)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, orgID, "project.update", "project", projectID, project)
	httpx.WriteJSON(w, http.StatusOK, project)
}

// firstProject returns the project a sandbox goes in when none was named: the
// organisation's oldest, as for keys. It answers the refusal itself when the
// organisation has no project.
func (s *Server) firstProject(w http.ResponseWriter, r *http.Request, orgID string) (string, bool) {
	projectID, err := s.st.FirstProject(r.Context(), orgID)
	if errors.Is(err, store.ErrNotFound) {
		noProject(w)
		return "", false
	}
	if err != nil {
		s.fail(w, err)
		return "", false
	}
	return projectID, true
}

// noProject refuses a key or a sandbox in an organisation with no projects.
// Every one is in a project, so there can be none until a project is created.
func noProject(w http.ResponseWriter) {
	conflict(w, "project_required",
		"this organisation has no projects, and every key and sandbox is in one; "+
			"create a project first")
}

// projectNameTaken refuses a second project of one name. Two would make
// every report that names a project ambiguous.
func projectNameTaken(w http.ResponseWriter, name string) {
	conflict(w, "project_name_taken",
		"another project in this organisation is already called '"+name+"'")
}

// deleteProject removes a project once no live key is bound to it. A refusal
// names the keys, because "in use" alone leaves the administrator guessing.
func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	projectID := r.PathValue("id")
	orgID, ok := s.projectOrg(w, r, p, projectID)
	if !ok {
		return
	}
	gone, err := s.st.DeleteProject(r.Context(), writeOrg(p), projectID)
	if errors.Is(err, store.ErrProjectHasSandboxes) {
		conflict(w, "project_has_sandboxes",
			fmt.Sprintf("'%s' still has %d live sandbox(es), expired ones included; "+
				"terminate them first", gone.Name, gone.LiveSandboxes))
		return
	}
	if inUse, ok := errors.AsType[*store.ProjectInUseError](err); ok {
		conflict(w, "project_has_keys",
			"revoke the keys of '"+inUse.Project+"' first: "+strings.Join(inUse.Keys, ", "))
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, orgID, "project.delete", "project", projectID, gone)
	// The project's guardrails are gone, so the gateways must drop them now.
	s.changed(r, orgID)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id": gone.ID, "name": gone.Name, "deleted": true,
		"detached_keys": gone.DetachedKeys,
	})
}
