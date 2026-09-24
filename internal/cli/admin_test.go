package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/catalog"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/id"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// recorded is one request the fake control plane saw.
type recorded struct {
	method string
	path   string
	// query is what the command narrowed the call to, for the reports whose
	// whole behaviour is in it: a window the CLI did not pass on is a report
	// answering a different question from the one that was asked for.
	query string
	body  map[string]any
}

// fakeControl stands in for the control API. Handlers are keyed by
// "METHOD /path", and everything they were sent is kept for the assertions.
type fakeControl struct {
	t        *testing.T
	seen     []recorded
	handlers map[string]any
}

func newFakeControl(t *testing.T, handlers map[string]any) *fakeControl {
	t.Helper()
	f := &fakeControl{t: t, handlers: handlers}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("KEERA_CONTROL_URL", srv.URL)
	t.Setenv("KEERA_OPERATOR_KEY", "test-operator-key")
	return f
}

// failure is a handler that answers with an error envelope, for the paths whose
// behaviour is what the CLI does when the control plane refuses.
type failure struct {
	status  int
	message string
}

func (f *fakeControl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	// Handlers are keyed without httpx.ControlPrefix. A call that arrives
	// without the prefix would 404 in production, so it is kept as it came
	// and fails to match.
	path := r.URL.Path
	if after, ok := strings.CutPrefix(path, httpx.ControlPrefix); ok {
		path = after
	}
	f.seen = append(f.seen, recorded{
		method: r.Method, path: path, query: r.URL.RawQuery, body: body,
	})

	res, found := f.handlers[r.Method+" "+path]
	if !found {
		f.t.Errorf("the CLI called %s %s, which the test did not expect", r.Method, r.URL.Path)
		http.Error(w, `{"error":{"message":"unexpected call"}}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if bad, ok := res.(failure); ok {
		w.WriteHeader(bad.status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"message": bad.message},
		})
		return
	}
	_ = json.NewEncoder(w).Encode(res)
}

// called reports whether METHOD path was requested at all.
func (f *fakeControl) called(method, path string) bool {
	for _, r := range f.seen {
		if r.method == method && r.path == path {
			return true
		}
	}
	return false
}

// request returns the one call to METHOD path, failing if it was not made.
func (f *fakeControl) request(method, path string) recorded {
	f.t.Helper()
	for _, r := range f.seen {
		if r.method == method && r.path == path {
			return r
		}
	}
	f.t.Fatalf("the CLI never called %s %s; it called %v", method, path, f.seen)
	return recorded{}
}

// quiet swallows what a command prints, so a passing test says nothing. Both
// streams: a command that hands over a secret puts it on stdout and says what
// it did on stderr.
func quiet(t *testing.T) {
	t.Helper()
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = devNull, devNull
	t.Cleanup(func() {
		os.Stdout, os.Stderr = stdout, stderr
		_ = devNull.Close()
	})
}

// captureStderr swallows stdout and hands back what was written to stderr, for
// the commands whose behaviour is the warning rather than the table.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = devNull, w
	var said string
	read := func() string {
		if w != nil {
			os.Stdout, os.Stderr = stdout, stderr
			_ = w.Close()
			raw, _ := io.ReadAll(r)
			said = string(raw)
			w = nil
		}
		return said
	}
	t.Cleanup(func() {
		read()
		_ = r.Close()
		_ = devNull.Close()
	})
	return read
}

// oneOrg is what resolveOrg needs to fill in --org by itself, which is the
// case in every dedicated deployment.
var oneOrg = map[string]any{"data": []map[string]any{{"id": "org_1", "name": "Example Bank"}}}

// -------------------------------------------------------------------- orgs

func TestOrgCreateSendsTheDomain(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"POST /v1/orgs": map[string]any{
			"id": "org_2", "name": "Another Bank", "email_domain": "anotherbank.ch",
		},
		"GET /v1/orgs": oneOrg,
	})

	if err := Run(context.Background(), []string{"org", "create", "Another Bank",
		"--domain", "AnotherBank.CH"}); err != nil {
		t.Fatal(err)
	}
	// Lowercased, because that is what a sign-in is compared against.
	if got := f.request("POST", "/v1/orgs").body["email_domain"]; got != "anotherbank.ch" {
		t.Errorf("email_domain sent = %v, want anotherbank.ch", got)
	}
}

func TestOrgSetPatchesTheDomain(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"PATCH /v1/orgs/org_2": map[string]any{"id": "org_2", "email_domain": "anotherbank.ch"},
	})

	if err := Run(context.Background(), []string{"org", "set", "org_2",
		"--domain", "anotherbank.ch"}); err != nil {
		t.Fatal(err)
	}
	if got := f.request("PATCH", "/v1/orgs/org_2").body["email_domain"]; got != "anotherbank.ch" {
		t.Errorf("email_domain sent = %v, want anotherbank.ch", got)
	}
}

// The id can be left off where there is only one organisation, which is the
// deployment that has to set a domain before it gets its second.
func TestOrgSetFindsTheOnlyOrganisation(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/orgs":         oneOrg,
		"PATCH /v1/orgs/org_1": map[string]any{"id": "org_1", "email_domain": "example.ch"},
	})

	if err := Run(context.Background(), []string{"org", "set", "--domain", "example.ch"}); err != nil {
		t.Fatal(err)
	}
	if !f.called("PATCH", "/v1/orgs/org_1") {
		t.Errorf("the CLI patched %v, want org_1", f.seen)
	}
}

func TestOrgSetNamesTheArgumentWhenThereAreSeveral(t *testing.T) {
	quiet(t)
	newFakeControl(t, map[string]any{
		"GET /v1/orgs": map[string]any{"data": []map[string]any{
			{"id": "org_1", "name": "Example Bank"}, {"id": "org_2", "name": "Another Bank"},
		}},
	})

	// Not "pass --org <id>": this command has no such flag.
	err := Run(context.Background(), []string{"org", "set", "--domain", "example.ch"})
	if err == nil || !strings.Contains(err.Error(), "keera org set <org-id>") {
		t.Fatalf("error = %v, want it to name the argument", err)
	}
}

func TestOrgSetClearsTheDomainOnlyWithItsOwnFlag(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"PATCH /v1/orgs/org_2": map[string]any{"id": "org_2", "email_domain": ""},
	})

	if err := Run(context.Background(), []string{"org", "set", "org_2", "--no-domain"}); err != nil {
		t.Fatal(err)
	}
	// Sent as an empty string rather than left out: the endpoint takes an
	// absent field as "say what to set" and refuses it.
	if got, present := f.request("PATCH", "/v1/orgs/org_2").body["email_domain"]; !present || got != "" {
		t.Errorf("email_domain = %v (present %v), want an empty string", got, present)
	}
}

func TestOrgSetRefusesToDoNothing(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{})

	err := Run(context.Background(), []string{"org", "set", "org_2"})
	if err == nil || !strings.Contains(err.Error(), "--domain") {
		t.Fatalf("error = %v, want it to name the flag", err)
	}
	if len(f.seen) != 0 {
		t.Errorf("the CLI called %v; a command that changes nothing changes nothing", f.seen)
	}
}

// A domain that is not a domain matches no sign-in, and would be found out
// months later by a customer who cannot get in.
func TestOrgSetRefusesSomethingThatIsNotADomain(t *testing.T) {
	quiet(t)
	newFakeControl(t, map[string]any{})

	// An address and a URL both carry the domain that was meant, so the refusal
	// says what to type instead of only that this was wrong.
	for _, given := range []string{"alice@example.ch", "https://example.ch/", "example.ch:443"} {
		err := Run(context.Background(), []string{"org", "set", "org_2", "--domain", given})
		if err == nil {
			t.Errorf("--domain %q was accepted", given)
			continue
		}
		if !strings.HasSuffix(err.Error(), "that is example.ch") {
			t.Errorf("--domain %q said %q, want it to name the domain meant", given, err)
		}
	}
	// A typo carries nothing to suggest, so it names what is wrong with it.
	err := Run(context.Background(), []string{"org", "set", "org_2", "--domain", "exam ple.ch"})
	if err == nil || !strings.Contains(err.Error(), "cannot appear") {
		t.Errorf("error = %v, want it to name the character", err)
	}
}

func TestCleanDomainTakesWhatPeopleType(t *testing.T) {
	for given, want := range map[string]string{
		"example.ch":     "example.ch",
		"@example.ch":    "example.ch",
		"Example.CH":     "example.ch",
		" example.ch. ":  "example.ch",
		"sub.example.ch": "sub.example.ch",
		"":               "",
	} {
		got, err := cleanDomain(given)
		if err != nil || got != want {
			t.Errorf("cleanDomain(%q) = %q, %v; want %q", given, got, err, want)
		}
	}
}

// Creating the second organisation is what turns "everyone lands here" into
// "placed by domain or refused", and it does that to the organisation that was
// already there rather than to the one being created.
func TestOrgCreateWarnsAboutAnOrganisationWithNoDomain(t *testing.T) {
	stderr := captureStderr(t)
	f := newFakeControl(t, map[string]any{
		"POST /v1/orgs": map[string]any{"id": "org_2", "name": "Another Bank"},
		"GET /v1/orgs": map[string]any{"data": []map[string]any{
			{"id": "org_1", "name": "Example Bank"},
			{"id": "org_2", "name": "Another Bank", "email_domain": "anotherbank.ch"},
		}},
	})

	if err := Run(context.Background(), []string{"org", "create", "Another Bank"}); err != nil {
		t.Fatal(err)
	}
	if !f.called("GET", "/v1/orgs") {
		t.Fatal("the CLI never looked at what else is there")
	}
	said := stderr()
	if !strings.Contains(said, "org_1") || strings.Contains(said, "org_2") {
		t.Errorf("warning was:\n%s\nwant it to name org_1, which has no domain, and not org_2", said)
	}
}

func TestOrgCreateSaysNothingOnTheFirstOrganisation(t *testing.T) {
	stderr := captureStderr(t)
	newFakeControl(t, map[string]any{
		"POST /v1/orgs": map[string]any{"id": "org_1", "name": "Example Bank"},
		"GET /v1/orgs":  oneOrg,
	})

	// One organisation needs no domain at all: everyone who signs in lands in
	// it. Warning here would be noise on every dedicated deployment there is.
	if err := Run(context.Background(), []string{"org", "create", "Example Bank"}); err != nil {
		t.Fatal(err)
	}
	if said := stderr(); said != "" {
		t.Errorf("said %q, want nothing", said)
	}
}

// ------------------------------------------------------------------- users

func TestUserRoleResolvesAnEmailToAnID(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/orgs": oneOrg,
		"GET /v1/users": map[string]any{"data": []map[string]any{
			{"id": "user_1", "org_id": "org_1", "email": "Alice@Example.com", "role": "member"},
		}},
		"PATCH /v1/users/user_1": map[string]any{"id": "user_1", "role": "admin"},
	})

	// An operator types the address they know, in whatever case they know it.
	if err := userCmd(context.Background(), []string{"role", "alice@example.com", "admin"}); err != nil {
		t.Fatal(err)
	}
	if got := f.request("PATCH", "/v1/users/user_1").body["role"]; got != "admin" {
		t.Errorf("role sent = %v, want admin", got)
	}
}

func TestUserRoleRefusesARoleTheControlPlaneWouldReject(t *testing.T) {
	quiet(t)
	newFakeControl(t, map[string]any{})

	err := userCmd(context.Background(), []string{"role", "alice@example.com", "owner"})
	if err == nil || !strings.Contains(err.Error(), "member, admin") {
		t.Fatalf("error = %v, want it to name the roles", err)
	}
}

// The operator role crosses organisations, so it comes from the gateway's own
// configuration. Refusing it here rather than at the control plane is the same
// answer one round trip earlier, and names the variable that does grant it.
func TestUserRoleRefusesTheOperatorRole(t *testing.T) {
	quiet(t)
	newFakeControl(t, map[string]any{})

	err := userCmd(context.Background(), []string{"role", "alice@example.com", "operator"})
	if err == nil || !strings.Contains(err.Error(), "KEERA_OPERATORS") {
		t.Fatalf("error = %v, want it to name KEERA_OPERATORS", err)
	}
}

func TestUserAddRefusesSomebodyWhoIsAlreadyThere(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/orgs": oneOrg,
		"GET /v1/users": map[string]any{"data": []map[string]any{
			{"id": "user_1", "org_id": "org_1", "email": "alice@example.com", "role": "member"},
		}},
	})

	// Upserting would silently re-role them and leave their sessions alone.
	err := userCmd(context.Background(), []string{"add", "alice@example.com", "--role", "admin"})
	if err == nil || !strings.Contains(err.Error(), "keera user role") {
		t.Fatalf("error = %v, want it to point at keera user role", err)
	}
	for _, r := range f.seen {
		if r.method == "POST" {
			t.Fatal("the CLI posted the person anyway")
		}
	}
}

func TestUserAddSendsTheRole(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/orgs":   oneOrg,
		"GET /v1/users":  map[string]any{"data": []map[string]any{}},
		"POST /v1/users": map[string]any{"id": "user_2", "email": "bob@example.com", "role": "admin"},
	})

	if err := userCmd(context.Background(), []string{"add", "bob@example.com", "--role", "admin"}); err != nil {
		t.Fatal(err)
	}
	body := f.request("POST", "/v1/users").body
	if body["email"] != "bob@example.com" || body["role"] != "admin" || body["org_id"] != "org_1" {
		t.Errorf("posted %v", body)
	}
}

// A key that names nobody is a key whose spend no report can attribute.
func TestKeyCreateAttributesAPersonByEmail(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/orgs": oneOrg,
		"GET /v1/users": map[string]any{"data": []map[string]any{
			{"id": "user_1", "org_id": "org_1", "email": "alice@example.com", "role": "member"},
		}},
		"POST /v1/keys": map[string]any{"id": "key_1", "key": "sk-keera-secret"},
	})

	if err := keyCmd(context.Background(), []string{"create", "--user", "alice@example.com", "--alias", "laptop"}); err != nil {
		t.Fatal(err)
	}
	if got := f.request("POST", "/v1/keys").body["user_id"]; got != "user_1" {
		t.Errorf("user_id sent = %v, want user_1", got)
	}
}

// liveKey is one key as GET /v1/keys reports it: on a team, attributed to a
// person, issued for 90 days, and carrying guardrails of its own.
var liveKey = map[string]any{
	"data": []map[string]any{{
		"id": "key_old", "org_id": "org_1", "team_id": "team_1", "user_id": "user_1",
		"alias": "a developer's laptop", "prefix": "keera_sk_ab",
		"created_at": "2026-01-01T00:00:00Z", "expires_at": "2026-04-01T00:00:00Z",
		"limits": map[string]any{"rpm": 120, "allowed_models": []string{"keera-speed"}},
	}},
}

func TestKeyRotateCarriesTheAttributionAndTheLifetime(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/orgs":                   oneOrg,
		"GET /v1/keys":                   liveKey,
		"POST /v1/keys":                  map[string]any{"id": "key_new", "key": "keera_sk_new"},
		"PUT /v1/guardrails/key/key_new": map[string]any{},
		"DELETE /v1/keys/key_old":        map[string]any{},
	})

	if err := keyCmd(context.Background(), []string{"rotate", "key_old"}); err != nil {
		t.Fatal(err)
	}
	issued := f.request("POST", "/v1/keys").body
	for field, want := range map[string]any{
		"team_id": "team_1", "user_id": "user_1", "alias": "a developer's laptop",
		// 90 days from the old key's own lifetime, not the two months it had left.
		"expires_in": "2160h0m0s",
	} {
		if got := issued[field]; got != want {
			t.Errorf("%s sent = %v, want %v", field, got, want)
		}
	}
	if !f.called("DELETE", "/v1/keys/key_old") {
		t.Error("the key it replaced was not revoked")
	}
}

func TestKeyRotateCopiesTheKeysOwnGuardrails(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/orgs":                   oneOrg,
		"GET /v1/keys":                   liveKey,
		"POST /v1/keys":                  map[string]any{"id": "key_new", "key": "keera_sk_new"},
		"PUT /v1/guardrails/key/key_new": map[string]any{},
		"DELETE /v1/keys/key_old":        map[string]any{},
	})

	if err := keyCmd(context.Background(), []string{"rotate", "key_old"}); err != nil {
		t.Fatal(err)
	}
	copied := f.request("PUT", "/v1/guardrails/key/key_new").body
	if got := copied["rpm"]; got != float64(120) {
		t.Errorf("rpm copied = %v, want 120", got)
	}
	models, _ := copied["allowed_models"].([]any)
	if len(models) != 1 || models[0] != "keera-speed" {
		t.Errorf("allowed_models copied = %v, want [keera-speed]", copied["allowed_models"])
	}
}

// A rotation that lost the key's own limits would hand the same person a key
// allowed more than the one it replaced, so it stops rather than finishing.
func TestKeyRotateKeepsTheOldKeyWhenTheGuardrailsCannotBeCopied(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/orgs":  oneOrg,
		"GET /v1/keys":  liveKey,
		"POST /v1/keys": map[string]any{"id": "key_new", "key": "keera_sk_new"},
		"PUT /v1/guardrails/key/key_new": failure{
			status: http.StatusInternalServerError, message: "the database went away",
		},
	})

	err := keyCmd(context.Background(), []string{"rotate", "key_old"})
	if err == nil {
		t.Fatal("rotate reported success after failing to copy the guardrails")
	}
	if !strings.Contains(err.Error(), "keera key revoke key_new") {
		t.Errorf("the error does not say how to undo the half-done rotation: %v", err)
	}
	if f.called("DELETE", "/v1/keys/key_old") {
		t.Error("the old key was revoked even though the new one has no guardrails")
	}
}

func TestKeyRotateRefusesAKeyThatIsAlreadyRevoked(t *testing.T) {
	quiet(t)
	newFakeControl(t, map[string]any{
		"GET /v1/orgs": oneOrg,
		"GET /v1/keys": map[string]any{"data": []map[string]any{{
			"id": "key_old", "org_id": "org_1", "alias": "laptop",
			"created_at": "2026-01-01T00:00:00Z", "revoked_at": "2026-02-01T00:00:00Z",
		}}},
	})

	err := keyCmd(context.Background(), []string{"rotate", "key_old"})
	if err == nil || !strings.Contains(err.Error(), "keera key create") {
		t.Errorf("error = %v, want one pointing at keera key create", err)
	}
}

func TestKeyRotateResolvesAnAliasAmongTheKeysThatStillWork(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/orgs": oneOrg,
		// The shape an organisation is in after one rotation: two keys under the
		// same alias, one of them revoked.
		"GET /v1/keys": map[string]any{"data": []map[string]any{
			{
				"id": "key_gone", "org_id": "org_1", "alias": "laptop",
				"created_at": "2026-01-01T00:00:00Z", "revoked_at": "2026-02-01T00:00:00Z",
			},
			{"id": "key_live", "org_id": "org_1", "alias": "laptop", "created_at": "2026-02-01T00:00:00Z"},
		}},
		"POST /v1/keys":            map[string]any{"id": "key_new", "key": "keera_sk_new"},
		"DELETE /v1/keys/key_live": map[string]any{},
	})

	if err := keyCmd(context.Background(), []string{"rotate", "laptop"}); err != nil {
		t.Fatal(err)
	}
	if !f.called("DELETE", "/v1/keys/key_live") {
		t.Error("rotate did not pick the key that still works")
	}
}

func TestKeyRotateRefusesAnAmbiguousAlias(t *testing.T) {
	quiet(t)
	newFakeControl(t, map[string]any{
		"GET /v1/orgs": oneOrg,
		"GET /v1/keys": map[string]any{"data": []map[string]any{
			{"id": "key_a", "org_id": "org_1", "alias": "laptop", "created_at": "2026-01-01T00:00:00Z"},
			{"id": "key_b", "org_id": "org_1", "alias": "laptop", "created_at": "2026-02-01T00:00:00Z"},
		}},
	})

	err := keyCmd(context.Background(), []string{"rotate", "laptop"})
	if err == nil {
		t.Fatal("rotate picked one of two keys with the same alias")
	}
	for _, want := range []string{"key_a", "key_b"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s to choose between: %v", want, err)
		}
	}
}

func TestKeyRevokeResolvesAnAliasAmongTheKeysThatStillWork(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/orgs": oneOrg,
		"GET /v1/keys": map[string]any{"data": []map[string]any{
			{
				"id": "key_gone", "org_id": "org_1", "alias": "laptop",
				"created_at": "2026-01-01T00:00:00Z", "revoked_at": "2026-02-01T00:00:00Z",
			},
			{"id": "key_live", "org_id": "org_1", "alias": "laptop", "created_at": "2026-02-01T00:00:00Z"},
		}},
		"DELETE /v1/keys/key_live": map[string]any{},
	})

	if err := keyCmd(context.Background(), []string{"revoke", "laptop", "--yes"}); err != nil {
		t.Fatal(err)
	}
	if !f.called("DELETE", "/v1/keys/key_live") {
		t.Error("revoke did not pick the key that still works")
	}
}

func TestKeyRevokeRefusesAnAmbiguousAlias(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/orgs": oneOrg,
		"GET /v1/keys": map[string]any{"data": []map[string]any{
			{"id": "key_a", "org_id": "org_1", "alias": "laptop", "created_at": "2026-01-01T00:00:00Z"},
			{"id": "key_b", "org_id": "org_1", "alias": "laptop", "created_at": "2026-02-01T00:00:00Z"},
		}},
	})

	err := keyCmd(context.Background(), []string{"revoke", "laptop"})
	if err == nil {
		t.Fatal("revoke picked one of two keys with the same alias")
	}
	for _, want := range []string{"key_a", "key_b"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s to choose between: %v", want, err)
		}
	}
	if f.called("DELETE", "/v1/keys/key_a") || f.called("DELETE", "/v1/keys/key_b") {
		t.Error("a key was revoked despite the alias naming two")
	}
}

// Revoking by id with --yes needs no lookup. The fake fails any call it does
// not list, which is the assertion. Without --yes the record is read for the
// prompt; that is the test below.
func TestKeyRevokeByIdLooksNothingUp(t *testing.T) {
	quiet(t)
	keyID := id.New("key")
	f := newFakeControl(t, map[string]any{
		"DELETE /v1/keys/" + keyID: map[string]any{},
	})

	if err := keyCmd(context.Background(), []string{"revoke", keyID, "--yes"}); err != nil {
		t.Fatal(err)
	}
	if !f.called("DELETE", "/v1/keys/"+keyID) {
		t.Error("the key was not revoked")
	}
}

// Revoking is immediate and cannot be undone, so it asks first like every other
// destructive command. Standard input is closed here, which is a confirmation
// that was not given.
func TestKeyRevokeAsksBeforeItRevokes(t *testing.T) {
	quiet(t)
	keyID := id.New("key")
	f := newFakeControl(t, map[string]any{
		"GET /v1/orgs": oneOrg,
		"GET /v1/keys": map[string]any{"data": []map[string]any{
			{"id": keyID, "org_id": "org_1", "alias": "laptop", "created_at": "2026-01-01T00:00:00Z"},
		}},
		"DELETE /v1/keys/" + keyID: map[string]any{},
	})

	if err := keyCmd(context.Background(), []string{"revoke", "laptop"}); err == nil {
		t.Fatal("revoke went ahead without a confirmation")
	}
	if f.called("DELETE", "/v1/keys/"+keyID) {
		t.Error("the key was revoked although nothing confirmed it")
	}
}

func TestKeyLifetimeIsWhatTheKeyWasIssuedFor(t *testing.T) {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expires := created.Add(720 * time.Hour)
	tests := []struct {
		name string
		key  store.KeySummary
		want string
	}{
		{
			name: "a key that never expires stays one",
			key:  store.KeySummary{KeyInfo: store.KeyInfo{CreatedAt: created}},
		},
		{
			name: "the original lifetime, not what is left of it",
			key: store.KeySummary{
				KeyInfo: store.KeyInfo{CreatedAt: created, ExpiresAt: &expires},
			},
			want: "720h0m0s",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := keyLifetime(tt.key); got != tt.want {
				t.Errorf("keyLifetime = %q, want %q", got, tt.want)
			}
		})
	}
}

// ------------------------------------------------------------------ models

func TestModelAddFillsInTheProviderDefaults(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/models":                map[string]any{"data": []map[string]any{}},
		"PUT /v1/models/keera-frontier": map[string]any{"alias": "keera-frontier"},
	})

	err := modelCmd(context.Background(), []string{
		"add", "keera-frontier", "--provider", "anthropic", "--backend-model", "claude-opus-5",
	})
	if err != nil {
		t.Fatal(err)
	}
	body := f.request("PUT", "/v1/models/keera-frontier").body
	if got := body["backends"].([]any); len(got) != 1 || got[0] != "https://api.anthropic.com/v1" {
		t.Errorf("backends = %v", got)
	}
	if body["api_key_env"] != "ANTHROPIC_API_KEY" {
		t.Errorf("api_key_env = %v", body["api_key_env"])
	}
	if body["input_micros_per_mtok"] != float64(5_000_000) {
		t.Errorf("input price = %v, want the provider's", body["input_micros_per_mtok"])
	}
	if body["max_context"] != float64(1_000_000) {
		t.Errorf("max_context = %v", body["max_context"])
	}
	if _, sent := body["api_key"]; sent {
		t.Error("a credential was sent when none was given")
	}
}

func TestModelAddRefusesAModelThatExists(t *testing.T) {
	quiet(t)
	newFakeControl(t, map[string]any{
		"GET /v1/models": map[string]any{"data": []map[string]any{
			{"alias": "keera-speed", "backends": []string{"http://vllm:8000/v1"}, "backend_model": "qwen"},
		}},
	})

	err := modelCmd(context.Background(), []string{"add", "keera-speed", "--backend", "http://vllm:8000/v1", "--backend-model", "qwen"})
	if err == nil || !strings.Contains(err.Error(), "keera model set") {
		t.Fatalf("error = %v, want it to point at keera model set", err)
	}
}

func TestModelSetKeepsWhatItWasNotGiven(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/models": map[string]any{"data": []map[string]any{{
			"alias": "keera-speed", "kind": "chat",
			"backends":      []string{"http://vllm-a:8000/v1", "http://vllm-b:8000/v1"},
			"backend_model": "qwen3-8b", "max_context": 32768,
			"input_micros_per_mtok": 100_000, "output_micros_per_mtok": 300_000,
			"api_key_env": "VLLM_API_KEY", "enabled": true,
		}}},
		"PUT /v1/models/keera-speed": map[string]any{"alias": "keera-speed"},
	})

	if err := modelCmd(context.Background(), []string{"set", "keera-speed", "--price-out", "0.5"}); err != nil {
		t.Fatal(err)
	}
	body := f.request("PUT", "/v1/models/keera-speed").body
	if body["output_micros_per_mtok"] != float64(500_000) {
		t.Errorf("output price = %v, want 500000", body["output_micros_per_mtok"])
	}
	// Everything else has to survive: the endpoint replaces the entry.
	if body["input_micros_per_mtok"] != float64(100_000) {
		t.Errorf("input price = %v, want it left alone", body["input_micros_per_mtok"])
	}
	if len(body["backends"].([]any)) != 2 {
		t.Errorf("backends = %v, want both left alone", body["backends"])
	}
	if body["backend_model"] != "qwen3-8b" || body["max_context"] != float64(32768) ||
		body["api_key_env"] != "VLLM_API_KEY" || body["enabled"] != true {
		t.Errorf("set changed a field it was not given: %v", body)
	}
}

func TestModelDisableKeepsTheEntryAndOnlyStopsServingIt(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/models": map[string]any{"data": []map[string]any{{
			"alias": "keera-speed", "kind": "chat", "backends": []string{"http://vllm:8000/v1"},
			"backend_model": "qwen3-8b", "enabled": true,
		}}},
		"PUT /v1/models/keera-speed": map[string]any{"alias": "keera-speed"},
	})

	if err := modelCmd(context.Background(), []string{"disable", "keera-speed"}); err != nil {
		t.Fatal(err)
	}
	body := f.request("PUT", "/v1/models/keera-speed").body
	if body["enabled"] != false || body["backend_model"] != "qwen3-8b" {
		t.Errorf("disable sent %v", body)
	}
}

// A model the catalogue file declares is applied again on every start, so the
// CLI says no here rather than writing a change that disappears at the next one.
func TestModelSetRefusesAModelTheCatalogueFileDeclares(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/models": map[string]any{"data": []map[string]any{{
			"alias": "keera-speed", "kind": "chat", "backends": []string{"http://vllm:8000/v1"},
			"backend_model": "qwen3-8b", "enabled": true, "managed": true,
		}}},
	})

	err := modelCmd(context.Background(), []string{"set", "keera-speed", "--price-out", "0.5"})
	if err == nil || !strings.Contains(err.Error(), "catalogue file") {
		t.Fatalf("error = %v, want it to name the catalogue file", err)
	}
	for _, r := range f.seen {
		if r.method == "PUT" {
			t.Fatal("the CLI wrote the change anyway")
		}
	}
}

func TestModelDisableRefusesAModelTheCatalogueFileDeclares(t *testing.T) {
	quiet(t)
	newFakeControl(t, map[string]any{
		"GET /v1/models": map[string]any{"data": []map[string]any{{
			"alias": "keera-speed", "kind": "chat", "backends": []string{"http://vllm:8000/v1"},
			"backend_model": "qwen3-8b", "enabled": true, "managed": true,
		}}},
	})

	err := modelCmd(context.Background(), []string{"disable", "keera-speed"})
	if err == nil || !strings.Contains(err.Error(), "catalogue file") {
		t.Fatalf("error = %v, want it to name the catalogue file", err)
	}
}

func TestModelDeleteRefusesAModelTheCatalogueFileDeclares(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/models": map[string]any{"data": []map[string]any{{
			"alias": "keera-speed", "kind": "chat", "backends": []string{"http://vllm:8000/v1"},
			"backend_model": "qwen3-8b", "enabled": true, "managed": true,
		}}},
	})

	// --yes, so what stops it is the rule and not the confirmation prompt.
	err := modelCmd(context.Background(), []string{"delete", "keera-speed", "--yes"})
	if err == nil || !strings.Contains(err.Error(), "catalogue file") {
		t.Fatalf("error = %v, want it to name the catalogue file", err)
	}
	for _, r := range f.seen {
		if r.method == "DELETE" {
			t.Fatal("the model was deleted anyway")
		}
	}
}

// The credential is the one field no catalogue file carries, so it is the one
// change a declared alias still takes.
func TestModelSetStoresACredentialOnADeclaredModel(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/models": map[string]any{"data": []map[string]any{{
			"alias": "keera-frontier", "kind": "chat",
			"backends":      []string{"https://api.anthropic.com/v1"},
			"backend_model": "claude-opus-5", "enabled": true, "managed": true,
		}}},
		"PUT /v1/models/keera-frontier": map[string]any{"alias": "keera-frontier"},
	})

	if err := modelCmd(context.Background(), []string{"set", "keera-frontier", "--api-key", "sk-live"}); err != nil {
		t.Fatal(err)
	}
	body := f.request("PUT", "/v1/models/keera-frontier").body
	if body["api_key"] != "sk-live" {
		t.Errorf("api_key sent = %v", body["api_key"])
	}
	if body["backend_model"] != "claude-opus-5" || body["enabled"] != true {
		t.Errorf("the declaration changed: %v", body)
	}
	if body["from_catalogue"] == true {
		t.Error("a credential change claimed to be the catalogue being applied")
	}
}

func TestModelCheckFailsWhenTheProbeDoes(t *testing.T) {
	quiet(t)
	newFakeControl(t, map[string]any{
		"POST /v1/models/keera-speed/check": map[string]any{
			"alias": "keera-speed", "reachable": true, "status": 200,
			"tool_call_as_text": true, "ok": false,
		},
	})

	// A rollout gated on a check should not have to read the table to find out.
	err := modelCmd(context.Background(), []string{"check", "keera-speed"})
	if err == nil || !strings.Contains(err.Error(), "did not pass") {
		t.Fatalf("error = %v, want a failed command", err)
	}
}

func TestModelCheckPointsAFileAtValidate(t *testing.T) {
	quiet(t)
	newFakeControl(t, map[string]any{})
	path := filepath.Join(t.TempDir(), "models.yaml")
	if err := os.WriteFile(path, []byte("models: []"), 0o600); err != nil {
		t.Fatal(err)
	}

	// `keera model check <file>` is what this command used to mean.
	err := modelCmd(context.Background(), []string{"check", path})
	if err == nil || !strings.Contains(err.Error(), "keera model validate") {
		t.Fatalf("error = %v, want the new name for validating a file", err)
	}
}

func TestModelApplyUpsertsEveryModelInTheFile(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"PUT /v1/models/keera-speed":    map[string]any{"alias": "keera-speed"},
		"PUT /v1/models/keera-frontier": map[string]any{"alias": "keera-frontier"},
	})
	path := filepath.Join(t.TempDir(), "models.yaml")
	file := "models:\n" +
		"  - alias: keera-speed\n" +
		"    backends: [http://vllm:8000/v1]\n" +
		"    backend_model: qwen3-8b\n" +
		"  - alias: keera-frontier\n" +
		"    provider: anthropic\n" +
		"    backend_model: claude-opus-5\n"
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := modelCmd(context.Background(), []string{"apply", path}); err != nil {
		t.Fatal(err)
	}
	// Applying the file is the one write that may touch a model the file
	// declares, so it says that is what it is.
	if got := f.request("PUT", "/v1/models/keera-speed").body["from_catalogue"]; got != true {
		t.Errorf("from_catalogue = %v, want the write to declare itself the catalogue", got)
	}
	if got := f.request("PUT", "/v1/models/keera-frontier").body["input_micros_per_mtok"]; got != float64(5_000_000) {
		t.Errorf("the provider's price was not applied: %v", got)
	}
}

func TestModelApplyValidatesBeforeItWritesAnything(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{})
	path := filepath.Join(t.TempDir(), "models.yaml")
	// The second entry is missing its backend_model, so neither is applied.
	file := "models:\n" +
		"  - alias: keera-speed\n" +
		"    backends: [http://vllm:8000/v1]\n" +
		"    backend_model: qwen3-8b\n" +
		"  - alias: broken\n" +
		"    backends: [http://vllm:8000/v1]\n"
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := modelCmd(context.Background(), []string{"apply", path}); err == nil {
		t.Fatal("a catalogue with a broken entry was applied")
	}
	if len(f.seen) != 0 {
		t.Errorf("the control plane was called anyway: %v", f.seen)
	}
}

// ------------------------------------------------------- flags and helpers

func TestApplyModelFlagsLeavesUngivenFieldsAlone(t *testing.T) {
	current := declared(policy.Model{
		Alias: "keera-speed", Kind: policy.KindChat,
		Backends: []string{"http://vllm:8000/v1"}, BackendModel: "qwen3-8b",
		InputMicrosPerMTok: 100_000, OutputMicrosPerMTok: 300_000,
		MaxContext: 32768, APIKeyEnv: "VLLM_API_KEY", Enabled: true,
	})
	f := &modelFlags{maxContext: -1, priceIn: -1, priceOut: 0, priceCached: -1}

	got, err := catalog.ParseModel(applyModelFlags(current, f))
	if err != nil {
		t.Fatal(err)
	}
	// 0 is a price, not an absence: a model can be deliberately unbilled.
	if got.OutputMicrosPerMTok != 0 {
		t.Errorf("output price = %d, want 0", got.OutputMicrosPerMTok)
	}
	if got.InputMicrosPerMTok != 100_000 || got.MaxContext != 32768 || !got.Enabled {
		t.Errorf("an ungiven field changed: %+v", got)
	}
}

// A product id is part of an address, not a field of a stored model, so
// changing one has to rebuild the address rather than patch it: the stored
// backends already have the old id written in and no placeholder left.
func TestProductIDFlagRebuildsTheAddress(t *testing.T) {
	current := declared(policy.Model{
		Alias: "keera-swiss", Kind: policy.KindChat, Provider: "infomaniak",
		Backends:     []string{"https://api.infomaniak.com/2/ai/100234/openai/v1"},
		BackendModel: "google/gemma-4-31B-it", MaxContext: 100_000,
		InputMicrosPerMTok: 200_000, OutputMicrosPerMTok: 400_000, Enabled: true,
	})
	f := &modelFlags{productID: "999888", maxContext: -1, priceIn: -1, priceOut: -1, priceCached: -1}

	got, err := catalog.ParseModel(applyModelFlags(current, f))
	if err != nil {
		t.Fatal(err)
	}
	const want = "https://api.infomaniak.com/2/ai/999888/openai/v1"
	if len(got.Backends) != 1 || got.Backends[0] != want {
		t.Errorf("Backends = %v, want [%s]", got.Backends, want)
	}
	// It is a declared field like any other, so it is not "only the credential"
	// and a model the catalogue file owns still refuses it.
	if onlyCredential(f) {
		t.Error("--product-id counted as a credential-only change")
	}
}

// A backend given alongside it is the operator's own answer and wins, the way
// every other value they state wins over the provider's table.
func TestProductIDFlagGivesWayToAStatedBackend(t *testing.T) {
	f := &modelFlags{
		productID: "999888", backends: stringList{"http://egress.corp:8080/v1"},
		maxContext: -1, priceIn: -1, priceOut: -1, priceCached: -1,
	}
	got, err := catalog.ParseModel(applyModelFlags(catalog.Model{
		Alias: "keera-swiss", Provider: "infomaniak", BackendModel: "google/gemma-4-31B-it",
	}, f))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Backends) != 1 || got.Backends[0] != "http://egress.corp:8080/v1" {
		t.Errorf("Backends = %v, want the declared proxy", got.Backends)
	}
}

func TestStringListTakesRepeatsAndCommas(t *testing.T) {
	var l stringList
	if err := l.Set("http://a:8000/v1, http://b:8000/v1"); err != nil {
		t.Fatal(err)
	}
	if err := l.Set("http://c:8000/v1"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(l, "|") != "http://a:8000/v1|http://b:8000/v1|http://c:8000/v1" {
		t.Errorf("backends = %v", l)
	}
}

func TestCredentialFlags(t *testing.T) {
	if got, err := credential(&modelFlags{}); err != nil || got != nil {
		t.Errorf("nothing given: %v, %v - want the stored one left alone", got, err)
	}
	got, err := credential(&modelFlags{noAPIKey: true})
	if err != nil || got == nil || *got != "" {
		t.Errorf("--no-api-key: %v, %v - want an empty credential", got, err)
	}
	if _, err := credential(&modelFlags{apiKey: "sk-1", noAPIKey: true}); err == nil {
		t.Error("--api-key with --no-api-key was accepted")
	}
}

func TestTextOrFileReadsAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(path, []byte("Answer in Swiss German.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := textOrFile("@" + path)
	if err != nil || got != "Answer in Swiss German." {
		t.Errorf("textOrFile = %q, %v", got, err)
	}
}

func TestCredentialSourceSaysWhereTheKeyComesFrom(t *testing.T) {
	cases := []struct {
		model policy.Model
		want  string
	}{
		{policy.Model{}, "(none)"},
		{policy.Model{APIKeyEnv: "ANTHROPIC_API_KEY"}, "$ANTHROPIC_API_KEY"},
		{policy.Model{HasAPIKey: true}, "stored"},
		{policy.Model{HasAPIKey: true, APIKeyEnv: "ANTHROPIC_API_KEY"}, "stored (overrides ANTHROPIC_API_KEY)"},
	}
	for _, c := range cases {
		if got := credentialSource(c.model); got != c.want {
			t.Errorf("credentialSource(%+v) = %q, want %q", c.model, got, c.want)
		}
	}
}

func TestLooksLikeCatalogueFileIgnoresAModelThatShareAName(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, name := range []string{"keera-speed", "models.yaml"} {
		if err := os.WriteFile(name, []byte("models: []"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A model is checked against the live backend even when something of that
	// name is sitting in the working directory.
	if looksLikeCatalogueFile("keera-speed") {
		t.Error("a model was taken for a catalogue file")
	}
	if !looksLikeCatalogueFile("models.yaml") {
		t.Error("a catalogue file was taken for a model")
	}
	if looksLikeCatalogueFile("gone.yaml") {
		t.Error("a file that is not there was taken for a catalogue")
	}
}

func TestFilterAddSendsTheModelAndTheInstruction(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/orgs": oneOrg,
		"PUT /v1/filters/redact-secrets": map[string]any{
			"alias": "redact-secrets", "model": "keera-guard", "prompt": "Remove credentials.",
		},
	})

	if err := Run(context.Background(), []string{"filter", "add", "redact-secrets",
		"--model", "keera-guard", "--prompt", "Remove credentials."}); err != nil {
		t.Fatalf("filter add: %v", err)
	}

	req := f.request("PUT", "/v1/filters/redact-secrets")
	if req.body["model"] != "keera-guard" {
		t.Errorf("model = %v, want the model it runs on", req.body["model"])
	}
	if req.body["prompt"] != "Remove credentials." {
		t.Errorf("prompt = %v, want the instruction", req.body["prompt"])
	}
}

func TestFilterSetChangesOneFieldAndKeepsTheRest(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/orgs": oneOrg,
		"GET /v1/filters": map[string]any{"data": []map[string]any{{
			"alias": "redact-secrets", "model": "keera-guard",
			"prompt": "Remove credentials.", "description": "the original",
		}}},
		"PUT /v1/filters/redact-secrets": map[string]any{"alias": "redact-secrets"},
	})

	if err := Run(context.Background(), []string{"filter", "set", "redact-secrets",
		"--model", "keera-guard-2"}); err != nil {
		t.Fatalf("filter set: %v", err)
	}

	req := f.request("PUT", "/v1/filters/redact-secrets")
	if req.body["model"] != "keera-guard-2" {
		t.Errorf("model = %v, want the new alias", req.body["model"])
	}
	if req.body["prompt"] != "Remove credentials." {
		t.Errorf("prompt = %v; changing the model must not clear the instruction",
			req.body["prompt"])
	}
	if req.body["description"] != "the original" {
		t.Errorf("description = %v; it was not mentioned and must be kept",
			req.body["description"])
	}
}

func TestFilterAddInShadowSaysSo(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/orgs":                   oneOrg,
		"PUT /v1/filters/redact-secrets": map[string]any{"alias": "redact-secrets"},
	})

	if err := Run(context.Background(), []string{"filter", "add", "redact-secrets",
		"--model", "keera-guard", "--prompt", "Remove credentials.", "--shadow"}); err != nil {
		t.Fatalf("filter add --shadow: %v", err)
	}
	if req := f.request("PUT", "/v1/filters/redact-secrets"); req.body["shadow"] != true {
		t.Errorf("shadow = %v, want the filter written not enforcing", req.body["shadow"])
	}
}

func TestFilterSetKeepsShadowWhenNeitherFlagIsGiven(t *testing.T) {
	quiet(t)
	// Why there are two flags: a `set` naming neither must not switch a
	// shadow filter to enforcing.
	f := newFakeControl(t, map[string]any{
		"GET /v1/orgs": oneOrg,
		"GET /v1/filters": map[string]any{"data": []map[string]any{{
			"alias": "redact-secrets", "model": "keera-guard",
			"prompt": "Remove credentials.", "shadow": true,
		}}},
		"PUT /v1/filters/redact-secrets": map[string]any{"alias": "redact-secrets"},
	})

	if err := Run(context.Background(), []string{"filter", "set", "redact-secrets",
		"--description", "now with a description"}); err != nil {
		t.Fatalf("filter set: %v", err)
	}
	if req := f.request("PUT", "/v1/filters/redact-secrets"); req.body["shadow"] != true {
		t.Errorf("shadow = %v; a field nobody mentioned must be kept", req.body["shadow"])
	}
}

func TestFilterSetEnforceTakesItOutOfShadow(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/orgs": oneOrg,
		"GET /v1/filters": map[string]any{"data": []map[string]any{{
			"alias": "redact-secrets", "model": "keera-guard",
			"prompt": "Remove credentials.", "shadow": true,
		}}},
		"PUT /v1/filters/redact-secrets": map[string]any{"alias": "redact-secrets"},
	})

	if err := Run(context.Background(), []string{"filter", "set", "redact-secrets",
		"--enforce"}); err != nil {
		t.Fatalf("filter set --enforce: %v", err)
	}
	req := f.request("PUT", "/v1/filters/redact-secrets")
	if req.body["shadow"] != false {
		t.Errorf("shadow = %v, want it enforcing", req.body["shadow"])
	}
	// And nothing else moved: what the week in shadow measured is what goes
	// live, which it would not be if promoting also rewrote the instruction.
	if req.body["prompt"] != "Remove credentials." {
		t.Errorf("prompt = %v, want the instruction that was measured", req.body["prompt"])
	}
}

func TestFilterShadowAndEnforceTogetherIsRefused(t *testing.T) {
	quiet(t)
	newFakeControl(t, map[string]any{"GET /v1/orgs": oneOrg})

	err := Run(context.Background(), []string{"filter", "set", "redact-secrets",
		"--shadow", "--enforce"})
	if err == nil {
		t.Fatal("both flags at once was accepted; one of them was silently ignored")
	}
}

func TestFilterReportAsksForTheWindowAndPrintsTheSplit(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/orgs": oneOrg,
		"GET /v1/filters/redact-secrets/report": map[string]any{
			"currency":   "CHF",
			"filter":     map[string]any{"alias": "redact-secrets", "model": "keera-guard"},
			"team_names": map[string]any{"team_1": "Payments Platform"},
			"report": map[string]any{
				"filter": "redact-secrets", "runs": 400, "pass": 300, "rewrote": 80,
				"refused": 16, "errors": 4, "requests": 1000,
				"cost_micros": 2_000_000, "org_cost_micros": 40_000_000,
				"latency_median_ms": 210, "latency_p95_ms": 900,
				"segments": 1600, "changed": 120,
				"teams": []map[string]any{
					{"team_id": "team_1", "runs": 100, "refused": 16, "rewrote": 20,
						"cost_micros": 500_000},
				},
			},
		},
	})

	out := captured(t, func() error {
		return Run(context.Background(), []string{"filter", "report", "redact-secrets",
			"--since", "168h"})
	})

	req := f.request("GET", "/v1/filters/redact-secrets/report")
	if !strings.Contains(req.query, "from=") {
		t.Errorf("query = %q, want the window it was asked for", req.query)
	}
	for _, want := range []string{
		"400 of the organisation's 1000 requests (40.0%)", // is it firing at all
		"4.0%",                    // and how much of that was a refusal
		"210ms median, 900ms p95", // what the request waiting for it paid
		"Payments Platform",       // and who is living with it
		"16.0%",                   // the team's own rate, not the org's
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report did not carry %q:\n%s", want, out)
		}
	}
}

func TestPolicySetAppliesFiltersAndClearsThemByFlag(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/guardrails/team/team_1": map[string]any{"rpm": 120},
		"PUT /v1/guardrails/team/team_1": map[string]any{},
	})

	if err := Run(context.Background(), []string{"guardrail", "set", "team", "team_1",
		"--filters", "redact-secrets,redact-clients"}); err != nil {
		t.Fatalf("guardrail set: %v", err)
	}
	req := f.request("PUT", "/v1/guardrails/team/team_1")
	got, _ := req.body["filters"].([]any)
	if len(got) != 2 || got[0] != "redact-secrets" || got[1] != "redact-clients" {
		t.Errorf("filters = %v, want both in the order given", req.body["filters"])
	}
	// The limits already stored are read back first, so naming a filter does
	// not silently drop the rate limit somebody else set.
	if req.body["rpm"] != float64(120) {
		t.Errorf("rpm = %v; setting one field must not clear the others", req.body["rpm"])
	}
}

func TestPolicySetClearsFiltersOnlyWithItsOwnFlag(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/guardrails/team/team_1": map[string]any{"filters": []string{"redact-secrets"}},
		"PUT /v1/guardrails/team/team_1": map[string]any{},
	})

	if err := Run(context.Background(), []string{"guardrail", "set", "team", "team_1",
		"--no-filters"}); err != nil {
		t.Fatalf("guardrail set: %v", err)
	}
	if got, present := f.request("PUT", "/v1/guardrails/team/team_1").body["filters"]; present {
		t.Errorf("filters = %v, want them gone", got)
	}
}

// The cached-input rate is set like the other prices, and survives a change
// to something else. Dropping it would charge cached tokens at full price.
func TestModelSetTakesTheCachedInputPrice(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/models": map[string]any{"data": []map[string]any{{
			"alias": "keera-frontier", "kind": "chat",
			"backends":      []string{"https://api.openai.com/v1"},
			"backend_model": "gpt-5.1", "max_context": 400_000,
			"input_micros_per_mtok": 1_250_000, "output_micros_per_mtok": 10_000_000,
			"cached_input_micros_per_mtok": 125_000,
			"enabled":                      true,
		}}},
		"PUT /v1/models/keera-frontier": map[string]any{"alias": "keera-frontier"},
	})

	if err := modelCmd(context.Background(),
		[]string{"set", "keera-frontier", "--price-cached", "0.4"}); err != nil {
		t.Fatal(err)
	}
	body := f.request("PUT", "/v1/models/keera-frontier").body
	if body["cached_input_micros_per_mtok"] != float64(400_000) {
		t.Errorf("cached price = %v, want 400000", body["cached_input_micros_per_mtok"])
	}
	if body["input_micros_per_mtok"] != float64(1_250_000) {
		t.Errorf("input price = %v, want it left alone", body["input_micros_per_mtok"])
	}

}

// And the other way round: changing the input price must not drop the cached
// rate the entry already carried. The endpoint replaces the whole entry, so a
// field the CLI forgot to restate is a field that is gone.
func TestModelSetKeepsTheCachedInputPriceItWasNotGiven(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/models": map[string]any{"data": []map[string]any{{
			"alias": "keera-frontier", "kind": "chat",
			"backends":      []string{"https://api.openai.com/v1"},
			"backend_model": "gpt-5.1", "max_context": 400_000,
			"input_micros_per_mtok": 1_250_000, "output_micros_per_mtok": 10_000_000,
			"cached_input_micros_per_mtok": 125_000,
			"enabled":                      true,
		}}},
		"PUT /v1/models/keera-frontier": map[string]any{"alias": "keera-frontier"},
	})

	if err := modelCmd(context.Background(),
		[]string{"set", "keera-frontier", "--price-in", "1"}); err != nil {
		t.Fatal(err)
	}
	body := f.request("PUT", "/v1/models/keera-frontier").body
	if body["cached_input_micros_per_mtok"] != float64(125_000) {
		t.Errorf("cached price = %v, want the stored 125000 left alone",
			body["cached_input_micros_per_mtok"])
	}
	if body["input_micros_per_mtok"] != float64(1_000_000) {
		t.Errorf("input price = %v, want the new 1000000", body["input_micros_per_mtok"])
	}
}
