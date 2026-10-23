package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/secret"
	"github.com/bespinian/keera-gateway/internal/store"
)

// call runs one handler as p, so what is under test is the handler's own
// authorization.
func (tn tenants) call(h handler, p *authn.Principal, method, target, body string) *httptest.ResponseRecorder {
	return serve(tn.ctx, h, p, method, target, body)
}

// A server belongs to one organisation: its administrators change it, its
// members see it without its address, and nobody else sees it at all.
func TestAnAdministratorManagesTheirOrganisationsMCPServers(t *testing.T) {
	tn := twoTenants(t)
	operator := &authn.Principal{Via: authn.MethodOperatorKey, Role: authn.RoleOperator}
	carol := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleAdmin,
		OrgID: "org_a", UserID: "user_carol"}
	alice := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleMember,
		OrgID: "org_a", UserID: "user_alice"}
	body := `{"url":"https://mcp.example.ch/mcp","description":"Issues"}`

	if w := tn.call(tn.srv.putMCPServer, alice, "PUT", "/v1/mcp-servers/github", body); w.Code != http.StatusForbidden {
		t.Errorf("a member got %d, want 403", w.Code)
	}
	if w := tn.call(tn.srv.putMCPServer, carol, "PUT", "/v1/mcp-servers/github?org_id=org_b", body); w.Code != http.StatusForbidden {
		t.Errorf("an administrator changed another organisation's server: %d", w.Code)
	}
	if w := tn.call(tn.srv.putMCPServer, operator, "PUT", "/v1/mcp-servers/github", body); w.Code != http.StatusBadRequest {
		t.Errorf("a server of no organisation got %d, want 400", w.Code)
	}
	if w := tn.call(tn.srv.putMCPServer, carol, "PUT", "/v1/mcp-servers/github", body); w.Code != http.StatusOK {
		t.Fatalf("the administrator got %d: %s", w.Code, w.Body)
	}
	if w := tn.call(tn.srv.putMCPServer, carol, "PUT", "/v1/mcp-servers/github",
		`{"url":"ftp://x"}`); w.Code != http.StatusBadRequest {
		t.Errorf("a bad URL got %d, want 400", w.Code)
	}

	list := func(p *authn.Principal, query string) []policy.MCPServer {
		t.Helper()
		w := tn.call(tn.srv.listMCPServers, p, "GET", "/v1/mcp-servers"+query, "")
		var out struct {
			Data []policy.MCPServer `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("list = %s", w.Body)
		}
		return out.Data
	}
	// A member may see which servers exist, but not where they are.
	if got := list(alice, ""); len(got) != 1 || got[0].URL != "" || !got[0].Enabled {
		t.Errorf("a member was shown %+v", got)
	}
	if got := list(carol, ""); len(got) != 1 || got[0].URL == "" {
		t.Errorf("the administrator was shown %+v, want the address", got)
	}
	if got := list(operator, "?org_id=org_b"); len(got) != 0 {
		t.Errorf("org_b has %+v, want none of org_a's", got)
	}
}

// An edit that does not mention the credential keeps it, and the answer says
// so. Otherwise the command line reports no credential right after a save.
func TestAPutAnswersWithTheStoredCredentialState(t *testing.T) {
	tn := twoTenants(t)
	box, err := secret.New("a-test-key-long-enough-to-be-one")
	if err != nil {
		t.Fatal(err)
	}
	tn.srv.opts.Secrets = box
	carol := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleAdmin,
		OrgID: "org_a", UserID: "user_carol"}

	put := func(h handler, path, body string) bool {
		t.Helper()
		w := tn.call(h, carol, "PUT", path, body)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT %s = %d: %s", path, w.Code, w.Body)
		}
		var out struct {
			HasAPIKey bool `json:"has_api_key"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.HasAPIKey
	}

	put(tn.srv.putMCPServer, "/v1/mcp-servers/github",
		`{"url":"https://mcp.example.ch/mcp","api_key":"secret"}`)
	if !put(tn.srv.putMCPServer, "/v1/mcp-servers/github",
		`{"url":"https://mcp.example.ch/mcp","description":"Issues"}`) {
		t.Error("an MCP server edit answered that no credential is stored")
	}

	body := `{"backends":["https://api.example.ch/v1"],"backend_model":"m","location":"ch"`
	put(tn.srv.putModel, "/v1/models/keera-frontier", body+`,"api_key":"secret"}`)
	if !put(tn.srv.putModel, "/v1/models/keera-frontier", body+`,"description":"Hosted"}`) {
		t.Error("a model edit answered that no credential is stored")
	}
}

// An edit that leaves out "enabled" does not switch a disabled server back on.
func TestAnEditKeepsAServerDisabled(t *testing.T) {
	tn := twoTenants(t)
	carol := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleAdmin,
		OrgID: "org_a", UserID: "user_carol"}
	put := func(body string) policy.MCPServer {
		t.Helper()
		w := tn.call(tn.srv.putMCPServer, carol, "PUT", "/v1/mcp-servers/github", body)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT = %d: %s", w.Code, w.Body)
		}
		var out policy.MCPServer
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	if !put(`{"url":"https://mcp.example.ch/mcp"}`).Enabled {
		t.Error("a new server started disabled")
	}
	put(`{"url":"https://mcp.example.ch/mcp","enabled":false}`)
	if put(`{"url":"https://mcp.example.ch/mcp","description":"Issues"}`).Enabled {
		t.Error("an edit switched a disabled server back on")
	}
}

// A stored credential goes only to the hosts it was entered for. Otherwise an
// administrator could repoint a server or model someone else set up and read
// its key.
func TestAStoredCredentialDoesNotMoveToANewHost(t *testing.T) {
	tn := twoTenants(t)
	box, err := secret.New("a-test-key-long-enough-to-be-one")
	if err != nil {
		t.Fatal(err)
	}
	tn.srv.opts.Secrets = box
	carol := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleAdmin,
		OrgID: "org_a", UserID: "user_carol"}
	put := func(h handler, path, body string) int {
		t.Helper()
		return tn.call(h, carol, "PUT", path, body).Code
	}

	const mcpPath = "/v1/mcp-servers/github"
	put(tn.srv.putMCPServer, mcpPath, `{"url":"https://mcp.example.ch/mcp","api_key":"secret"}`)
	if code := put(tn.srv.putMCPServer, mcpPath, `{"url":"https://mcp.example.ch/v2/mcp"}`); code != http.StatusOK {
		t.Errorf("a new path on the same host = %d, want 200", code)
	}
	if code := put(tn.srv.putMCPServer, mcpPath, `{"url":"https://attacker.example/mcp"}`); code != http.StatusBadRequest {
		t.Errorf("a new host without the key = %d, want 400", code)
	}
	if code := put(tn.srv.putMCPServer, mcpPath, `{"url":"https://attacker.example/mcp","api_key":""}`); code != http.StatusOK {
		t.Errorf("a new host with the key removed = %d, want 200", code)
	}

	const modelPath = "/v1/models/keera-frontier"
	body := func(backends string) string {
		return `{"backends":[` + backends + `],"backend_model":"m","location":"ch"`
	}
	put(tn.srv.putModel, modelPath, body(`"https://api.example.ch/v1"`)+`,"api_key":"secret"}`)
	if code := put(tn.srv.putModel, modelPath, body(`"https://api.example.ch/v1","https://attacker.example/v1"`)+`}`); code != http.StatusBadRequest {
		t.Errorf("a second backend on a new host without the key = %d, want 400", code)
	}
	if code := put(tn.srv.putModel, modelPath, body(`"https://attacker.example/v1"`)+`,"api_key":"other"}`); code != http.StatusOK {
		t.Errorf("a new host with a new key = %d, want 200", code)
	}
}

func TestAToolAllowListMustNameRealServers(t *testing.T) {
	tn := twoTenants(t)
	if err := tn.srv.st.UpsertMCPServer(tn.ctx, policy.MCPServer{
		OrgID: "org_a", Alias: "github", URL: "https://mcp.example.ch/mcp", Enabled: true,
	}, store.Credential{}); err != nil {
		t.Fatal(err)
	}
	carol := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleAdmin,
		OrgID: "org_a", UserID: "user_carol"}

	w := tn.call(tn.srv.putGuardrails, carol, "PUT", "/v1/guardrails/org/org_a",
		`{"allowed_tools":["githbu/search"]}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "githbu") {
		t.Errorf("a typo got %d: %s", w.Code, w.Body)
	}
	// Another organisation's server is not one this one can allow.
	if err := tn.srv.st.UpsertMCPServer(tn.ctx, policy.MCPServer{
		OrgID: "org_b", Alias: "jira", URL: "https://jira.example.ch/mcp", Enabled: true,
	}, store.Credential{}); err != nil {
		t.Fatal(err)
	}
	w = tn.call(tn.srv.putGuardrails, carol, "PUT", "/v1/guardrails/org/org_a",
		`{"allowed_tools":["jira"]}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("allowing another organisation's server got %d: %s", w.Code, w.Body)
	}
	w = tn.call(tn.srv.putGuardrails, carol, "PUT", "/v1/guardrails/org/org_a",
		`{"allowed_tools":["github/search_code"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	w = tn.call(tn.srv.effectiveGuardrails, carol, "GET", "/v1/guardrails/org/org_a/effective", "")
	var eff Effective
	if err := json.Unmarshal(w.Body.Bytes(), &eff); err != nil {
		t.Fatal(err)
	}
	if len(eff.AllowedTools) != 1 {
		t.Errorf("effective = %+v", eff)
	}
}
