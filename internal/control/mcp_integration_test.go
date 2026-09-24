package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
)

// call runs one handler directly, as p, so what is under test is the
// handler's own authorization.
func (tn tenants) call(h handler, p *authn.Principal, method, path, body string,
	values map[string]string,
) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, httpx.ControlPrefix+path, strings.NewReader(body)).
		WithContext(tn.ctx)
	for k, v := range values {
		r.SetPathValue(k, v)
	}
	h(w, r, p)
	return w
}

func TestOnlyAnOperatorChangesTheMCPCatalogue(t *testing.T) {
	tn := twoTenants(t)
	if _, err := tn.srv.st.Pool().Exec(tn.ctx, "TRUNCATE mcp_servers"); err != nil {
		t.Fatal(err)
	}
	operator := &authn.Principal{Via: authn.MethodOperatorKey, Role: authn.RoleOperator}
	carol := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleAdmin,
		OrgID: "org_a", UserID: "user_carol"}
	body := `{"url":"https://mcp.example.ch/mcp","description":"Issues","api_key_env":"GH_TOKEN"}`
	path := map[string]string{"alias": "github"}

	if w := tn.call(tn.srv.putMCPServer, carol, "PUT", "/v1/mcp-servers/github", body, path); w.Code != http.StatusForbidden {
		t.Errorf("an organisation's administrator got %d, want 403", w.Code)
	}
	if w := tn.call(tn.srv.putMCPServer, operator, "PUT", "/v1/mcp-servers/github", body, path); w.Code != http.StatusOK {
		t.Fatalf("operator got %d: %s", w.Code, w.Body)
	}
	if w := tn.call(tn.srv.putMCPServer, operator, "PUT", "/v1/mcp-servers/github",
		`{"url":"ftp://x"}`, path); w.Code != http.StatusBadRequest {
		t.Errorf("a bad URL got %d, want 400", w.Code)
	}

	// Anybody may see which servers exist, but not where they are.
	w := tn.call(tn.srv.listMCPServers, carol, "GET", "/v1/mcp-servers", "", nil)
	var list struct {
		Data []policy.MCPServer `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Data) != 1 {
		t.Fatalf("list = %s", w.Body)
	}
	if list.Data[0].URL != "" || list.Data[0].APIKeyEnv != "" || !list.Data[0].Enabled {
		t.Errorf("an administrator was shown %+v", list.Data[0])
	}
}

func TestAToolAllowListMustNameRealServers(t *testing.T) {
	tn := twoTenants(t)
	if _, err := tn.srv.st.Pool().Exec(tn.ctx, "TRUNCATE mcp_servers"); err != nil {
		t.Fatal(err)
	}
	if err := tn.srv.st.UpsertMCPServer(tn.ctx, policy.MCPServer{
		Alias: "github", URL: "https://mcp.example.ch/mcp", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	carol := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleAdmin,
		OrgID: "org_a", UserID: "user_carol"}
	path := map[string]string{"scope": "org", "id": "org_a"}

	w := tn.call(tn.srv.putGuardrails, carol, "PUT", "/v1/guardrails/org/org_a",
		`{"allowed_tools":["githbu/search"]}`, path)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "githbu") {
		t.Errorf("a typo got %d: %s", w.Code, w.Body)
	}
	w = tn.call(tn.srv.putGuardrails, carol, "PUT", "/v1/guardrails/org/org_a",
		`{"allowed_tools":["github/search_code"],"block_hosted_tools":true}`, path)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	w = tn.call(tn.srv.effectiveGuardrails, carol, "GET", "/v1/guardrails/org/org_a/effective", "", path)
	var eff Effective
	if err := json.Unmarshal(w.Body.Bytes(), &eff); err != nil {
		t.Fatal(err)
	}
	if len(eff.AllowedTools) != 1 || !eff.BlockHostedTools {
		t.Errorf("effective = %+v", eff)
	}
}
