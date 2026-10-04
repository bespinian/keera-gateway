package cli

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bespinian/keera-gateway/internal/catalog"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/id"
	"github.com/bespinian/keera-gateway/internal/policy"
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
	// Who is calling, for the commands that ask before choosing a default.
	// An administrator, so no default is chosen unless a test says otherwise.
	if _, set := handlers["GET /v1/me"]; !set {
		handlers["GET /v1/me"] = map[string]any{"role": "admin", "org_id": "org_1"}
	}
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

var paymentsTeam = map[string]any{"data": []map[string]any{
	{"id": "team_1", "org_id": "org_1", "name": "Payments"},
}}

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

// A rename sends only the name, so the domain stays what it was.
func TestOrgSetRenamesWithoutTouchingTheDomain(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"PATCH /v1/orgs/org_2": map[string]any{"id": "org_2", "name": "Another Bank AG"},
	})

	if err := Run(context.Background(), []string{"org", "set", "org_2",
		"--name", " Another Bank AG "}); err != nil {
		t.Fatal(err)
	}
	body := f.request("PATCH", "/v1/orgs/org_2").body
	if body["name"] != "Another Bank AG" {
		t.Errorf("name sent = %v, want Another Bank AG", body["name"])
	}
	if _, present := body["email_domain"]; present {
		t.Errorf("email_domain was sent with a rename: %v", body)
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
	// Sent as an empty string rather than left out: the endpoint leaves an
	// absent field as it is.
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

// The control plane copies the team, the person, the lifetime and the key's
// own guardrails in one transaction. The CLI only names the key and passes on
// what was asked to change.
func TestKeyRotateAsksTheControlPlane(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/orgs": oneOrg,
		"GET /v1/keys": liveKey,
		"POST /v1/keys/key_old/rotate": map[string]any{
			"id": "key_new", "key": "keera_sk_new", "replaced": "key_old",
		},
	})

	if err := keyCmd(context.Background(),
		[]string{"rotate", "a developer's laptop", "--alias", "new laptop"}); err != nil {
		t.Fatal(err)
	}
	sent := f.request("POST", "/v1/keys/key_old/rotate").body
	if sent["alias"] != "new laptop" || sent["expires_in"] != "" {
		t.Errorf("sent %v, want the new alias and no expiry", sent)
	}
	if f.called("POST", "/v1/keys") || f.called("DELETE", "/v1/keys/key_old") {
		t.Errorf("the CLI rotated by hand: %v", f.seen)
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
		"POST /v1/keys/key_live/rotate": map[string]any{"id": "key_new", "key": "keera_sk_new"},
	})

	if err := keyCmd(context.Background(), []string{"rotate", "laptop"}); err != nil {
		t.Fatal(err)
	}
	if !f.called("POST", "/v1/keys/key_live/rotate") {
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
// that was not given. The prompt goes to stderr, so `--json` output stays JSON.
func TestKeyRevokeAsksBeforeItRevokes(t *testing.T) {
	stderr := captureStderr(t)
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
	if said := stderr(); !strings.Contains(said, "Revoking laptop") ||
		!strings.Contains(said, "Type the alias to confirm:") {
		t.Errorf("stderr = %q, want the prompt on it", said)
	}
}

// ------------------------------------------------------------------ models

func TestModelAddFillsInTheProviderDefaults(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/orgs":                  oneOrg,
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
		"GET /v1/orgs": oneOrg,
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
		"GET /v1/orgs": oneOrg,
		"GET /v1/models": map[string]any{"data": []map[string]any{{
			"alias": "keera-speed", "kind": "chat",
			"backends":      []string{"http://vllm-a:8000/v1", "http://vllm-b:8000/v1"},
			"backend_model": "qwen3-8b", "max_context": 32768,
			"input_micros_per_mtok": 100_000, "output_micros_per_mtok": 300_000,
			"enabled": true,
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
		body["enabled"] != true {
		t.Errorf("set changed a field it was not given: %v", body)
	}
}

func TestModelDisableKeepsTheEntryAndOnlyStopsServingIt(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/orgs": oneOrg,
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

// Setting a key sends the model as it is, so nothing else about it changes.
func TestModelSetStoresACredentialAndKeepsTheRest(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/orgs": oneOrg,
		"GET /v1/models": map[string]any{"data": []map[string]any{{
			"alias": "keera-frontier", "kind": "chat",
			"backends":      []string{"https://api.anthropic.com/v1"},
			"backend_model": "claude-opus-5", "location": "usa", "enabled": true,
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
}

func TestModelCheckFailsWhenTheProbeDoes(t *testing.T) {
	quiet(t)
	newFakeControl(t, map[string]any{
		"GET /v1/orgs":   oneOrg,
		"GET /v1/models": map[string]any{"data": []map[string]any{{"alias": "keera-speed"}}},
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

func TestModelApplyUpsertsEveryModelInTheFile(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/orgs":                  oneOrg,
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
	if got := f.request("PUT", "/v1/models/keera-speed").body["backend_model"]; got != "qwen3-8b" {
		t.Errorf("backend_model = %v, want the file's", got)
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
		MaxContext: 32768, Enabled: true,
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

// Where a model runs belongs to its provider, so a new provider brings its own
// location rather than keeping the old one's.
func TestProviderFlagBringsItsOwnLocation(t *testing.T) {
	current := declared(policy.Model{
		Alias: "keera-big", Kind: policy.KindChat, Provider: "anthropic",
		Backends: []string{"https://api.anthropic.com/v1"}, BackendModel: "claude-opus-5",
		ReleaseDate: "2026-01-01", Location: "usa", Enabled: true,
	})
	f := &modelFlags{
		provider: "stepping-stone", backends: stringList{"https://llm.stoney-cloud.com/v1"},
		backendModel: "Qwen/Qwen3-Coder-Next",
		maxContext:   -1, priceIn: -1, priceOut: -1, priceCached: -1,
	}
	got, err := catalog.ParseModel(applyModelFlags(current, f))
	if err != nil {
		t.Fatal(err)
	}
	if got.Location != "ch" {
		t.Errorf("location = %q, want stepping stone's ch", got.Location)
	}
	if got.ReleaseDate == "2026-01-01" {
		t.Error("the old model's release date was kept for a different model")
	}

	// A stated location wins, and changing only the price keeps both.
	f = &modelFlags{location: "onprem", maxContext: -1, priceIn: 1, priceOut: -1, priceCached: -1}
	got, err = catalog.ParseModel(applyModelFlags(current, f))
	if err != nil {
		t.Fatal(err)
	}
	if got.Location != "onprem" || got.ReleaseDate != "2026-01-01" {
		t.Errorf("location = %q, release date = %q, want onprem and the kept date",
			got.Location, got.ReleaseDate)
	}
}

// The old provider's address and prices would send the requests to the wrong
// place and bill them at the wrong rate.
func TestProviderFlagBringsItsOwnAddressAndPrices(t *testing.T) {
	current := declared(policy.Model{
		Alias: "keera-coder", Kind: policy.KindChat, Provider: "anthropic",
		Backends: []string{"https://api.anthropic.com/v1"}, BackendModel: "claude-opus-5",
		InputMicrosPerMTok: 5_000_000, OutputMicrosPerMTok: 25_000_000,
		MaxContext: 1_000_000, Enabled: true,
	})
	f := &modelFlags{
		provider: "stepping-stone", backendModel: "Qwen/Qwen3-Coder-Next",
		maxContext: -1, priceIn: -1, priceOut: -1, priceCached: -1,
	}
	got, err := catalog.ParseModel(applyModelFlags(current, f))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Backends) != 1 || got.Backends[0] == "https://api.anthropic.com/v1" {
		t.Errorf("Backends = %v, want stepping stone's", got.Backends)
	}
	if got.InputMicrosPerMTok == 5_000_000 || got.MaxContext == 1_000_000 {
		t.Errorf("the old provider's price or context window was kept: %+v", got)
	}
}

// Another model of the same provider has its own prices, context window,
// description and release date. Keeping the old model's would bill it wrong.
func TestBackendModelFlagBringsTheNewModelsValues(t *testing.T) {
	current := declared(policy.Model{
		Alias: "keera-coder", Kind: policy.KindChat, Provider: "stepping-stone",
		Backends: []string{"https://llm.stoney-cloud.com/v1"}, BackendModel: "Qwen/Qwen3-Coder-Next",
		Description: "made for code", InputMicrosPerMTok: 340_000, OutputMicrosPerMTok: 1_700_000,
		CachedInputMicrosPerMTok: 51_000, MaxContext: 256_000, ReleaseDate: "2026-02-03",
		Location: "ch", Enabled: true,
	})
	f := &modelFlags{
		backendModel: "openai/gpt-oss-120b",
		maxContext:   -1, priceIn: -1, priceOut: -1, priceCached: -1,
	}
	got, err := catalog.ParseModel(applyModelFlags(current, f))
	if err != nil {
		t.Fatal(err)
	}
	if got.InputMicrosPerMTok != 400_000 || got.OutputMicrosPerMTok != 1_600_000 ||
		got.CachedInputMicrosPerMTok != 60_000 || got.MaxContext != 128_000 ||
		got.ReleaseDate != "2025-08-05" || got.Description == "made for code" {
		t.Errorf("the old model's values were kept: %+v", got)
	}

	// A value given on the same command wins.
	f.priceIn = 1
	got, err = catalog.ParseModel(applyModelFlags(current, f))
	if err != nil {
		t.Fatal(err)
	}
	if got.InputMicrosPerMTok != 1_000_000 {
		t.Errorf("input price = %d, want the one given", got.InputMicrosPerMTok)
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
	if got, err := credential("", false); err != nil || got != nil {
		t.Errorf("nothing given: %v, %v - want the stored one left alone", got, err)
	}
	got, err := credential("", true)
	if err != nil || got == nil || *got != "" {
		t.Errorf("--no-api-key: %v, %v - want an empty credential", got, err)
	}
	if _, err := credential("sk-1", true); err == nil {
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

func TestCredentialSourceSaysWhetherAKeyIsStored(t *testing.T) {
	cases := []struct {
		model policy.Model
		want  string
	}{
		{policy.Model{}, "(none)"},
		{policy.Model{HasAPIKey: true}, "stored"},
	}
	for _, c := range cases {
		if got := credentialSource(c.model); got != c.want {
			t.Errorf("credentialSource(%+v) = %q, want %q", c.model, got, c.want)
		}
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

// edit and update are other names for set, so they keep what is not given too.
func TestFilterAndRouterEditAndUpdateKeepTheRest(t *testing.T) {
	for _, verb := range []string{"edit", "update"} {
		t.Run(verb, func(t *testing.T) {
			quiet(t)
			f := newFakeControl(t, map[string]any{
				"GET /v1/orgs": oneOrg,
				"GET /v1/filters": map[string]any{"data": []map[string]any{{
					"alias": "redact-secrets", "model": "keera-guard",
					"prompt": "Remove credentials.", "description": "the original",
				}}},
				"PUT /v1/filters/redact-secrets": map[string]any{"alias": "redact-secrets"},
				"GET /v1/routers": map[string]any{"data": []map[string]any{{
					"alias": "auto", "model": "keera-speed", "destinations": []string{"a", "b"},
					"prompt": "Pick one.", "description": "the original",
				}}},
				"PUT /v1/routers/auto": map[string]any{"alias": "auto"},
			})

			if err := Run(context.Background(), []string{"filter", verb, "redact-secrets",
				"--model", "keera-guard-2"}); err != nil {
				t.Fatalf("filter %s: %v", verb, err)
			}
			if req := f.request("PUT", "/v1/filters/redact-secrets"); req.body["prompt"] != "Remove credentials." ||
				req.body["description"] != "the original" {
				t.Errorf("filter %s dropped what was not given: %v", verb, req.body)
			}

			if err := Run(context.Background(), []string{"router", verb, "auto",
				"--description", "changed"}); err != nil {
				t.Fatalf("router %s: %v", verb, err)
			}
			if req := f.request("PUT", "/v1/routers/auto"); req.body["prompt"] != "Pick one." ||
				req.body["model"] != "keera-speed" {
				t.Errorf("router %s dropped what was not given: %v", verb, req.body)
			}
		})
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
		"GET /v1/orgs":                   oneOrg,
		"GET /v1/teams":                  paymentsTeam,
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
		"GET /v1/orgs":                   oneOrg,
		"GET /v1/teams":                  paymentsTeam,
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
		"GET /v1/orgs": oneOrg,
		"GET /v1/models": map[string]any{"data": []map[string]any{{
			"alias": "keera-frontier", "kind": "chat",
			"backends":      []string{"https://api.openai.com/v1"},
			"backend_model": "gpt-5.1", "max_context": 400_000,
			"input_micros_per_mtok": 1_250_000, "output_micros_per_mtok": 10_000_000,
			"cached_input_micros_per_mtok": 125_000,
			"location":                     "usa", "enabled": true,
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
		"GET /v1/orgs": oneOrg,
		"GET /v1/models": map[string]any{"data": []map[string]any{{
			"alias": "keera-frontier", "kind": "chat",
			"backends":      []string{"https://api.openai.com/v1"},
			"backend_model": "gpt-5.1", "max_context": 400_000,
			"input_micros_per_mtok": 1_250_000, "output_micros_per_mtok": 10_000_000,
			"cached_input_micros_per_mtok": 125_000,
			"location":                     "usa", "enabled": true,
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

// An operator's model goes to an organisation: the only one when --org is
// left out. A change to an existing model goes to the organisation it
// belongs to.
func TestAnOperatorsModelGoesToTheOnlyOrganisation(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/me":   map[string]any{"role": "operator"},
		"GET /v1/orgs": map[string]any{"data": []map[string]any{{"id": "org_1", "name": "Bank"}}},
		"GET /v1/models": map[string]any{"data": []map[string]any{{
			"alias": "keera-frontier", "kind": "chat", "backend_model": "gpt-5.1",
			"backends": []string{"https://api.openai.com/v1"}, "location": "usa",
			"enabled": true, "org_id": "org_1",
		}}},
		"PUT /v1/models/mine":           map[string]any{"alias": "mine"},
		"PUT /v1/models/keera-frontier": map[string]any{"alias": "keera-frontier"},
	})

	if err := modelCmd(context.Background(), []string{"add", "mine",
		"--backend", "http://vllm:8000/v1", "--backend-model", "qwen"}); err != nil {
		t.Fatal(err)
	}
	if got := f.request("PUT", "/v1/models/mine").query; got != "org_id=org_1" {
		t.Errorf("the new model went to %q, want org_id=org_1", got)
	}
	if got := f.request("GET", "/v1/models").query; got != "org_id=org_1" {
		t.Errorf("the models were listed for %q, want org_id=org_1", got)
	}

	if err := modelCmd(context.Background(), []string{"set", "keera-frontier",
		"--api-key", "sk-test"}); err != nil {
		t.Fatal(err)
	}
	if got := f.request("PUT", "/v1/models/keera-frontier").query; got != "org_id=org_1" {
		t.Errorf("the key went to %q, want org_id=org_1", got)
	}
}

// Someone in one organisation never names it: a guardrail on "org" is theirs.
func TestAnOrganisationsGuardrailNeedsNoID(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/orgs":                 oneOrg,
		"GET /v1/guardrails/org/org_1": map[string]any{},
		"PUT /v1/guardrails/org/org_1": map[string]any{"allowed_models": []string{"keera-speed"}},
	})

	if err := guardrailCmd(context.Background(),
		[]string{"set", "org", "--models", "keera-speed"}); err != nil {
		t.Fatal(err)
	}
	if !f.called("PUT", "/v1/guardrails/org/org_1") {
		t.Errorf("the guardrail was not written to the caller's organisation: %v", f.seen)
	}
	// A team or a key still has to be named.
	if err := guardrailCmd(context.Background(), []string{"get", "team"}); err == nil {
		t.Error("a team guardrail was read without naming the team")
	}
}

// With several organisations, --org names one, whichever verb it follows.
func TestModelOrgFlagPicksOneOfSeveralOrganisations(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/me": map[string]any{"role": "operator"},
		"GET /v1/orgs": map[string]any{"data": []map[string]any{
			{"id": "org_a", "name": "Bank"}, {"id": "org_b", "name": "Insurer"},
		}},
		"GET /v1/models": map[string]any{"data": []map[string]any{}},
	})

	if err := modelCmd(context.Background(), []string{"list", "--org", "org_b"}); err != nil {
		t.Fatal(err)
	}
	if got := f.request("GET", "/v1/models").query; got != "org_id=org_b" {
		t.Errorf("the models were listed for %q, want org_id=org_b", got)
	}
}

// A flag that belongs to another verb is refused, not silently dropped.
func TestModelRefusesAnotherVerbsFlag(t *testing.T) {
	quiet(t)
	newFakeControl(t, map[string]any{})

	err := modelCmd(context.Background(), []string{"enable", "keera-speed", "--provider", "openai"})
	if err == nil || !strings.Contains(err.Error(), "does not take --provider") {
		t.Fatalf("err = %v, want --provider refused", err)
	}
}

// Deleting a model does not stop the filters and routers that name it, so the
// confirmation lists them.
func TestModelUsersNamesTheFiltersAndRoutersOnIt(t *testing.T) {
	quiet(t)
	newFakeControl(t, map[string]any{
		"GET /v1/filters": map[string]any{"data": []map[string]any{
			{"alias": "redact", "model": "keera-guard"},
			{"alias": "other", "model": "keera-speed"},
		}},
		"GET /v1/routers": map[string]any{"data": []map[string]any{
			{"alias": "auto", "model": "keera-speed", "destinations": []string{"keera-guard", "big"}},
			{"alias": "decide", "model": "keera-guard", "destinations": []string{"big"}},
			{"alias": "elsewhere", "model": "keera-speed", "destinations": []string{"big"}},
		}},
	})

	filters, routers, err := modelUsers(context.Background(), newClient(), "org_1", "keera-guard")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(filters, ",") != "redact" {
		t.Errorf("filters = %v, want [redact]", filters)
	}
	if strings.Join(routers, ",") != "auto,decide" {
		t.Errorf("routers = %v, want [auto decide]", routers)
	}
}

// An administrator's models are their organisation's, which is the only one
// they can see: nothing asks them for --org.
func TestAnAdministratorsModelNeedsNoOrg(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/orgs":        oneOrg,
		"GET /v1/models":      map[string]any{"data": []map[string]any{}},
		"PUT /v1/models/mine": map[string]any{"alias": "mine", "org_id": "org_1"},
	})

	if err := modelCmd(context.Background(), []string{"add", "mine",
		"--backend", "http://vllm:8000/v1", "--backend-model", "qwen"}); err != nil {
		t.Fatal(err)
	}
	if got := f.request("PUT", "/v1/models/mine").query; got != "org_id=org_1" {
		t.Errorf("the model was sent with %q, want the only organisation", got)
	}
}

func TestMicrosRoundsRatherThanTruncates(t *testing.T) {
	// 2.01 * 1e6 is 2009999.9999999998 as a float; the panel rounds, so the
	// command must too or the two would store different budgets.
	for units, want := range map[float64]int64{2.01: 2_010_000, 0.29: 290_000, 15: 15_000_000, 0: 0} {
		if got := micros(units); got != want {
			t.Errorf("micros(%v) = %d, want %d", units, got, want)
		}
	}
}

func TestOrgAndGuardrailRefuseAnotherVerbsFlag(t *testing.T) {
	// Each of these used to go ahead and quietly drop the flag.
	quiet(t)
	f := newFakeControl(t, map[string]any{"GET /v1/orgs": oneOrg})

	for _, args := range [][]string{
		{"org", "list", "--domain", "example.ch"},
		{"org", "create", "Another Bank", "--no-domain"},
		{"org", "delete", "org_1", "--domain", "example.ch", "--yes"},
		{"guardrail", "get", "team", "team_1", "--rpm", "5"},
	} {
		err := Run(context.Background(), args)
		if err == nil || !strings.Contains(err.Error(), "does not take") {
			t.Errorf("%v: err = %v, want a refusal of the flag", args, err)
		}
	}
	if len(f.seen) != 0 {
		t.Errorf("a refused command still called the control plane: %+v", f.seen)
	}
}

func TestReportsTakeATeamByNameAndAPersonByEmail(t *testing.T) {
	// The same names 'keera team rename' and 'keera key create --user' take.
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/orgs": oneOrg,
		"GET /v1/teams": map[string]any{"data": []map[string]any{
			{"id": "team_1", "org_id": "org_1", "name": "Payments Platform"},
		}},
		"GET /v1/users": map[string]any{"data": []map[string]any{
			{"id": "user_1", "org_id": "org_1", "email": "ada@example.ch"},
		}},
		"GET /v1/requests": map[string]any{"data": []any{}},
	})

	if err := Run(context.Background(), []string{"failures",
		"--team", "payments platform", "--user", "Ada@example.ch"}); err != nil {
		t.Fatal(err)
	}
	q := f.request("GET", "/v1/requests").query
	if !strings.Contains(q, "team_id=team_1") || !strings.Contains(q, "user_id=user_1") {
		t.Errorf("query = %q, want the team's and the person's ids", q)
	}
}

func TestANegativeBudgetFlagIsLeftUnset(t *testing.T) {
	// Negative is how the flag says it was not given. 'keera budget' and
	// 'guardrail set' share the flags, so they read it the same way.
	for _, amount := range []float64{-1, -5} {
		var lim policy.Limits
		f := registerGuardrailFlags(flag.NewFlagSet("budget", flag.ContinueOnError))
		f.budget = amount
		if err := f.apply(&lim); err != nil {
			t.Fatal(err)
		}
		if lim.BudgetMicros != nil {
			t.Errorf("--budget %v set a budget of %d", amount, *lim.BudgetMicros)
		}
	}
}

// A cut never splits a character in two.
func TestTruncatingKeepsWholeCharacters(t *testing.T) {
	long := strings.Repeat("ä", 120)
	for name, got := range map[string]string{"firstLine": firstLine(long), "oneLine": oneLine(long)} {
		if !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
			t.Errorf("%s cut %q badly", name, got)
		}
	}
}

// A guardrail names its team the way every other command does: by name.
func TestGuardrailTakesATeamByName(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/orgs":                   oneOrg,
		"GET /v1/teams":                  paymentsTeam,
		"GET /v1/guardrails/team/team_1": map[string]any{},
		"PUT /v1/guardrails/team/team_1": map[string]any{},

		"GET /v1/guardrails/team/team_1/effective": map[string]any{},
	})
	if err := Run(context.Background(), []string{"limit", "team", "payments", "--rpm", "60"}); err != nil {
		t.Fatal(err)
	}
	if got := f.request("PUT", "/v1/guardrails/team/team_1").body["rpm"]; got != float64(60) {
		t.Errorf("rpm = %v, want 60 on the team named payments", got)
	}
}

// An unknown verb is said before anything is sent: no sign-in is needed to
// be told about a typo.
func TestAnUnknownVerbIsRefusedBeforeAnyCall(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{})
	for _, args := range [][]string{
		{"model", "lst"}, {"filter", "lst"}, {"router", "lst"}, {"mcp", "lst"},
		{"usage", "--by", "team", "extra"}, {"mcp", "connect", "github", "--json"},
	} {
		if err := Run(context.Background(), args); err == nil {
			t.Errorf("keera %s was accepted", strings.Join(args, " "))
		}
	}
	if len(f.seen) != 0 {
		t.Errorf("a refused command still called the control plane: %+v", f.seen)
	}
}

// A filter or router that does not exist is said before the prompt, not after
// somebody has typed its alias back.
func TestDeleteLooksTheAliasUpBeforeAsking(t *testing.T) {
	quiet(t)
	newFakeControl(t, map[string]any{
		"GET /v1/orgs":    oneOrg,
		"GET /v1/filters": map[string]any{"data": []any{}},
		"GET /v1/routers": map[string]any{"data": []any{}},
	})
	for _, cmd := range []string{"filter", "router"} {
		err := Run(context.Background(), []string{cmd, "delete", "typo"})
		if err == nil || !strings.Contains(err.Error(), "no "+cmd+" typo") {
			t.Errorf("%s delete typo: err = %v, want the alias refused", cmd, err)
		}
	}
}

func TestKeyCreateTakesTheAliasOnce(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{})
	err := Run(context.Background(), []string{"key", "create", "laptop", "--alias", "phone"})
	if err == nil || !strings.Contains(err.Error(), "alias once") {
		t.Errorf("err = %v, want the two aliases refused", err)
	}
	if len(f.seen) != 0 {
		t.Errorf("a refused command still called the control plane: %+v", f.seen)
	}
}

func TestRotatingARevokedKeyWithNoTeamSuggestsNoTeam(t *testing.T) {
	quiet(t)
	newFakeControl(t, map[string]any{
		"GET /v1/keys": map[string]any{"data": []map[string]any{
			{"id": "key_1", "alias": "ci", "revoked_at": "2026-01-01T00:00:00Z"},
		}},
	})
	err := rotateKey(context.Background(), newClient(), "org_1", "key_1", "", "", false)
	if err == nil || strings.Contains(err.Error(), "--team") {
		t.Errorf("err = %v, want a hint without --team", err)
	}
}

func TestMaxSizeShowsEitherPartAlone(t *testing.T) {
	for _, tc := range []struct {
		cpu, memory int
		want        string
	}{
		{2000, 0, "2c, any memory"},
		{0, 8192, "any CPU, 8Gi"},
		{1500, 4096, "1.5c, 4Gi"},
	} {
		if got := maxSize(tc.cpu, tc.memory); got != tc.want {
			t.Errorf("maxSize(%d, %d) = %q, want %q", tc.cpu, tc.memory, got, tc.want)
		}
	}
	var w strings.Builder
	tw := newTable(&w)
	memory := 8192
	printLimits(tw, policy.Limits{MaxSandboxMemory: &memory})
	_ = tw.Flush()
	if !strings.Contains(w.String(), "any CPU, 8Gi") {
		t.Errorf("a memory-only limit was not shown:\n%s", w.String())
	}
}
