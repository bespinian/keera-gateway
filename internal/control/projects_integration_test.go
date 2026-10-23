package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/store"
)

// changeProject runs one of the two handlers that name a project in the path, on
// project_a, the way the router would.
func (tn tenants) changeProject(h handler, p *authn.Principal, method, body string) *httptest.ResponseRecorder {
	return tn.call(h, p, method, "/v1/projects/project_a", body)
}

// A project's organisation comes from its row, not the request, so these
// routes must look it up.
func TestAProjectIsChangedAndDeletedOnlyInsideItsOwnTenant(t *testing.T) {
	tn := twoTenants(t)
	st := tn.srv.st
	for _, project := range []struct{ id, org, name string }{
		{"project_a", "org_a", "Payments Platform"},
		{"project_a2", "org_a", "Data Science"},
	} {
		if _, err := st.CreateProject(tn.ctx, store.Project{ID: project.id, OrgID: project.org, Name: project.name}); err != nil {
			t.Fatalf("CreateProject %s: %v", project.id, err)
		}
	}

	// The other customer's administrator is told it is missing, so they cannot
	// confirm the id exists.
	w := tn.changeProject(tn.srv.updateProject, admin("org_b"), http.MethodPatch,
		`{"name":"Ours Now"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("another tenant's administrator changed it: status = %d, want 404: %s", w.Code, w.Body)
	}

	w = tn.changeProject(tn.srv.updateProject, admin("org_a"), http.MethodPatch,
		`{"name":"Payments","description":"Card payments"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d changing a project in the caller's own tenant, want 200: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"description":"Card payments"`) {
		t.Errorf("the change did not keep the description: %s", w.Body)
	}
	names, err := st.ProjectNames(tn.ctx, "org_a")
	if err != nil || names["project_a"] != "Payments" {
		t.Errorf("after the change ProjectNames = %v, %v", names, err)
	}

	// The name a sibling project already holds comes back as a conflict that
	// says which name and why, not as an internal error over a unique index.
	w = tn.changeProject(tn.srv.updateProject, admin("org_a"), http.MethodPatch,
		`{"name":"Data Science"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d renaming onto a name in use, want 409: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "Data Science") {
		t.Errorf("the conflict did not name the name: %s", w.Body)
	}
}

// Deleting a project with a working key would take the key with it, so it is
// refused, naming the keys to deal with first.
func TestDeletingAProjectIsRefusedWhileItHoldsAWorkingKey(t *testing.T) {
	tn := twoTenants(t)
	st := tn.srv.st
	if _, err := st.CreateProject(tn.ctx, store.Project{ID: "project_a", OrgID: "org_a", Name: "Payments Platform"}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := st.CreateKey(tn.ctx, store.KeyInfo{
		ID: "key_ci", OrgID: "org_a", ProjectID: "project_a",
		Name: "the build pipeline", Prefix: "sk-ci",
	}, []byte("hash-of-key_ci")); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	w := tn.changeProject(tn.srv.deleteProject, admin("org_a"), http.MethodDelete, "")
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d deleting a project with a live key, want 409: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "the build pipeline") {
		t.Errorf("the refusal did not name the key: %s", w.Body)
	}

	if err := st.RevokeKey(tn.ctx, store.AnyOrg, "key_ci"); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	w = tn.changeProject(tn.srv.deleteProject, admin("org_a"), http.MethodDelete, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d deleting a project whose keys are revoked, want 200: %s", w.Code, w.Body)
	}
	var body struct {
		Deleted      bool `json:"deleted"`
		DetachedKeys int  `json:"detached_keys"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	if !body.Deleted || body.DetachedKeys != 1 {
		t.Errorf("the deletion reported %+v", body)
	}
	if names, err := st.ProjectNames(tn.ctx, "org_a"); err != nil || names["project_a"] != "" {
		t.Errorf("after the deletion ProjectNames = %v, %v", names, err)
	}
}
