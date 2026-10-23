package control

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/payment"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/registry"
	"github.com/bespinian/keera-gateway/internal/sandbox"
	"github.com/bespinian/keera-gateway/internal/store"
)

// isolationCase is one request an administrator of org_b sends about org_a.
// Its method is the route's.
type isolationCase struct {
	path, body string
}

// isolationCases has a request for every authenticated route, naming
// org_a's rows or org_a itself. Each must be refused.
var isolationCases = map[string]isolationCase{
	"POST /v1/me/org": {"/v1/me/org", `{"org_id":"org_a"}`},

	"PATCH /v1/orgs/{id}":  {"/v1/orgs/org_a", `{"name":"taken"}`},
	"DELETE /v1/orgs/{id}": {"/v1/orgs/org_a", ""},

	"POST /v1/projects":           {"/v1/projects", `{"org_id":"org_a","name":"taken"}`},
	"GET /v1/projects":            {"/v1/projects?org_id=org_a", ""},
	"PATCH /v1/projects/{id}":     {"/v1/projects/project_a", `{"name":"taken"}`},
	"DELETE /v1/projects/{id}":    {"/v1/projects/project_a", ""},
	"POST /v1/users":              {"/v1/users", `{"org_id":"org_a","email":"eve@example.ch","role":"admin"}`},
	"GET /v1/users":               {"/v1/users?org_id=org_a", ""},
	"PATCH /v1/users/{id}":        {"/v1/users/user_a", `{"role":"admin"}`},
	"POST /v1/users/{id}/disable": {"/v1/users/user_a/disable", ""},
	"POST /v1/users/{id}/enable":  {"/v1/users/user_a/enable", ""},

	"POST /v1/keys":               {"/v1/keys", `{"org_id":"org_a","name":"taken"}`},
	"GET /v1/keys":                {"/v1/keys?org_id=org_a", ""},
	"PATCH /v1/keys/{id}":         {"/v1/keys/key_a", `{"name":"taken"}`},
	"DELETE /v1/keys/{id}":        {"/v1/keys/key_a", ""},
	"POST /v1/keys/{id}/rotate":   {"/v1/keys/key_a/rotate", "{}"},
	"POST /v1/keys/{id}/activate": {"/v1/keys/key_a/activate", "{}"},

	"GET /v1/guardrails/{scope}/{id}":           {"/v1/guardrails/org/org_a", ""},
	"GET /v1/guardrails/{scope}/{id}/effective": {"/v1/guardrails/key/key_a/effective", ""},
	"PUT /v1/guardrails/{scope}/{id}":           {"/v1/guardrails/project/project_a", `{"rpm":1}`},

	"GET /v1/filters":                {"/v1/filters?org_id=org_a", ""},
	"PUT /v1/filters/{alias}":        {"/v1/filters/f?org_id=org_a", `{"kind":"regex","pattern":"x"}`},
	"DELETE /v1/filters/{alias}":     {"/v1/filters/f?org_id=org_a", ""},
	"POST /v1/filters/{alias}/check": {"/v1/filters/f/check?org_id=org_a", ""},
	"GET /v1/filters/{alias}/report": {"/v1/filters/f/report?org_id=org_a", ""},
	"GET /v1/routers":                {"/v1/routers?org_id=org_a", ""},
	"PUT /v1/routers/{alias}":        {"/v1/routers/r?org_id=org_a", `{"routes":[]}`},
	"DELETE /v1/routers/{alias}":     {"/v1/routers/r?org_id=org_a", ""},
	"POST /v1/routers/{alias}/check": {"/v1/routers/r/check?org_id=org_a", ""},
	"GET /v1/routers/{alias}/report": {"/v1/routers/r/report?org_id=org_a", ""},
	"GET /v1/models":                 {"/v1/models?org_id=org_a", ""},
	"PUT /v1/models/{alias}":         {"/v1/models/m?org_id=org_a", `{"backends":["http://x"],"enabled":true}`},
	"DELETE /v1/models/{alias}":      {"/v1/models/m?org_id=org_a", ""},
	"POST /v1/models/{alias}/check":  {"/v1/models/m/check?org_id=org_a", ""},
	"GET /v1/mcp-servers":            {"/v1/mcp-servers?org_id=org_a", ""},
	"PUT /v1/mcp-servers/{alias}":    {"/v1/mcp-servers/s?org_id=org_a", `{"url":"https://mcp.example"}`},
	"DELETE /v1/mcp-servers/{alias}": {"/v1/mcp-servers/s?org_id=org_a", ""},
	"GET /v1/tool-calls":             {"/v1/tool-calls?org_id=org_a", ""},
	// Ada's key: the playground sends as the key it is given.
	"POST /v1/playground/chat":              {"/v1/playground/chat?key_id=key_a", `{"model":"m","messages":[]}`},
	"GET /v1/diagnostics":                   {"/v1/diagnostics?org_id=org_a", ""},
	"GET /v1/setup":                         {"/v1/setup?org_id=org_a", ""},
	"GET /v1/overview":                      {"/v1/overview?org_id=org_a", ""},
	"GET /v1/map":                           {"/v1/map?org_id=org_a", ""},
	"GET /v1/usage":                         {"/v1/usage?org_id=org_a", ""},
	"GET /v1/billing":                       {"/v1/billing?org_id=org_a", ""},
	"GET /v1/billing/account":               {"/v1/billing/account?org_id=org_a", ""},
	"PATCH /v1/billing/account":             {"/v1/billing/account?org_id=org_a", `{"org_id":"org_a"}`},
	"DELETE /v1/billing/account/card":       {"/v1/billing/account/card?org_id=org_a", ""},
	"POST /v1/billing/topups":               {"/v1/billing/topups?org_id=org_a", `{"org_id":"org_a","amount_micros":10000000}`},
	"GET /v1/billing/payments":              {"/v1/billing/payments?org_id=org_a", ""},
	"GET /v1/billing/payments/{id}/receipt": {"/v1/billing/payments/pay_a/receipt", ""},
	"POST /v1/billing/grants":               {"/v1/billing/grants", `{"org_id":"org_a","amount_micros":10000000}`},
	"GET /v1/audit":                         {"/v1/audit?org_id=org_a", ""},
	"GET /v1/requests":                      {"/v1/requests?org_id=org_a", ""},
	"GET /v1/requests/stream":               {"/v1/requests/stream?org_id=org_a", ""},
	"GET /v1/sessions":                      {"/v1/sessions?org_id=org_a", ""},
	"GET /v1/sessions/{id}":                 {"/v1/sessions/req_a?org_id=org_a", ""},

	"GET /v1/sandbox-classes":           {"/v1/sandbox-classes?org_id=org_a", ""},
	"PUT /v1/sandbox-classes/{name}":    {"/v1/sandbox-classes/standard?org_id=org_a", `{"image":"example/sandbox:1","cpu_millis":1000,"memory_mib":1024,"disk_mib":1024}`},
	"DELETE /v1/sandbox-classes/{name}": {"/v1/sandbox-classes/standard?org_id=org_a", ""},
	"GET /v1/sandboxes":                 {"/v1/sandboxes?org_id=org_a", ""},
	"POST /v1/sandboxes":                {"/v1/sandboxes", `{"org_id":"org_a","name":"taken","class":"standard","authorized_keys":["ssh-ed25519 AAAA eve"]}`},
	"GET /v1/sandboxes/{ref}":           {"/v1/sandboxes/sbx_a", ""},
	"DELETE /v1/sandboxes/{ref}":        {"/v1/sandboxes/sbx_a", ""},
	"POST /v1/sandboxes/{ref}/extend":   {"/v1/sandboxes/sbx_a/extend", "{}"},
	"POST /v1/sandboxes/{ref}/suspend":  {"/v1/sandboxes/sbx_a/suspend", ""},
	"POST /v1/sandboxes/{ref}/resume":   {"/v1/sandboxes/sbx_a/resume", ""},
	"GET /v1/sandbox-usage":             {"/v1/sandbox-usage?org_id=org_a", ""},
}

// notTenantData are the routes that only ever act on the caller, or read
// what is the same for every tenant, so there is no other tenant's request
// to make. Each says why.
var notTenantData = map[string]string{
	"POST /auth/logout":                "ends the caller's own sign-in",
	"GET /v1/me":                       "the caller",
	"GET /v1/access":                   "the caller's own keys and limits",
	"POST /v1/orgs":                    "creates a new organisation; only an operator may",
	"GET /v1/orgs":                     "lists the organisations the caller is in",
	"GET /v1/providers":                "the hosted-provider catalogue, the same for everyone",
	"GET /v1/connect":                  "the client catalogue, the same for everyone",
	"POST /v1/connect/render":          "renders the models in the request, and reads nothing",
	"GET /v1/clients":                  "the clients that called with the caller's own keys",
	"GET /v1/passkeys":                 "the caller's own passkeys",
	"POST /v1/passkeys/options":        "the caller's own passkeys",
	"POST /v1/passkeys":                "the caller's own passkeys",
	"DELETE /v1/passkeys/{id}":         "the caller's own passkeys",
	"POST /v1/users/{id}/passkey-link": "passkeys are off on this server, so it is refused for everyone",
}

// Every route a signed-in tenant can call is called here by an administrator
// of another organisation, naming the first one's rows. None may succeed.
// Each handler checks permissions itself, so this is what notices one that
// forgot. A route without a case fails the test, so a new route gets one.
func TestNoRouteLetsOneTenantReachAnother(t *testing.T) {
	st, ctx := testStore(t)
	for _, id := range []string{"org_a", "org_b"} {
		if _, err := st.CreateOrg(ctx, store.Org{ID: id, Name: id}, store.OrgTemplate{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.CreateProject(ctx, store.Project{ID: "project_a", OrgID: "org_a", Name: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddUser(ctx, "user_a", "org_a", "ada@example.ch", "", "member"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateKey(ctx, store.KeyInfo{ID: "key_a", OrgID: "org_a", ProjectID: "project_a",
		UserID: "user_a", Name: "ada's laptop", Prefix: "keera_sk_a"},
		[]byte("hash-of-key_a-is-32-bytes-long!!")); err != nil {
		t.Fatal(err)
	}
	class := policy.SandboxClass{OrgID: "org_a", Name: "standard", Image: "example/sandbox:1",
		Isolation: policy.IsolationIsolated, CPU: 1000, Memory: 1024, Disk: 1024,
		DefaultTTLSeconds: 3600, MaxTTLSeconds: 3600}
	if err := st.CreatePayment(ctx, store.Payment{ID: "pay_a", OrgID: "org_a", Kind: store.PaymentTopUp,
		AmountMicros: 10_000_000, Actor: "ada@example.ch"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSandboxClass(ctx, &class); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	if _, err := st.CreateSandbox(ctx, store.Sandbox{ID: "sbx_a", OrgID: "org_a", ProjectID: "project_a",
		UserID: "user_a", Owner: "ada@example.ch", Name: "desk", Class: "standard",
		Purpose: policy.PurposeEngineer, State: policy.SandboxReady, Image: "example/sandbox:1",
		Isolation: policy.IsolationIsolated, CPU: 1000, Memory: 1024, Disk: 1024,
		KeyID: "key_a", ExpiresAt: &expires}); err != nil {
		t.Fatal(err)
	}

	// The attacker: an administrator of org_b, signed in to the panel.
	eve, err := st.AddUser(ctx, "user_eve", "org_b", "eve@example.ch", "", "admin")
	if err != nil {
		t.Fatal(err)
	}
	token, hash, csrf := authn.NewSession()
	if err := st.CreateSession(ctx, hash, eve.ID, csrf, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	reg, err := registry.New(ctx, st, registry.Options{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(st, reg, nil, nil, Options{
		OperatorKey: testOperatorKey,
		Sandboxes:   sandbox.NewManager(st, stubDriver{}, sandbox.ManagerOptions{}),
		// Nothing listens there: a refused request never reaches it.
		Payments: &Payments{Client: payment.New(payment.Settings{URL: "http://127.0.0.1:1",
			SpaceID: 1, UserID: "1", AuthKey: []byte("key")})},
	}, slog.New(slog.DiscardHandler))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	send := func(method, path, body string, header ...string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, ts.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		req.Header.Set("X-CSRF-Token", csrf)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return resp.StatusCode, string(raw)
	}
	if code, body := send("GET", httpx.ControlPrefix+"/v1/me", ""); code != http.StatusOK {
		t.Fatalf("the attacker is not signed in: %d %s", code, body)
	}

	for _, pattern := range srv.routes {
		if _, ok := notTenantData[pattern]; ok {
			continue
		}
		c, ok := isolationCases[pattern]
		if !ok {
			t.Errorf("%s has no case here; add a request about another tenant's data, "+
				"or say in notTenantData why there is none", pattern)
			continue
		}
		method, _, _ := strings.Cut(pattern, " ")
		code, body := send(method, httpx.ControlPrefix+c.path, c.body)
		if code != http.StatusForbidden && code != http.StatusNotFound {
			t.Errorf("%s from another tenant = %d %s, want 403 or 404", pattern, code, body)
		}
		for _, secret := range []string{"ada@example.ch", "ada's laptop", "keera_sk_a"} {
			if strings.Contains(body, secret) {
				t.Errorf("%s showed another tenant's %q: %s", pattern, secret, body)
			}
		}
	}
	// Attaching to a sandbox is a byte stream, registered on its own.
	if code, body := send("GET", httpx.SandboxPrefix+"/v1/sbx_a/tcp/2222", "",
		"Connection", "Upgrade", "Upgrade", upgradeProtocol); code != http.StatusForbidden &&
		code != http.StatusNotFound {
		t.Errorf("attaching to another tenant's sandbox = %d %s, want 403 or 404", code, body)
	}
	for pattern := range isolationCases {
		if !slices.Contains(srv.routes, pattern) {
			t.Errorf("%s is a case here but no longer a route", pattern)
		}
	}
	for pattern := range notTenantData {
		if !slices.Contains(srv.routes, pattern) {
			t.Errorf("%s is in notTenantData but no longer a route", pattern)
		}
	}
}
