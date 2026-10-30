package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bespinian/keera-gateway/internal/catalog"
	"github.com/bespinian/keera-gateway/internal/control"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/id"
	"github.com/bespinian/keera-gateway/internal/policy"
)

type recorded struct {
	method string
	path   string
	// For a report the query is the whole behaviour.
	query string
	body  map[string]any
}

// fakeControl stands in for the control API. Handlers are keyed by
// "METHOD /path".
type fakeControl struct {
	t        *testing.T
	seen     []recorded
	handlers map[string]any
}

func newFakeControl(t *testing.T, handlers map[string]any) *fakeControl {
	t.Helper()
	// An administrator, so no default is chosen unless a test says otherwise.
	if _, set := handlers["GET /v1/me"]; !set {
		handlers["GET /v1/me"] = map[string]any{"role": "admin", "org_id": "org_1"}
	}
	// One organisation, so --org fills itself in.
	if _, set := handlers["GET /v1/orgs"]; !set {
		handlers["GET /v1/orgs"] = oneOrg
	}
	f := &fakeControl{t: t, handlers: handlers}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("KEERA_CONTROL_URL", srv.URL)
	t.Setenv("KEERA_OPERATOR_KEY", "test-operator-key")
	t.Setenv("KEERA_ORG", "")
	return f
}

// failure answers with an error envelope.
type failure struct {
	status  int
	message string
}

func (f *fakeControl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	// Handlers are keyed without httpx.ControlPrefix. A call without the
	// prefix would 404 in production, so it fails to match.
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

func (f *fakeControl) called(method, path string) bool {
	for _, r := range f.seen {
		if r.method == method && r.path == path {
			return true
		}
	}
	return false
}

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

// quiet swallows stdout and stderr. Calling either returned function puts the
// streams back and returns what that one was sent.
func quiet(t *testing.T) (stdout, stderr func() string) {
	t.Helper()
	// Read as it fills, so a long help text cannot block.
	pipe := func() (*os.File, <-chan string) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		said := make(chan string, 1)
		go func() {
			raw, _ := io.ReadAll(r)
			_ = r.Close()
			said <- string(raw)
		}()
		return w, said
	}
	outW, outC := pipe()
	errW, errC := pipe()
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	var out, errs string
	restore := sync.OnceFunc(func() {
		os.Stdout, os.Stderr = oldOut, oldErr
		_ = outW.Close()
		_ = errW.Close()
		out, errs = <-outC, <-errC
	})
	t.Cleanup(restore)
	return func() string { restore(); return out }, func() string { restore(); return errs }
}

// capture runs one invocation and returns its stdout.
func capture(t *testing.T, args ...string) string {
	t.Helper()
	stdout, _ := quiet(t)
	if err := Run(context.Background(), args); err != nil {
		t.Fatalf("keera %s: %v", strings.Join(args, " "), err)
	}
	return stdout()
}

// oneOrg lets resolveOrg fill in --org by itself.
var oneOrg = map[string]any{"data": []map[string]any{{"id": "org_1", "name": "Example Bank"}}}

var paymentsProject = map[string]any{"data": []map[string]any{
	{"id": "project_1", "org_id": "org_1", "name": "Payments"},
}}

func TestOrgCreateSendsTheDomain(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"POST /v1/orgs": map[string]any{
			"id": "org_2", "name": "Another Bank", "email_domain": "anotherbank.ch",
		},
	})

	if err := Run(context.Background(), []string{
		"org", "create", "Another Bank",
		"--domain", "AnotherBank.CH",
	}); err != nil {
		t.Fatal(err)
	}
	// Lowercased, as sign-ins are compared.
	if got := f.request("POST", "/v1/orgs").body["email_domain"]; got != "anotherbank.ch" {
		t.Errorf("email_domain sent = %v, want anotherbank.ch", got)
	}
}

func TestOrgSetPatchesTheDomain(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"PATCH /v1/orgs/org_2": map[string]any{"id": "org_2", "email_domain": "anotherbank.ch"},
	})

	if err := Run(context.Background(), []string{
		"org", "set", "org_2",
		"--domain", "anotherbank.ch",
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.request("PATCH", "/v1/orgs/org_2").body["email_domain"]; got != "anotherbank.ch" {
		t.Errorf("email_domain sent = %v, want anotherbank.ch", got)
	}
}

func TestOrgSetRenamesWithoutTouchingTheDomain(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"PATCH /v1/orgs/org_2": map[string]any{"id": "org_2", "name": "Another Bank AG"},
	})

	if err := Run(context.Background(), []string{
		"org", "set", "org_2",
		"--name", " Another Bank AG ",
	}); err != nil {
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

// The id can be left off where there is only one organisation, which is when
// a domain has to be set.
func TestOrgSetFindsTheOnlyOrganisation(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
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
	if err == nil || !strings.Contains(err.Error(), "keera org set <org>") {
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
	// Sent as "", since the endpoint keeps an absent field.
	if got, present := f.request("PATCH", "/v1/orgs/org_2").body["email_domain"]; !present || got != "" {
		t.Errorf("email_domain = %v (present %v), want an empty string", got, present)
	}
}

// A bad domain matches no sign-in, which nobody would notice for months.
func TestOrgSetRefusesSomethingThatIsNotADomain(t *testing.T) {
	quiet(t)
	newFakeControl(t, map[string]any{})

	// The refusal says which domain to type instead.
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
	// A typo has nothing to suggest, so it names what is wrong.
	err := Run(context.Background(), []string{"org", "set", "org_2", "--domain", "exam ple.ch"})
	if err == nil || !strings.Contains(err.Error(), "cannot appear") {
		t.Errorf("error = %v, want it to name the character", err)
	}
}

// Creating a second organisation means sign-ins to the first are placed by
// domain or refused.
func TestOrgCreateWarnsAboutAnOrganisationWithNoDomain(t *testing.T) {
	_, stderr := quiet(t)
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
	_, stderr := quiet(t)
	newFakeControl(t, map[string]any{
		"POST /v1/orgs": map[string]any{"id": "org_1", "name": "Example Bank"},
	})

	// One organisation needs no domain: everyone lands in it.
	if err := Run(context.Background(), []string{"org", "create", "Example Bank"}); err != nil {
		t.Fatal(err)
	}
	if said := stderr(); said != "" {
		t.Errorf("said %q, want nothing", said)
	}
}

func TestUserRoleResolvesAnEmailToAnID(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/users": map[string]any{"data": []map[string]any{
			{"id": "user_1", "org_id": "org_1", "email": "Alice@Example.com", "role": "member"},
		}},
		"PATCH /v1/users/user_1": map[string]any{"id": "user_1", "role": "admin"},
	})

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

func TestUserAddRefusesSomebodyWhoIsAlreadyThere(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/users": map[string]any{"data": []map[string]any{
			{"id": "user_1", "org_id": "org_1", "email": "alice@example.com", "role": "member"},
		}},
	})

	// Upserting would silently change their role and keep their sessions.
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

// No report can attribute the spend of a key that names nobody.
func TestKeyCreateAttributesAPersonByEmail(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/users": map[string]any{"data": []map[string]any{
			{"id": "user_1", "org_id": "org_1", "email": "alice@example.com", "role": "member"},
		}},
		"POST /v1/keys": map[string]any{"id": "key_1", "key": "sk-keera-secret"},
	})

	if err := keyCmd(context.Background(), []string{"create", "--user", "alice@example.com", "--name", "laptop"}); err != nil {
		t.Fatal(err)
	}
	if got := f.request("POST", "/v1/keys").body["user_id"]; got != "user_1" {
		t.Errorf("user_id sent = %v, want user_1", got)
	}
}

// liveKey has a project, a person, 90 days and its own guardrails.
var liveKey = map[string]any{
	"data": []map[string]any{{
		"id": "key_old", "org_id": "org_1", "project_id": "project_1", "user_id": "user_1",
		"name": "a developer's laptop", "prefix": "keera_sk_ab",
		"created_at": "2026-01-01T00:00:00Z", "expires_at": "2026-04-01T00:00:00Z",
		"limits": map[string]any{"requests": 120, "allowed_models": []string{"keera-speed"}},
	}},
}

// The control plane copies everything else; the CLI only names the key and
// passes on what was asked to change.
func TestKeyRotateAsksTheControlPlane(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/keys": liveKey,
		"POST /v1/keys/key_old/rotate": map[string]any{
			"id": "key_new", "key": "keera_sk_new", "replaced_key_id": "key_old",
		},
	})

	if err := keyCmd(context.Background(),
		[]string{"rotate", "a developer's laptop", "--name", "new laptop", "--yes"}); err != nil {
		t.Fatal(err)
	}
	sent := f.request("POST", "/v1/keys/key_old/rotate").body
	if _, set := sent["expires_in_seconds"]; sent["name"] != "new laptop" || set {
		t.Errorf("sent %v, want the new name and no expiry", sent)
	}
	if f.called("POST", "/v1/keys") || f.called("DELETE", "/v1/keys/key_old") {
		t.Errorf("the CLI rotated by hand: %v", f.seen)
	}
}

func TestKeySetRenamesTheKeyItFinds(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/keys":           liveKey,
		"PATCH /v1/keys/key_old": map[string]any{"id": "key_old", "name": "old laptop"},
	})

	if err := keyCmd(context.Background(),
		[]string{"rename", "a developer's laptop", "--name", " old laptop "}); err != nil {
		t.Fatal(err)
	}
	if sent := f.request("PATCH", "/v1/keys/key_old").body; sent["name"] != "old laptop" {
		t.Errorf("sent %v, want the new name", sent)
	}
	if err := keyCmd(context.Background(), []string{"set", "key_old"}); err == nil ||
		!strings.Contains(err.Error(), "nothing to change") {
		t.Errorf("err = %v, want nothing to change", err)
	}
}

func TestKeyRotateRefusesAKeyThatIsAlreadyRevoked(t *testing.T) {
	quiet(t)
	newFakeControl(t, map[string]any{
		"GET /v1/keys": map[string]any{"data": []map[string]any{{
			"id": "key_old", "org_id": "org_1", "name": "laptop",
			"created_at": "2026-01-01T00:00:00Z", "revoked_at": "2026-02-01T00:00:00Z",
		}}},
	})

	err := keyCmd(context.Background(), []string{"rotate", "key_old"})
	if err == nil || !strings.Contains(err.Error(), "keera key create") {
		t.Errorf("error = %v, want one pointing at keera key create", err)
	}
}

// A member is not pointed at a command that would refuse them.
func TestKeyRotateSendsAMemberWithARevokedKeyToAnAdministrator(t *testing.T) {
	quiet(t)
	revoked := []map[string]any{{
		"id": "key_old", "org_id": "org_1", "user_id": "user_1", "name": "laptop",
		"created_at": "2026-01-01T00:00:00Z", "revoked_at": "2026-02-01T00:00:00Z",
	}}
	newFakeControl(t, map[string]any{
		"GET /v1/me":   map[string]any{"role": "member", "org_id": "org_1", "user_id": "user_1"},
		"GET /v1/keys": map[string]any{"data": revoked},
	})

	// By id, the key is found and then refused; by name, no live key is found.
	for _, who := range []string{"key_old", "laptop"} {
		err := keyCmd(context.Background(), []string{"rotate", who})
		if err == nil || !strings.Contains(err.Error(), "ask an administrator") ||
			strings.Contains(err.Error(), "keera key create") {
			t.Errorf("%s: error = %v, want one pointing at an administrator", who, err)
		}
	}
}

// keyVerbs act on one key, with the call each makes under /v1/keys/<id>.
var keyVerbs = []struct {
	verb, method, suffix string
	reply                map[string]any
}{
	{"rotate", "POST", "/rotate", map[string]any{"id": "key_new", "key": "keera_sk_new"}},
	{"revoke", "DELETE", "", map[string]any{}},
}

func TestKeyRotateAndRevokeResolveANameAmongTheKeysThatStillWork(t *testing.T) {
	for _, tc := range keyVerbs {
		t.Run(tc.verb, func(t *testing.T) {
			quiet(t)
			f := newFakeControl(t, map[string]any{
				// After one rotation: two keys with the same name, one revoked.
				"GET /v1/keys": map[string]any{"data": []map[string]any{
					{
						"id": "key_gone", "org_id": "org_1", "name": "laptop",
						"created_at": "2026-01-01T00:00:00Z", "revoked_at": "2026-02-01T00:00:00Z",
					},
					{"id": "key_live", "org_id": "org_1", "name": "laptop", "created_at": "2026-02-01T00:00:00Z"},
				}},
				tc.method + " /v1/keys/key_live" + tc.suffix: tc.reply,
			})

			if err := keyCmd(context.Background(), []string{tc.verb, "laptop", "--yes"}); err != nil {
				t.Fatal(err)
			}
			if !f.called(tc.method, "/v1/keys/key_live"+tc.suffix) {
				t.Errorf("%s did not pick the key that still works", tc.verb)
			}
		})
	}
}

func TestKeyRotateAndRevokeRefuseAnAmbiguousName(t *testing.T) {
	for _, tc := range keyVerbs {
		t.Run(tc.verb, func(t *testing.T) {
			quiet(t)
			f := newFakeControl(t, map[string]any{
				"GET /v1/keys": map[string]any{"data": []map[string]any{
					{"id": "key_a", "org_id": "org_1", "name": "laptop", "created_at": "2026-01-01T00:00:00Z"},
					{"id": "key_b", "org_id": "org_1", "name": "laptop", "created_at": "2026-02-01T00:00:00Z"},
				}},
			})

			err := keyCmd(context.Background(), []string{tc.verb, "laptop"})
			if err == nil {
				t.Fatalf("%s picked one of two keys with the same name", tc.verb)
			}
			for _, want := range []string{"key_a", "key_b"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error does not name %s to choose between: %v", want, err)
				}
				if f.called(tc.method, "/v1/keys/"+want+tc.suffix) {
					t.Errorf("%s acted on %s despite the name matching two", tc.verb, want)
				}
			}
		})
	}
}

// Revoking by id with --yes needs no lookup.
func TestKeyRevokeByIdLooksNothingUp(t *testing.T) {
	quiet(t)
	keyID := id.New("key")
	f := newFakeControl(t, map[string]any{
		"DELETE /v1/keys/" + keyID: map[string]any{},
	})

	if err := keyCmd(context.Background(), []string{"revoke", keyID, "--yes"}); err != nil {
		t.Fatal(err)
	}
	if len(f.seen) != 1 || !f.called("DELETE", "/v1/keys/"+keyID) {
		t.Errorf("calls = %v, want only the revocation", f.seen)
	}
}

// Closed stdin is a confirmation not given. The prompt goes to stderr, so
// `--json` output stays JSON.
func TestKeyRevokeAsksBeforeItRevokes(t *testing.T) {
	_, stderr := quiet(t)
	keyID := id.New("key")
	f := newFakeControl(t, map[string]any{
		"GET /v1/keys": map[string]any{"data": []map[string]any{
			{"id": keyID, "org_id": "org_1", "name": "laptop", "created_at": "2026-01-01T00:00:00Z"},
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
		!strings.Contains(said, "Type the name to confirm:") {
		t.Errorf("stderr = %q, want the prompt on it", said)
	}
}

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
	if body["input_micros_per_mtok"] != float64(4_800_000) {
		t.Errorf("input price = %v, want the provider's", body["input_micros_per_mtok"])
	}
	if body["max_context_tokens"] != float64(1_000_000) {
		t.Errorf("max_context_tokens = %v", body["max_context_tokens"])
	}
	if _, sent := body["api_key"]; sent {
		t.Error("a credential was sent when none was given")
	}
}

// The endpoint replaces the whole entry, so a field the CLI forgot to restate
// is gone.
func TestModelSetChangesWhatItIsGivenAndKeepsTheRest(t *testing.T) {
	stored := map[string]any{
		"alias": "keera-speed", "kind": "chat",
		"backends":      []string{"http://vllm-a:8000/v1", "http://vllm-b:8000/v1"},
		"backend_model": "qwen3-8b", "max_context_tokens": 32768,
		"input_micros_per_mtok": 100_000, "output_micros_per_mtok": 300_000,
		"cached_input_micros_per_mtok": 50_000,
		"enabled":                      true,
	}
	for _, tc := range []struct {
		flag, value, field string
		want               int
	}{
		{"--price-in", "1", "input_micros_per_mtok", 1_000_000},
		{"--price-out", "0.5", "output_micros_per_mtok", 500_000},
		{"--price-cached", "0.4", "cached_input_micros_per_mtok", 400_000},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			quiet(t)
			f := newFakeControl(t, map[string]any{
				"GET /v1/models":             map[string]any{"data": []map[string]any{stored}},
				"PUT /v1/models/keera-speed": map[string]any{"alias": "keera-speed"},
			})

			if err := modelCmd(context.Background(),
				[]string{"set", "keera-speed", tc.flag, tc.value}); err != nil {
				t.Fatal(err)
			}
			body := f.request("PUT", "/v1/models/keera-speed").body
			for _, field := range []string{
				"input_micros_per_mtok", "output_micros_per_mtok",
				"cached_input_micros_per_mtok", "max_context_tokens",
			} {
				want := stored[field].(int)
				if field == tc.field {
					want = tc.want
				}
				if body[field] != float64(want) {
					t.Errorf("%s = %v, want %d", field, body[field], want)
				}
			}
			if len(body["backends"].([]any)) != 2 {
				t.Errorf("backends = %v, want both left alone", body["backends"])
			}
			if body["backend_model"] != "qwen3-8b" || body["enabled"] != true {
				t.Errorf("set changed a field it was not given: %v", body)
			}
		})
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

// Setting a key sends the rest of the model unchanged.
func TestModelSetStoresACredentialAndKeepsTheRest(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
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
		"GET /v1/models": map[string]any{"data": []map[string]any{{"alias": "keera-speed"}}},
		"POST /v1/models/keera-speed/check": map[string]any{
			"alias": "keera-speed", "reachable": true, "status": 200,
			"tool_call_as_text": true, "ok": false,
		},
	})

	// A failed check fails the command.
	err := modelCmd(context.Background(), []string{"check", "keera-speed"})
	if err == nil || !strings.Contains(err.Error(), "did not pass") {
		t.Fatalf("error = %v, want a failed command", err)
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
	if got := f.request("PUT", "/v1/models/keera-speed").body["backend_model"]; got != "qwen3-8b" {
		t.Errorf("backend_model = %v, want the file's", got)
	}
	if got := f.request("PUT", "/v1/models/keera-frontier").body["input_micros_per_mtok"]; got != float64(4_800_000) {
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
	// 0 is a price: a model can be deliberately unbilled.
	if got.OutputMicrosPerMTok != 0 {
		t.Errorf("output price = %d, want 0", got.OutputMicrosPerMTok)
	}
	if got.InputMicrosPerMTok != 100_000 || got.MaxContext != 32768 || !got.Enabled {
		t.Errorf("an ungiven field changed: %+v", got)
	}
}

// The stored backends have the old product id written in, so a new one
// rebuilds the address.
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

// A stated backend wins, as every stated value wins over the provider's table.
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

// A new provider brings its own location.
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

// Otherwise requests would go to the wrong place at the wrong rate.
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

// Another model of the same provider has its own values; keeping the old
// model's would bill it wrong.
func TestBackendModelFlagBringsTheNewModelsValues(t *testing.T) {
	current := declared(policy.Model{
		Alias: "keera-coder", Kind: policy.KindChat, Provider: "stepping-stone",
		Backends: []string{"https://llm.stoney-cloud.com/v1"}, BackendModel: "Qwen/Qwen3-Coder-Next",
		DisplayName: "Qwen3 Coder Next", Description: "made for code", InputMicrosPerMTok: 340_000, OutputMicrosPerMTok: 1_700_000,
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
		got.CachedInputMicrosPerMTok != 0 || got.MaxContext != 128_000 ||
		got.ReleaseDate != "2025-08-05" || got.Description == "made for code" ||
		got.DisplayName != "gpt-oss-120b" {
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
		{policy.Model{}, "-"},
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
		"GET /v1/filters": map[string]any{"data": []any{}},
		"PUT /v1/filters/redact-secrets": map[string]any{
			"alias": "redact-secrets", "model": "keera-guard", "prompt": "Remove credentials.",
		},
	})

	if err := Run(context.Background(), []string{
		"filter", "add", "redact-secrets",
		"--model", "keera-guard", "--prompt", "Remove credentials.",
	}); err != nil {
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

// An empty --description clears it, and a set with nothing to change writes
// nothing.
func TestSetClearsADescriptionAndRefusesNothing(t *testing.T) {
	for _, kind := range []string{"filter", "router"} {
		t.Run(kind, func(t *testing.T) {
			quiet(t)
			f := newFakeControl(t, map[string]any{
				"GET /v1/" + kind + "s": map[string]any{"data": []map[string]any{{
					"alias": "a", "model": "keera-guard", "mode": "model",
					"prompt": "Pick one.", "description": "the original",
				}}},
				"PUT /v1/" + kind + "s/a": map[string]any{"alias": "a"},
			})
			if err := Run(context.Background(), []string{kind, "set", "a", "--description", ""}); err != nil {
				t.Fatalf("%s set: %v", kind, err)
			}
			if d := f.request("PUT", "/v1/"+kind+"s/a").body["description"]; d != "" {
				t.Errorf("description = %v, want it cleared", d)
			}
		})
	}

	// --step only picks the step the other flags change.
	for _, args := range [][]string{
		{"filter", "set", "a"},
		{"router", "set", "a", "--step", "route"},
	} {
		quiet(t)
		f := newFakeControl(t, map[string]any{})
		if err := Run(context.Background(), args); err == nil ||
			!strings.Contains(err.Error(), "nothing to change") {
			t.Errorf("%v = %v, want nothing to change", args, err)
		}
		if f.called("PUT", "/v1/"+args[0]+"s/a") {
			t.Errorf("%v wrote the %s back", args, args[0])
		}
	}
}

// A set naming neither shadow flag does not switch a shadow filter to
// enforcing, and --no-shadow changes nothing else.
func TestFilterAndRouterSetKeepTheRest(t *testing.T) {
	filter := map[string]any{
		"alias": "redact-secrets", "model": "keera-guard",
		"prompt": "Remove credentials.", "description": "the original", "shadow": true,
	}
	changes := []struct {
		flags []string
		want  map[string]any
	}{
		{[]string{"--model", "keera-guard-2"}, map[string]any{"model": "keera-guard-2"}},
		{[]string{"--description", "changed"}, map[string]any{"description": "changed"}},
		{[]string{"--no-shadow"}, map[string]any{"shadow": false}},
	}
	for _, verb := range []string{"set", "edit", "update"} {
		for _, c := range changes {
			t.Run("filter "+verb+" "+c.flags[0], func(t *testing.T) {
				quiet(t)
				f := newFakeControl(t, map[string]any{
					"GET /v1/filters":                map[string]any{"data": []map[string]any{filter}},
					"PUT /v1/filters/redact-secrets": map[string]any{"alias": "redact-secrets"},
				})
				args := append([]string{"filter", verb, "redact-secrets"}, c.flags...)
				if err := Run(context.Background(), args); err != nil {
					t.Fatal(err)
				}
				body := f.request("PUT", "/v1/filters/redact-secrets").body
				for _, field := range []string{"model", "prompt", "description", "shadow"} {
					want, changed := c.want[field]
					if !changed {
						want = filter[field]
					}
					if body[field] != want {
						t.Errorf("%s = %v, want %v", field, body[field], want)
					}
				}
			})
		}

		t.Run("router "+verb, func(t *testing.T) {
			quiet(t)
			f := newFakeControl(t, map[string]any{
				"GET /v1/routers": map[string]any{"data": []map[string]any{{
					"alias": "auto", "description": "the original", "display_name": "Auto",
					"start": map[string]any{
						"id": "route", "kind": "route", "model": "keera-speed",
						"destinations": []string{"a", "b"}, "prompt": "Pick one.",
					},
				}}},
				"PUT /v1/routers/auto": map[string]any{"alias": "auto"},
			})
			if err := Run(context.Background(), []string{
				"router", verb, "auto",
				"--description", "changed",
			}); err != nil {
				t.Fatal(err)
			}
			req := f.request("PUT", "/v1/routers/auto")
			start, _ := req.body["start"].(map[string]any)
			if start["prompt"] != "Pick one." || start["model"] != "keera-speed" ||
				req.body["display_name"] != "Auto" || req.body["description"] != "changed" {
				t.Errorf("router %s dropped what was not given: %v", verb, req.body)
			}
		})
	}
}

// A new filter enforces, as in the panel, unless --shadow is given.
func TestFilterAddEnforcesUnlessShadow(t *testing.T) {
	for _, tc := range []struct {
		flags []string
		want  bool
	}{
		{nil, false},
		{[]string{"--shadow"}, true},
	} {
		quiet(t)
		f := newFakeControl(t, map[string]any{
			"GET /v1/filters":                map[string]any{"data": []any{}},
			"PUT /v1/filters/redact-secrets": map[string]any{"alias": "redact-secrets"},
		})

		args := append([]string{
			"filter", "add", "redact-secrets",
			"--model", "keera-guard", "--prompt", "Remove credentials.",
		}, tc.flags...)
		if err := Run(context.Background(), args); err != nil {
			t.Fatalf("filter add %v: %v", tc.flags, err)
		}
		if req := f.request("PUT", "/v1/filters/redact-secrets"); req.body["shadow"] != tc.want {
			t.Errorf("filter add %v: shadow = %v, want %v", tc.flags, req.body["shadow"], tc.want)
		}
	}
}

func TestFilterReportAsksForTheWindowAndPrintsTheSplit(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/filters/redact-secrets/report": map[string]any{
			"filter":        map[string]any{"alias": "redact-secrets", "model": "keera-guard"},
			"project_names": map[string]any{"project_1": "Payments Platform"},
			"report": map[string]any{
				"filter": "redact-secrets", "runs": 400, "pass": 300, "rewrote": 80,
				"refused": 16, "errors": 4, "requests": 1000,
				"cost_micros": 2_000_000, "org_cost_micros": 40_000_000,
				"latency_median_ms": 210, "latency_p95_ms": 900,
				"segments": 1600, "changed": 120,
				"projects": []map[string]any{
					{
						"project_id": "project_1", "runs": 100, "refused": 16, "rewrote": 20,
						"cost_micros": 500_000,
					},
				},
			},
		},
	})

	out := capture(t, "filter", "report", "redact-secrets", "--since", "168h")

	req := f.request("GET", "/v1/filters/redact-secrets/report")
	if !strings.Contains(req.query, "from=") {
		t.Errorf("query = %q, want the window it was asked for", req.query)
	}
	for _, want := range []string{
		"400 of the organisation's 1000 requests (40.0%)", // is it firing at all
		"4.0%",                    // and how much of that was a refusal
		"210ms median, 900ms p95", // what the request waiting for it paid
		"Payments Platform",       // and who is living with it
		"16.0%",                   // the project's own rate, not the org's
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report did not carry %q:\n%s", want, out)
		}
	}
}

func TestPolicySetAppliesFiltersAndClearsThemByFlag(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/projects": paymentsProject,
		"GET /v1/guardrails/project/project_1": map[string]any{"rate_limits": []any{
			map[string]any{"requests": 120},
		}},
		"PUT /v1/guardrails/project/project_1": map[string]any{},
	})

	if err := Run(context.Background(), []string{
		"guardrail", "set", "project", "project_1",
		"--filters", "redact-secrets,redact-clients",
	}); err != nil {
		t.Fatalf("guardrail set: %v", err)
	}
	req := f.request("PUT", "/v1/guardrails/project/project_1")
	got, _ := req.body["filters"].([]any)
	if len(got) != 2 || got[0] != "redact-secrets" || got[1] != "redact-clients" {
		t.Errorf("filters = %v, want both in the order given", req.body["filters"])
	}
	// The stored limits are read first, so naming a filter keeps the rate
	// limit.
	if got := rateLimits(req.body); len(got) != 1 || got[0]["requests"] != float64(120) {
		t.Errorf("rate_limits = %v; setting one field must not clear the others", got)
	}
}

func TestPolicySetClearsFiltersOnlyWithItsOwnFlag(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/projects":                     paymentsProject,
		"GET /v1/guardrails/project/project_1": map[string]any{"filters": []string{"redact-secrets"}},
		"PUT /v1/guardrails/project/project_1": map[string]any{},
	})

	if err := Run(context.Background(), []string{
		"guardrail", "set", "project", "project_1",
		"--no-filters",
	}); err != nil {
		t.Fatalf("guardrail set: %v", err)
	}
	if got, present := f.request("PUT", "/v1/guardrails/project/project_1").body["filters"]; present {
		t.Errorf("filters = %v, want them gone", got)
	}
}

// Without --org an operator's model goes to the only organisation. A change
// goes to the model's own organisation.
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

	if err := modelCmd(context.Background(), []string{
		"add", "mine",
		"--backend", "http://vllm:8000/v1", "--backend-model", "qwen",
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.request("PUT", "/v1/models/mine").query; got != "org_id=org_1" {
		t.Errorf("the new model went to %q, want org_id=org_1", got)
	}
	if got := f.request("GET", "/v1/models").query; got != "org_id=org_1" {
		t.Errorf("the models were listed for %q, want org_id=org_1", got)
	}

	if err := modelCmd(context.Background(), []string{
		"set", "keera-frontier",
		"--api-key", "sk-test",
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.request("PUT", "/v1/models/keera-frontier").query; got != "org_id=org_1" {
		t.Errorf("the key went to %q, want org_id=org_1", got)
	}
}

// A guardrail on "org" needs no id for someone in one organisation.
func TestAnOrganisationsGuardrailNeedsNoID(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
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
	// A project or a key still has to be named.
	if err := guardrailCmd(context.Background(), []string{"get", "project"}); err == nil {
		t.Error("a project guardrail was read without naming the project")
	}
}

func TestModelListUsesTheChosenOrganisation(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/me": map[string]any{"role": "operator"},
		"GET /v1/orgs": map[string]any{"data": []map[string]any{
			{"id": "org_a", "name": "Bank"}, {"id": "org_b", "name": "Insurer"},
		}},
		"GET /v1/models": map[string]any{"data": []map[string]any{}},
	})

	t.Setenv("KEERA_ORG", "org_b")
	if err := modelCmd(context.Background(), []string{"list"}); err != nil {
		t.Fatal(err)
	}
	if got := f.request("GET", "/v1/models").query; got != "org_id=org_b" {
		t.Errorf("the models were listed for %q, want org_id=org_b", got)
	}
}

func TestModelUsersNamesTheFiltersAndRoutersOnIt(t *testing.T) {
	quiet(t)
	newFakeControl(t, map[string]any{
		"GET /v1/filters": map[string]any{"data": []map[string]any{
			{"alias": "redact", "model": "keera-guard"},
			{"alias": "other", "model": "keera-speed"},
		}},
		"GET /v1/routers": map[string]any{"data": []map[string]any{
			{"alias": "auto", "start": map[string]any{
				"id": "r", "kind": "route",
				"model": "keera-speed", "destinations": []string{"keera-guard", "big"},
			}},
			{"alias": "decide", "start": map[string]any{
				"id": "r", "kind": "route",
				"model": "keera-guard", "destinations": []string{"big"},
			}},
			{"alias": "direct", "start": map[string]any{"id": "m", "kind": "model", "ref": "keera-guard"}},
			{"alias": "elsewhere", "start": map[string]any{
				"id": "r", "kind": "route",
				"model": "keera-speed", "destinations": []string{"big"},
			}},
		}},
	})

	filters, routers, err := modelUsers(context.Background(), newClient(), "org_1", "keera-guard")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(filters, ",") != "redact" {
		t.Errorf("filters = %v, want [redact]", filters)
	}
	if strings.Join(routers, ",") != "auto,decide,direct" {
		t.Errorf("routers = %v, want [auto decide direct]", routers)
	}
}

// An administrator sees only their organisation, so nothing asks for --org.
func TestAnAdministratorsModelNeedsNoOrg(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/models":      map[string]any{"data": []map[string]any{}},
		"PUT /v1/models/mine": map[string]any{"alias": "mine", "org_id": "org_1"},
	})

	if err := modelCmd(context.Background(), []string{
		"add", "mine",
		"--backend", "http://vllm:8000/v1", "--backend-model", "qwen",
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.request("PUT", "/v1/models/mine").query; got != "org_id=org_1" {
		t.Errorf("the model was sent with %q, want the only organisation", got)
	}
}

func TestMicrosRoundsRatherThanTruncates(t *testing.T) {
	// 2.01 * 1e6 is 2009999.9999999998 as a float; the panel rounds, so the
	// command must too.
	for units, want := range map[float64]int64{2.01: 2_010_000, 0.29: 290_000, 15: 15_000_000, 0: 0} {
		if got := micros(units); got != want {
			t.Errorf("micros(%v) = %d, want %d", units, got, want)
		}
	}
}

func TestReportsTakeAProjectByNameAndAPersonByEmail(t *testing.T) {
	// The same names 'keera project set' and 'keera key create --user' take.
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/projects": map[string]any{"data": []map[string]any{
			{"id": "project_1", "org_id": "org_1", "name": "Payments Platform"},
		}},
		"GET /v1/users": map[string]any{"data": []map[string]any{
			{"id": "user_1", "org_id": "org_1", "email": "ada@example.ch"},
		}},
		"GET /v1/requests": map[string]any{"data": []any{}},
	})

	if err := Run(context.Background(), []string{
		"failures",
		"--project", "payments platform", "--user", "Ada@example.ch",
	}); err != nil {
		t.Fatal(err)
	}
	q := f.request("GET", "/v1/requests").query
	if !strings.Contains(q, "project_id=project_1") || !strings.Contains(q, "user_id=user_1") {
		t.Errorf("query = %q, want the project's and the person's ids", q)
	}
}

// --status takes one status or a class. Anything else would filter nothing.
func TestFailuresTakeAStatusOrAClass(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/requests": map[string]any{"data": []any{}},
	})
	for _, status := range []string{"502", "5xx"} {
		if err := Run(context.Background(), []string{"failures", "--status", status}); err != nil {
			t.Fatalf("--status %s: %v", status, err)
		}
		if q := f.seen[len(f.seen)-1].query; !strings.Contains(q, "status="+status) {
			t.Errorf("query = %q, want status=%s", q, status)
		}
	}
	if err := Run(context.Background(), []string{"failures", "--status", "bad"}); err == nil {
		t.Error("--status bad was sent, and would have filtered nothing")
	}
}

func TestANegativeBudgetFlagIsLeftUnset(t *testing.T) {
	// Negative means not given, in 'keera budget' and 'guardrail set' alike.
	for _, amount := range []float64{-1, -5} {
		var lim policy.Limits
		f := registerGuardrailFlags(flag.NewFlagSet("budget", flag.ContinueOnError))
		f.budget = amount
		if err := f.apply(&lim); err != nil {
			t.Fatal(err)
		}
		if lim.Budgets != nil {
			t.Errorf("--budget %v set a budget: %v", amount, lim.Budgets)
		}
	}
}

func TestTruncatingKeepsWholeCharacters(t *testing.T) {
	long := strings.Repeat("ä", 120)
	for name, got := range map[string]string{"firstLine": firstLine(long), "oneLine": oneLine(long)} {
		if !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
			t.Errorf("%s cut %q badly", name, got)
		}
	}
}

func TestGuardrailTakesAProjectByName(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/projects":                     paymentsProject,
		"GET /v1/guardrails/project/project_1": map[string]any{},
		"PUT /v1/guardrails/project/project_1": map[string]any{},

		"GET /v1/guardrails/project/project_1/effective": map[string]any{},
	})
	if err := Run(context.Background(), []string{"limit", "project", "payments", "--requests", "60"}); err != nil {
		t.Fatal(err)
	}
	if got := rateLimits(f.request("PUT", "/v1/guardrails/project/project_1").body); len(got) != 1 ||
		got[0]["requests"] != float64(60) || got[0]["period"] != "minute" {
		t.Errorf("rate_limits = %v, want 60 requests a minute on the project named payments", got)
	}
}

func rateLimits(body map[string]any) []map[string]any {
	var out []map[string]any
	list, _ := body["rate_limits"].([]any)
	for _, v := range list {
		if m, ok := v.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func TestLimitCountsPerDayWithPer(t *testing.T) {
	// --per picks the rate limit to set; the one per minute stays.
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/projects": paymentsProject,
		"GET /v1/guardrails/project/project_1": map[string]any{"rate_limits": []any{
			map[string]any{"requests": 60, "period": "minute"},
		}},
		"PUT /v1/guardrails/project/project_1": map[string]any{},

		"GET /v1/guardrails/project/project_1/effective": map[string]any{},
	})
	if err := Run(context.Background(), []string{
		"limit", "project", "payments",
		"--tokens", "5000000", "--per", "day",
	}); err != nil {
		t.Fatal(err)
	}
	got := rateLimits(f.request("PUT", "/v1/guardrails/project/project_1").body)
	if len(got) != 2 || got[0]["requests"] != float64(60) ||
		got[1]["tokens"] != float64(5_000_000) || got[1]["period"] != "day" {
		t.Errorf("rate_limits = %v, want 60 requests a minute and 5000000 tokens a day", got)
	}
	err := Run(context.Background(), []string{"limit", "project", "payments", "--per", "week"})
	if err == nil || !strings.Contains(err.Error(), "--per must be minute, day or month") {
		t.Errorf("err = %v, want --per refused", err)
	}
}

func TestGuardrailTakesAProjectByNameInOneOfSeveralOrgs(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/orgs": map[string]any{"data": []map[string]any{
			{"id": "org_1", "name": "Example Bank"}, {"id": "org_2", "name": "Other"},
		}},
		"GET /v1/projects":                     paymentsProject,
		"GET /v1/guardrails/project/project_1": map[string]any{},
		"PUT /v1/guardrails/project/project_1": map[string]any{},

		"GET /v1/guardrails/project/project_1/effective": map[string]any{},
	})
	if err := Run(context.Background(), []string{"budget", "project", "payments"}); err == nil ||
		!strings.Contains(err.Error(), "keera org switch") {
		t.Errorf("without a chosen organisation = %v, want to be told to choose one", err)
	}
	t.Setenv("KEERA_ORG", "org_1")
	if err := Run(context.Background(), []string{
		"budget", "project", "payments",
		"--budget", "500",
	}); err != nil {
		t.Fatal(err)
	}
	if !f.called("PUT", "/v1/guardrails/project/project_1") {
		t.Error("the project named payments in org_1 was not changed")
	}
}

// A missing filter or router is reported before the prompt.
func TestDeleteLooksTheAliasUpBeforeAsking(t *testing.T) {
	quiet(t)
	newFakeControl(t, map[string]any{
		"GET /v1/filters": map[string]any{"data": []any{}},
		"GET /v1/routers": map[string]any{"data": []any{}},
	})
	for _, cmd := range []string{"filter", "router"} {
		err := Run(context.Background(), []string{cmd, "delete", "typo"})
		if err == nil || !strings.Contains(err.Error(), `no `+cmd+` "typo"`) {
			t.Errorf("%s delete typo: err = %v, want the alias refused", cmd, err)
		}
	}
}

// A command that cannot be right is refused before anything is sent, so no
// flag is quietly dropped.
func TestRefusedBeforeAnyCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string // in the error; empty when any error will do
	}{
		{"org set with nothing to change", []string{"org", "set", "org_2"}, "nothing to change"},
		{"org list with --domain", []string{"org", "list", "--domain", "example.ch"}, "does not take"},
		{"org create with --no-domain", []string{"org", "create", "Another Bank", "--no-domain"}, "does not take"},
		{"org delete with --domain", []string{"org", "delete", "org_1", "--domain", "example.ch", "--yes"}, "does not take"},
		{"guardrail get with --requests", []string{"guardrail", "get", "project", "project_1", "--requests", "5"}, "does not take"},
		{"model enable with --provider", []string{"model", "enable", "keera-speed", "--provider", "openai"}, "does not take --provider"},
		// Only the gateway's configuration grants the operator role.
		{"the operator role", []string{"user", "role", "alice@example.com", "operator"}, "KEERA_OPERATORS"},
		{"key create named twice", []string{"key", "create", "laptop", "--name", "phone"}, "name once"},
		{"a filter mode that does not exist", []string{"filter", "add", "a", "--mode", "redact"}, "--mode"},
		{"filter --shadow and --no-shadow", []string{"filter", "set", "redact-secrets", "--shadow", "--no-shadow"}, "opposites"},
		{"model typo", []string{"model", "lst"}, ""},
		{"filter typo", []string{"filter", "lst"}, ""},
		{"router typo", []string{"router", "lst"}, ""},
		{"mcp typo", []string{"mcp", "lst"}, ""},
		{"usage with an extra argument", []string{"usage", "--by", "project", "extra"}, ""},
		{"mcp connect with --json", []string{"mcp", "connect", "github", "--json"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			quiet(t)
			f := newFakeControl(t, map[string]any{})
			err := Run(context.Background(), tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("keera %s: err = %v, want %q", strings.Join(tc.args, " "), err, tc.want)
			}
			if len(f.seen) != 0 {
				t.Errorf("keera %s called the control plane: %+v", strings.Join(tc.args, " "), f.seen)
			}
		})
	}
}

func TestRotatingARevokedKeyWithNoProjectSuggestsNoProject(t *testing.T) {
	quiet(t)
	newFakeControl(t, map[string]any{
		"GET /v1/keys": map[string]any{"data": []map[string]any{
			{"id": "key_1", "name": "ci", "revoked_at": "2026-01-01T00:00:00Z"},
		}},
	})
	err := keyCmd(context.Background(), []string{"rotate", "key_1", "--org", "org_1", "--yes"})
	if err == nil || strings.Contains(err.Error(), "--project") {
		t.Errorf("err = %v, want a hint without --project", err)
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
	printLimits(tw, policy.Limits{MaxSandboxMemory: &memory}, "project")
	_ = tw.Flush()
	if !strings.Contains(w.String(), "any CPU, 8Gi") {
		t.Errorf("a memory-only limit was not shown:\n%s", w.String())
	}
}

// An 'add' that overwrote would drop every field it was not given.
func TestAddRefusesAnAliasThatExists(t *testing.T) {
	quiet(t)
	existing := map[string]any{"data": []map[string]any{{"alias": "a"}}}
	f := newFakeControl(t, map[string]any{
		"GET /v1/filters":     existing,
		"GET /v1/routers":     existing,
		"GET /v1/mcp-servers": existing,
		"GET /v1/models":      existing,
	})
	for _, args := range [][]string{
		{"filter", "add", "a", "--mode", "pattern", "--rules", "x => y"},
		{"router", "add", "a", "--mode", "fallback", "--destinations", "b,c"},
		{"mcp", "add", "a", "--endpoint", "https://mcp.example.ch"},
		{"model", "add", "a", "--backend", "http://vllm:8000/v1", "--backend-model", "qwen"},
	} {
		err := Run(context.Background(), args)
		if err == nil || !strings.Contains(err.Error(), "keera "+args[0]+" set a") {
			t.Errorf("keera %s: err = %v, want it to point at set", strings.Join(args, " "), err)
		}
	}
	for _, r := range f.seen {
		if r.method == "PUT" {
			t.Errorf("an add of an existing alias wrote %s", r.path)
		}
	}
}

// Rotating revokes the old key at once, so it asks first.
func TestKeyRotateAsksBeforeItRotates(t *testing.T) {
	_, stderr := quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/keys":                 liveKey,
		"POST /v1/keys/key_old/rotate": map[string]any{"id": "key_new", "key": "keera_sk_new"},
	})

	if err := keyCmd(context.Background(), []string{"rotate", "a developer's laptop"}); err == nil {
		t.Fatal("rotate went ahead without a confirmation")
	}
	if f.called("POST", "/v1/keys/key_old/rotate") {
		t.Error("the key was rotated although nothing confirmed it")
	}
	if said := stderr(); !strings.Contains(said, "Rotating a developer's laptop") {
		t.Errorf("stderr = %q, want the prompt on it", said)
	}
}

// A new key gets the panel's default lifetime; 'never' asks for none.
func TestKeyCreateExpiresByDefault(t *testing.T) {
	quiet(t)
	for _, tc := range []struct {
		flags []string
		want  any
	}{
		{nil, float64(90 * 24 * 3600)},
		{[]string{"--expires", "720h"}, float64(720 * 3600)},
		{[]string{"--expires", "never"}, nil},
	} {
		f := newFakeControl(t, map[string]any{
			"POST /v1/keys": map[string]any{"id": "key_1", "key": "keera_sk_new"},
		})
		args := append([]string{"create", "laptop"}, tc.flags...)
		if err := keyCmd(context.Background(), args); err != nil {
			t.Fatal(err)
		}
		if got := f.request("POST", "/v1/keys").body["expires_in_seconds"]; got != tc.want {
			t.Errorf("%v: expires_in_seconds = %v, want %v", tc.flags, got, tc.want)
		}
	}
	if err := keyCmd(context.Background(), []string{"rotate", "laptop", "--expires", "never"}); err == nil {
		t.Error("rotate accepted --expires never, which the control API reads as keeping the old lifetime")
	}
}

// A day for --expires is the last day the key works. A past day is refused.
func TestKeyCreateExpiresOnADay(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"POST /v1/keys": map[string]any{"id": "key_1", "key": "keera_sk_new"},
	})
	day := time.Now().AddDate(0, 1, 0).Format(time.DateOnly)
	if err := keyCmd(context.Background(), []string{"create", "laptop", "--expires", day}); err != nil {
		t.Fatal(err)
	}
	sent := f.request("POST", "/v1/keys").body
	raw, _ := sent["expires_at"].(string)
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil || sent["expires_in_seconds"] != nil {
		t.Fatalf("sent %v, want only expires_at", sent)
	}
	if at.Local().Format(time.DateOnly) != day || at.Local().Hour() != 23 {
		t.Errorf("expires_at = %v, want the end of %s", at.Local(), day)
	}

	past := time.Now().AddDate(0, 0, -1).Format(time.DateOnly)
	if err := keyCmd(context.Background(), []string{"create", "laptop", "--expires", past}); err == nil {
		t.Error("create accepted a day that has already passed")
	}
}

func TestDeleteJSONKeepsWhatTheServerSaid(t *testing.T) {
	newFakeControl(t, map[string]any{
		"DELETE /v1/orgs/org_2": map[string]any{
			"id": "org_2", "name": "Another Bank", "projects": 1, "sandbox_classes": 3,
		},
	})
	out := capture(t, "org", "delete", "org_2", "--yes", "--json")
	if !strings.Contains(out, "sandbox_classes") {
		t.Errorf("--json dropped a field the server sent:\n%s", out)
	}
}

// A leading flag means the listing, or the list of verbs where there is none.
func TestALeadingFlagIsTheListing(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/projects": map[string]any{"data": []any{}},
	})
	t.Setenv("KEERA_ORG", "org_1")
	if err := Run(context.Background(), []string{"project", "--json"}); err != nil {
		t.Fatal(err)
	}
	f.request("GET", "/v1/projects")
	err := Run(context.Background(), []string{"guardrail", "--json"})
	if err == nil || !strings.Contains(err.Error(), "needs a subcommand") {
		t.Errorf("guardrail --json: err = %v, want it to ask for a subcommand", err)
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		42 * time.Second:                "42s",
		11*time.Minute + 30*time.Second: "11m30s",
		24 * time.Hour:                  "24h",
		7 * 24 * time.Hour:              "7d",
		3*time.Hour + 12*time.Minute:    "3h12m",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestOrgCommandsTakeAName(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"PATCH /v1/orgs/org_1":      map[string]any{"id": "org_1", "name": "Example Bank AG"},
		"DELETE /v1/orgs/org_1":     map[string]any{"id": "org_1"},
		"PATCH /v1/billing/account": map[string]any{"org_id": "org_1", "invoiced": true},
	})
	for _, args := range [][]string{
		{"org", "set", "example bank", "--name", "Example Bank AG"},
		{"org", "delete", "Example Bank", "--yes"},
		{"billing", "invoiced", "--org", "Example Bank"},
	} {
		if err := Run(context.Background(), args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	f.request("PATCH", "/v1/orgs/org_1")
	f.request("DELETE", "/v1/orgs/org_1")
	if q := f.request("PATCH", "/v1/billing/account").query; !strings.Contains(q, "org_id=org_1") {
		t.Errorf("invoiced went to %q, want org_1", q)
	}
}

func TestBillingInvoicedOff(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"PATCH /v1/billing/account": map[string]any{"org_id": "org_1"},
	})
	if err := Run(context.Background(), []string{"billing", "invoiced", "--org", "org_1", "--off"}); err != nil {
		t.Fatal(err)
	}
	if got := f.request("PATCH", "/v1/billing/account").body["invoiced"]; got != false {
		t.Errorf("invoiced = %v, want false", got)
	}
}

// Forgetting the card asks first unless --yes is given.
func TestBillingForgetCardConfirms(t *testing.T) {
	account := map[string]any{"payments": true, "account": map[string]any{
		"org_id": "org_1", "card": map[string]any{"label": "Visa 4242"},
	}}

	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/billing/account":         account,
		"DELETE /v1/billing/account/card": nil,
	})
	stdin(t, "no\n")
	t.Setenv("KEERA_ORG", "org_1")
	if err := Run(context.Background(), []string{"billing", "forget-card"}); err == nil {
		t.Error("a wrong answer forgot the card")
	}
	if f.called("DELETE", "/v1/billing/account/card") {
		t.Error("the card was deleted without a confirmation")
	}

	if err := Run(context.Background(), []string{"billing", "forget-card", "--yes"}); err != nil {
		t.Fatal(err)
	}
	f.request("DELETE", "/v1/billing/account/card")
}

func stdin(t *testing.T, text string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.WriteString(text)
	_ = w.Close()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old; _ = r.Close() })
}

// --no- flags clear, so no word is reserved that a model could be called.
func TestGuardrailClearsWithNoFlags(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/guardrails/org/org_1": map[string]any{
			"allowed_models": []string{"keera-speed"}, "allowed_tools": []string{"github"},
		},
		"PUT /v1/guardrails/org/org_1": map[string]any{},
	})
	if err := Run(context.Background(), []string{
		"guardrail", "set", "org", "org_1",
		"--no-models", "--no-tools",
	}); err != nil {
		t.Fatal(err)
	}
	body := f.request("PUT", "/v1/guardrails/org/org_1").body
	if body["allowed_models"] != nil || body["allowed_tools"] != nil {
		t.Errorf("sent %v, want both cleared", body)
	}

	err := Run(context.Background(), []string{
		"guardrail", "set", "org", "org_1",
		"--models", "any", "--no-models",
	})
	if err == nil || !strings.Contains(err.Error(), "opposites") {
		t.Errorf("--models with --no-models: err = %v, want opposites", err)
	}
}

func TestUsageNarrowsLikeTheOtherReports(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/projects": paymentsProject,
		"GET /v1/usage":    map[string]any{"data": []any{}},
	})
	if err := Run(context.Background(), []string{
		"usage", "--project", "payments",
		"--model", "keera-speed",
	}); err != nil {
		t.Fatal(err)
	}
	q := f.request("GET", "/v1/usage").query
	if !strings.Contains(q, "project_id=project_1") || !strings.Contains(q, "alias=keera-speed") {
		t.Errorf("usage query = %q, want the project and the model", q)
	}
}

// A key issued by a person is theirs unless they say otherwise.
func TestKeyCreateIsTheCallersByDefault(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/me":    map[string]any{"user_id": "user_me", "role": "admin", "org_id": "org_1"},
		"POST /v1/keys": map[string]any{"id": "key_1", "key": "sk-keera-secret"},
	})

	if err := keyCmd(context.Background(), []string{"create", "laptop"}); err != nil {
		t.Fatal(err)
	}
	if got := f.request("POST", "/v1/keys").body["user_id"]; got != "user_me" {
		t.Errorf("user_id sent = %v, want the caller's", got)
	}

	if err := keyCmd(context.Background(), []string{"create", "pipeline", "--no-user"}); err != nil {
		t.Fatal(err)
	}
	if got, set := f.seen[len(f.seen)-1].body["user_id"]; set {
		t.Errorf("--no-user sent user_id %v", got)
	}
}

// A pending key has no secret to print.
func TestKeyCreatePendingNeedsAPersonAndPrintsNoSecret(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/users": map[string]any{"data": []map[string]any{
			{"id": "user_1", "org_id": "org_1", "email": "alice@example.com", "role": "member"},
		}},
		"POST /v1/keys": map[string]any{"id": "key_1", "user_id": "user_1", "pending": true},
	})

	if err := keyCmd(context.Background(),
		[]string{"create", "laptop", "--user", "alice@example.com", "--pending"}); err != nil {
		t.Fatal(err)
	}
	if got := f.request("POST", "/v1/keys").body["pending"]; got != true {
		t.Errorf("pending sent = %v, want true", got)
	}

	err := keyCmd(context.Background(), []string{"create", "ci", "--no-user", "--pending"})
	if err == nil || !strings.Contains(err.Error(), "--user") {
		t.Errorf("err = %v, want a pending key to need a person", err)
	}
}

func TestKeyActivateAsksTheControlPlane(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/keys": map[string]any{"data": []map[string]any{
			{"id": "key_p", "org_id": "org_1", "user_id": "user_1", "name": "laptop", "pending": true},
		}},
		"POST /v1/keys/key_p/activate": map[string]any{"id": "key_p", "key": "keera_sk_new"},
	})

	if err := keyCmd(context.Background(), []string{"activate", "laptop"}); err != nil {
		t.Fatal(err)
	}
	if !f.called("POST", "/v1/keys/key_p/activate") {
		t.Errorf("the key was not activated: %v", f.seen)
	}
}

// Each budget is printed with its level, and none at all is unlimited.
func TestEffectiveBudgetsNameTheirLevel(t *testing.T) {
	style = false
	eff := control.Effective{Levels: []control.EffectiveLevel{
		{Type: policy.ScopeOrg, ID: "org_1", Name: "acme", Limits: policy.Limits{
			Budgets: []policy.Budget{{BudgetMicros: 100_000_000}},
		}},
		{Type: policy.ScopeKey, ID: "key_1", Limits: policy.Limits{
			Budgets: []policy.Budget{{BudgetMicros: 5_000_000, Period: policy.PeriodDay}},
		}},
	}}
	var out bytes.Buffer
	w := newTable(&out)
	printEffectiveSpend(w, eff)
	printEffectiveSpend(w, control.Effective{})
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(strings.Fields(out.String()), " ")
	for _, want := range []string{
		"CHF 100.00 per month set by org acme",
		"CHF 5.00 per day set by key key_1",
		"budget (unlimited)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}
