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
	"github.com/bespinian/keera-gateway/internal/registry"
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
	"PUT /v1/guardrails/{scope}/{id}":           {"/v1/guardrails/project/project_a", `{"rate_limits":[]}`},

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
	"GET /v1/billing/charges":               {"/v1/billing/charges?org_id=org_a", ""},
	"POST /v1/billing/charges":              {"/v1/billing/charges", `{"org_id":"org_a","name":"x","amount_micros":10000000,"every":"month"}`},
	"DELETE /v1/billing/charges/{id}":       {"/v1/billing/charges/charge_a", ""},
	"POST /v1/billing/charges/{id}/pay":     {"/v1/billing/charges/charge_a/pay", ""},
	"GET /v1/audit":                         {"/v1/audit?org_id=org_a", ""},
	"GET /v1/requests":                      {"/v1/requests?org_id=org_a", ""},
	"GET /v1/requests/stream":               {"/v1/requests/stream?org_id=org_a", ""},
	"GET /v1/sessions":                      {"/v1/sessions?org_id=org_a", ""},
	"GET /v1/sessions/{id}":                 {"/v1/sessions/req_a?org_id=org_a", ""},
}

// serviceCases are Keera Agents' routes, called with its service key while
// naming org_b and org_a's rows. The service key reaches every organisation,
// so the boundary here is the one a request names: none may act on another
// organisation's person, project or key, and none may touch a key a person
// issued.
var serviceCases = map[string][]isolationCase{
	"POST /v1/service/keys": {
		{"/v1/service/keys", `{"org_id":"org_b","user_id":"user_a","name":"taken"}`},
		{"/v1/service/keys", `{"org_id":"org_b","user_id":"user_eve","project_id":"project_a","name":"taken"}`},
	},
	"DELETE /v1/service/keys/{id}": {
		{"/v1/service/keys/key_agents_a?org_id=org_b", ""},
		// Ada's own key, which a person issued.
		{"/v1/service/keys/key_a?org_id=org_a", ""},
	},
	"GET /v1/service/users": {
		{"/v1/service/users?org_id=org_b&email=ada@example.ch", ""},
		{"/v1/service/users?org_id=org_b&id=user_a", ""},
	},
	"GET /v1/service/catalog": {
		{"/v1/service/catalog?org_id=org_b&project_id=project_a", ""},
	},
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
	if _, err := st.CreateKey(ctx, store.KeyInfo{
		ID: "key_a", OrgID: "org_a", ProjectID: "project_a",
		UserID: "user_a", Name: "ada's laptop", Prefix: "keera_sk_a",
	},
		[]byte("hash-of-key_a-is-32-bytes-long!!")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateKey(ctx, store.KeyInfo{
		ID: "key_agents_a", OrgID: "org_a", ProjectID: "project_a",
		UserID: "user_a", Name: "ada's agent", Prefix: "keera_sk_g", IssuedBy: store.IssuedByAgents,
	},
		[]byte("hash-of-key_agents_a-is-32-bytes")); err != nil {
		t.Fatal(err)
	}
	if err := st.CreatePayment(ctx, store.Payment{
		ID: "pay_a", OrgID: "org_a", Kind: store.PaymentTopUp,
		AmountMicros: 10_000_000, Actor: "ada@example.ch",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateCharge(ctx, store.RecurringCharge{
		ID: "charge_a", OrgID: "org_a", Name: "Dedicated model", AmountMicros: 10_000_000,
		Every: "month", StartsOn: "2026-11-01", Actor: "operator",
	}); err != nil {
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
		OperatorKey:      testOperatorKey,
		AgentsServiceKey: testServiceKey,
		// Nothing listens there: a refused request never reaches it.
		Payments: &Payments{Client: payment.New(payment.Settings{
			URL:     "http://127.0.0.1:1",
			SpaceID: 1, UserID: "1", AuthKey: []byte("key"),
		})},
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
		if _, ok := notTenantData[pattern]; ok || isServiceRoute(pattern) {
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
	for _, pattern := range srv.routes {
		if !isServiceRoute(pattern) {
			continue
		}
		cases, ok := serviceCases[pattern]
		if !ok {
			t.Errorf("%s has no case in serviceCases; add a request about another "+
				"organisation's rows", pattern)
			continue
		}
		method, _, _ := strings.Cut(pattern, " ")
		for _, c := range cases {
			path := httpx.ControlPrefix + c.path
			// Eve's session and the operator key are not the service key.
			if code, body := send(method, path, c.body); code != http.StatusUnauthorized {
				t.Errorf("%s with a session = %d %s, want 401", pattern, code, body)
			}
			if code, body := send(method, path, c.body,
				"Authorization", "Bearer "+testOperatorKey); code != http.StatusUnauthorized {
				t.Errorf("%s with the operator key = %d %s, want 401", pattern, code, body)
			}
			code, body := send(method, path, c.body, "Authorization", "Bearer "+testServiceKey)
			if code != http.StatusNotFound {
				t.Errorf("%s %s about another organisation = %d %s, want 404", pattern, c.path, code, body)
			}
			for _, secret := range []string{"ada@example.ch", "ada's", "keera_sk_"} {
				if strings.Contains(body, secret) {
					t.Errorf("%s showed another organisation's %q: %s", pattern, secret, body)
				}
			}
		}
	}
	for pattern := range serviceCases {
		if !slices.Contains(srv.routes, pattern) {
			t.Errorf("%s is in serviceCases but no longer a route", pattern)
		}
	}
	// The keys named above are still live: nothing was revoked by mistake.
	for _, id := range []string{"key_a", "key_agents_a"} {
		if res, err := st.LookupKeyByID(ctx, id); err != nil || res.Key.ID != id {
			t.Errorf("%s after the service cases: %v", id, err)
		}
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

// isServiceRoute is a route of Keera Agents' server, which takes the service
// key instead of a sign-in.
func isServiceRoute(pattern string) bool {
	_, path, _ := strings.Cut(pattern, " ")
	return strings.HasPrefix(path, "/v1/service/")
}
