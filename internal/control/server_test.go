package control

import (
	"crypto/tls"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/connect"
	"github.com/bespinian/keera-gateway/internal/gateway"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/metrics"
)

const testOperatorKey = "an-operator-key-long-enough-to-be-real"

// newServer builds a control server with no store behind it. That is enough to
// exercise the credential and CSRF paths, which decide whether a request ever
// reaches a handler at all.
func newServer() *Server {
	return New(nil, nil, nil, nil, Options{OperatorKey: testOperatorKey}, slog.New(slog.DiscardHandler))
}

// echo is a handler that records the principal it was given.
func echo(seen **authn.Principal) handler {
	return func(w http.ResponseWriter, _ *http.Request, p *authn.Principal) {
		*seen = p
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"ok": "yes"})
	}
}

func TestOperatorKeyAuthenticatesAsAnOperator(t *testing.T) {
	var seen *authn.Principal
	h := newServer().authenticated(echo(&seen))

	r := httptest.NewRequest(http.MethodGet, httpx.ControlPrefix+"/v1/orgs", nil)
	r.Header.Set("Authorization", "Bearer "+testOperatorKey)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if seen == nil || seen.Via != authn.MethodOperatorKey {
		t.Fatalf("principal = %+v, want the operator key", seen)
	}
	if !seen.Unrestricted() {
		t.Error("the operator key must see every organisation")
	}
	if seen.Actor() != "operator key" {
		t.Errorf("Actor = %q; a shared credential must not claim to be a person", seen.Actor())
	}
}

func TestRejectsWrongOrMissingCredentials(t *testing.T) {
	var seen *authn.Principal
	h := newServer().authenticated(echo(&seen))

	tests := []struct{ name, header string }{
		{"nothing at all", ""},
		{"a wrong key", "Bearer not-the-operator-key"},
		{"an empty bearer", "Bearer "},
		{"a key that is a prefix of the real one", "Bearer " + testOperatorKey[:10]},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, httpx.ControlPrefix+"/v1/orgs", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", w.Code)
			}
			if seen != nil {
				t.Error("the handler ran for a request that was not authenticated")
			}
		})
	}
}

func TestOperatorKeyIsExemptFromCSRF(t *testing.T) {
	// The operator key is not carried by a browser, so nothing can be tricked into
	// submitting it. Requiring a token there would break every script.
	var seen *authn.Principal
	h := newServer().authenticated(echo(&seen))

	r := httptest.NewRequest(http.MethodPost, httpx.ControlPrefix+"/v1/teams", strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer "+testOperatorKey)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
}

func TestSessionsNeedTheirCSRFTokenOnAWrite(t *testing.T) {
	s := newServer()
	principal := &authn.Principal{
		Via: authn.MethodSession, UserID: "user_1", Email: "ada@example.ch",
		Role: authn.RoleAdmin, OrgID: "org_a", CSRF: "the-token",
	}

	var seen *authn.Principal
	// Stand in for the cookie lookup, which is the only part that needs a store.
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.withPrincipal(w, r, principal, echo(&seen))
	})

	tests := []struct {
		name       string
		method     string
		token      string
		wantStatus int
	}{
		{"a read needs no token", http.MethodGet, "", http.StatusOK},
		{"a write with the token", http.MethodPost, "the-token", http.StatusOK},
		{"a write without one", http.MethodPost, "", http.StatusForbidden},
		{"a write with the wrong one", http.MethodPost, "guessed", http.StatusForbidden},
		{"a delete without one", http.MethodDelete, "", http.StatusForbidden},
		{"a put without one", http.MethodPut, "", http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			seen = nil
			r := httptest.NewRequest(tc.method, httpx.ControlPrefix+"/v1/teams", strings.NewReader("{}"))
			if tc.token != "" {
				r.Header.Set("X-CSRF-Token", tc.token)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusForbidden && seen != nil {
				t.Error("the handler ran despite the missing CSRF token")
			}
		})
	}
}

func TestErrorsAreParseableEnvelopes(t *testing.T) {
	h := newServer().authenticated(func(http.ResponseWriter, *http.Request, *authn.Principal) {})
	r := httptest.NewRequest(http.MethodGet, httpx.ControlPrefix+"/v1/orgs", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q; the panel parses this", ct)
	}
	if !strings.Contains(w.Body.String(), `"error"`) {
		t.Errorf("body = %s, want an error envelope", w.Body)
	}
}

func TestThePlaygroundNeedsACredentialAndAnOrganisation(t *testing.T) {
	// A message typed into the panel spends money, so it is a write in every
	// sense that matters - and it has to say whose money.
	srv := New(nil, nil, nil, nil, Options{
		OperatorKey: testOperatorKey,
		// A gateway that is never reached: both refusals happen before it.
		Gateway: gateway.New(nil, nil, nil, nil, nil, gateway.Options{},
			slog.New(slog.DiscardHandler)),
	}, slog.New(slog.DiscardHandler))
	h := srv.Handler()

	post := func(path, auth string) int {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, path,
			strings.NewReader(`{"model":"keera-code","messages":[]}`))
		if auth != "" {
			r.Header.Set("Authorization", "Bearer "+auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	if got := post(httpx.ControlPrefix+"/v1/playground/chat", ""); got != http.StatusUnauthorized {
		t.Errorf("status = %d without a credential, want 401", got)
	}
	// An operator looking at every tenant at once has not said which budget
	// this comes out of, and there is no defensible default.
	if got := post(httpx.ControlPrefix+"/v1/playground/chat", testOperatorKey); got != http.StatusBadRequest {
		t.Errorf("status = %d with no organisation chosen, want 400", got)
	}
}

func TestThePlaygroundSaysSoWhenThereIsNoGateway(t *testing.T) {
	// A control plane running without an inference listener should say that
	// rather than fail somewhere further in.
	srv := New(nil, nil, nil, nil,
		Options{OperatorKey: testOperatorKey}, slog.New(slog.DiscardHandler))

	r := httptest.NewRequest(http.MethodPost, httpx.ControlPrefix+"/v1/playground/chat?org_id=org_1",
		strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer "+testOperatorKey)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", w.Code)
	}
}

// The panel's "add a hosted model" list comes from the same table the
// catalogue file is parsed against, and only an operator can add a model.
func TestProvidersAreServedToOperatorsWithoutSecrets(t *testing.T) {
	s := newServer()

	r := httptest.NewRequest(http.MethodGet, httpx.ControlPrefix+"/v1/providers", nil)
	r.Header.Set("Authorization", "Bearer "+testOperatorKey)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	body := w.Body.String()
	for _, want := range []string{"anthropic", "api.anthropic.com", "ANTHROPIC_API_KEY"} {
		if !strings.Contains(body, want) {
			t.Errorf("response does not mention %q: %s", want, body)
		}
	}

	// A member may read the catalogue, but not what could be added to it.
	member := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleMember, OrgID: "org_1"}
	w = httptest.NewRecorder()
	s.listProviders(w, r, member)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d for a member, want 403", w.Code)
	}
}

// The provider name a model carries is what the panel reads to tell the
// provider's answers from the operator's, so a name this build does not know is
// worth a refusal: it would otherwise be stored and silently match nothing.
func TestUnknownProviderIsRefused(t *testing.T) {
	s := newServer()

	body := `{"backend_model":"acme-1","backends":["https://api.acme.example/v1"],` +
		`"provider":"acme"}`
	r := httptest.NewRequest(http.MethodPut, httpx.ControlPrefix+"/v1/models/keera-acme", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testOperatorKey)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)

	// Before anything is written, as above: the store behind this server is nil.
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "anthropic") {
		t.Errorf("the refusal does not say which providers are known: %s", w.Body)
	}
}

// A deployment with no encryption key must say so rather than store a
// credential in a form a database dump would hand over.
func TestAPIKeyIsRefusedWhenItCouldNotBeEncrypted(t *testing.T) {
	s := newServer() // no Secrets configured

	body := `{"backend_model":"claude-opus-5","backends":["https://api.anthropic.com/v1"],` +
		`"api_key":"sk-ant-a-real-credential"}`
	r := httptest.NewRequest(http.MethodPut, httpx.ControlPrefix+"/v1/models/keera-frontier", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testOperatorKey)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)

	// 400 rather than a 500 from the nil store behind this server: the refusal
	// has to happen before anything is written.
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "KEERA_SECRET_KEY") {
		t.Errorf("the refusal does not say what to set: %s", w.Body)
	}
	if strings.Contains(w.Body.String(), "sk-ant-a-real-credential") {
		t.Error("the credential was echoed back in the error")
	}
}

// The panel puts this URL into the configuration a developer copies into their
// editor, so a wrong one is a client that cannot connect at all. One listener
// serves both planes, so it is always the panel's own origin plus /api - the
// only question is which origin.
func TestGatewayURL(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		host string
		tls  bool
		want string
	}{
		{
			name: "the declared public URL wins over the Host header",
			opts: Options{PublicURL: "https://keera.example.ch"},
			host: "10.0.0.7:8080",
			want: "https://keera.example.ch/api",
		},
		{
			name: "a declared public URL keeps its port",
			opts: Options{PublicURL: "https://keera.example.ch:8443"},
			host: "10.0.0.7:8080",
			want: "https://keera.example.ch:8443/api",
		},
		{
			name: "otherwise the host the panel is being read on",
			host: "keera.example.ch:8080",
			want: "http://keera.example.ch:8080/api",
		},
		{
			name: "over TLS without a declared URL",
			host: "keera.example.ch",
			tls:  true,
			want: "https://keera.example.ch/api",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.opts.OperatorKey = testOperatorKey
			s := New(nil, nil, nil, nil, tt.opts, slog.New(slog.DiscardHandler))

			r := httptest.NewRequest(http.MethodGet, httpx.ControlPrefix+"/v1/me", nil)
			r.Host = tt.host
			if tt.tls {
				r.TLS = &tls.ConnectionState{}
			}
			if got := s.gatewayURL(r); got != tt.want {
				t.Errorf("gatewayURL = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAnEmptyOperatorKeyAuthenticatesNobody(t *testing.T) {
	// A deployment that configured no operator key must not have one. Comparing a
	// presented credential against sha256("") would hand an operator session to
	// whoever sent an empty key first.
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))

	t.Run("the sign-in route says so instead of comparing", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, httpx.ControlPrefix+"/auth/local", strings.NewReader(`{"key":""}`))
		w := httptest.NewRecorder()
		s.localLogin(w, r)
		if w.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want 501; an empty key must not sign anybody in", w.Code)
		}
	})

	t.Run("and neither does a whitespace key", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, httpx.ControlPrefix+"/auth/local", strings.NewReader(`{"key":"  "}`))
		w := httptest.NewRecorder()
		s.localLogin(w, r)
		if w.Code == http.StatusOK {
			t.Fatal("a whitespace key signed in")
		}
	})

	t.Run("no bearer token is the operator key", func(t *testing.T) {
		for _, presented := range []string{"x", " ", "anything"} {
			r := httptest.NewRequest(http.MethodGet, httpx.ControlPrefix+"/v1/orgs", nil)
			r.Header.Set("Authorization", "Bearer "+presented)
			if p, err := s.principal(r); err == nil {
				t.Errorf("%q authenticated as %+v", presented, p)
			}
		}
	})

	t.Run("the panel is told not to offer the field", func(t *testing.T) {
		w := httptest.NewRecorder()
		s.authConfig(w, httptest.NewRequest(http.MethodGet, httpx.ControlPrefix+"/auth/config", nil))
		if strings.Contains(w.Body.String(), `"operator_key":true`) {
			t.Errorf("auth config offers the operator key: %s", w.Body.String())
		}
	})
}

func TestSignInIsThrottledPerAddress(t *testing.T) {
	// Unthrottled, this route answers "is this the operator key" as fast as the
	// network allows.
	h := newServer().Handler()

	attempt := func(addr string) int {
		r := httptest.NewRequest(http.MethodPost, httpx.ControlPrefix+"/auth/local",
			strings.NewReader(`{"key":"not-the-operator-key"}`))
		r.RemoteAddr = addr
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	for i := range signInRPM {
		if got := attempt("198.51.100.7:5000"); got != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i, got)
		}
	}
	if got := attempt("198.51.100.7:5000"); got != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 after %d attempts", got, signInRPM)
	}
	// A throttle keyed by address must not lock out everybody else.
	if got := attempt("198.51.100.8:5000"); got != http.StatusUnauthorized {
		t.Errorf("another address got %d, want 401", got)
	}
}

func TestMetricsTokenReadsMetricsAndNothingElse(t *testing.T) {
	const scrape = "a-scrape-token-long-enough-to-be-real"
	s := New(nil, nil, metrics.New(), nil,
		Options{OperatorKey: testOperatorKey, MetricsToken: scrape}, slog.New(slog.DiscardHandler))
	h := s.Handler()

	get := func(path, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	if w := get("/metrics", scrape); w.Code != http.StatusOK {
		t.Fatalf("the scrape token got %d on /metrics, want 200", w.Code)
	}
	// The whole point is that a scrape configuration holds something that
	// cannot change a guardrail.
	if w := get(httpx.ControlPrefix+"/v1/orgs", scrape); w.Code != http.StatusUnauthorized {
		t.Errorf("the scrape token got %d on /v1/orgs, want 401", w.Code)
	}
	if w := get("/metrics", testOperatorKey); w.Code != http.StatusOK {
		t.Errorf("the operator key got %d on /metrics, want 200", w.Code)
	}
	if w := get("/metrics", "neither-of-the-two-credentials"); w.Code != http.StatusUnauthorized {
		t.Errorf("an unknown credential got %d on /metrics, want 401", w.Code)
	}
	if w := get("/metrics", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("an anonymous request got %d on /metrics, want 401", w.Code)
	}
}

func TestMetricsWithNoScrapeTokenStaysOperatorOnly(t *testing.T) {
	s := New(nil, nil, metrics.New(), nil,
		Options{OperatorKey: testOperatorKey}, slog.New(slog.DiscardHandler))
	h := s.Handler()

	// Without a configured token, the empty string must not become one.
	for _, token := range []string{"", " ", "x"} {
		r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("token %q got %d, want 401", token, w.Code)
		}
	}
}

// The client catalogue is what the panel's Connect a client screen and
// `keera connect` both render, so the endpoint has to carry the templates and
// the gateway address a developer's editor is to use - and no credential.
func TestConnectServesTheClientCatalogueAndTheGatewayAddress(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{
		OperatorKey: testOperatorKey,
		PublicURL:   "https://keera.example.ch",
	}, slog.New(slog.DiscardHandler))

	r := httptest.NewRequest(http.MethodGet, httpx.ControlPrefix+"/v1/connect", nil)
	r.Header.Set("Authorization", "Bearer "+testOperatorKey)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	var res struct {
		Data       []connect.Client `json:"data"`
		GatewayURL string           `json:"gateway_url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.GatewayURL != "https://keera.example.ch/api" {
		t.Errorf("gateway_url = %q, want the declared origin plus the inference prefix",
			res.GatewayURL)
	}
	if len(res.Data) != len(connect.Clients()) {
		t.Errorf("served %d clients, want %d", len(res.Data), len(connect.Clients()))
	}
	// The templates have to survive the round trip unsubstituted: the panel
	// fills them in per keystroke in its URL field, which is why they are
	// templates rather than rendered here.
	for _, c := range res.Data {
		if !strings.Contains(c.Template, "{{base}}") {
			t.Errorf("%s came back without its {{base}} placeholder", c.Key)
		}
	}
	// A member configures their own editor, so this is not an admin read. What
	// it must never carry is a credential.
	member := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleMember, OrgID: "org_1"}
	w = httptest.NewRecorder()
	s.listConnect(w, r, member)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d for a member, want 200", w.Code)
	}
	if strings.Contains(w.Body.String(), testOperatorKey) {
		t.Error("the catalogue carries the operator key")
	}
}
