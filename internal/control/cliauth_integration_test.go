package control

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/store"
)

// The half of a command-line sign-in after the code is issued: the exchange
// and the token it produces. The token must act as the person who signed in,
// with their role and organisation, not as the operator.

// signedInAtATerminal is a person with a code waiting to be redeemed, and the
// server to redeem it against.
func signedInAtATerminal(t *testing.T, verifier string) (*httptest.Server, string, store.User) {
	t.Helper()
	st, ctx := testStore(t, "users", "cli_codes", "cli_tokens", "sessions")
	if _, err := st.CreateOrg(ctx, store.Org{ID: "org_1", Name: "Example Bank"}, store.OrgTemplate{}); err != nil {
		t.Fatalf("creating the organisation: %v", err)
	}
	user, err := st.AddUser(ctx, "user_1", "org_1", "alice@example.ch", "sso:alice", "member")
	if err != nil {
		t.Fatalf("creating the person: %v", err)
	}

	code, hash := authn.NewCLICode()
	if err := st.CreateCLICode(ctx, hash, user.ID, authn.CLIChallenge(verifier),
		time.Now().Add(authn.CLICodeTTL)); err != nil {
		t.Fatalf("recording the code: %v", err)
	}

	srv := New(st, nil, nil, nil, Options{OperatorKey: testOperatorKey},
		slog.New(slog.DiscardHandler))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, code, user
}

// redeem posts to the code-redeeming route with no credential.
func redeem(t *testing.T, ts *httptest.Server, in, out any) int {
	t.Helper()
	const path = "/auth/cli/token"
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		ts.URL+httpx.ControlPrefix+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = res.Body.Close() }()
	if out != nil && res.StatusCode == http.StatusOK {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			t.Fatalf("decoding %s: %v", path, err)
		}
	}
	return res.StatusCode
}

// asToken reads /v1/me the way every command does: the token in the header,
// nothing else.
func asToken(t *testing.T, ts *httptest.Server, token string, out any) int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		ts.URL+httpx.ControlPrefix+"/v1/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /v1/me: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if out != nil && res.StatusCode == http.StatusOK {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			t.Fatalf("decoding /v1/me: %v", err)
		}
	}
	return res.StatusCode
}

// A terminal ends up signed in as the person, with their role and
// organisation, not with the operator key.
func TestARedeemedCodeSignsTheTerminalInAsThePerson(t *testing.T) {
	verifier := authn.NewCLIVerifier()
	ts, code, user := signedInAtATerminal(t, verifier)

	var issued struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if status := redeem(t, ts, map[string]string{
		"code": code, "verifier": verifier, "label": "alice@thinkpad",
	}, &issued); status != http.StatusOK {
		t.Fatalf("redeeming the code: status = %d, want 200", status)
	}
	if issued.Token == "" || issued.ExpiresAt.Before(time.Now()) {
		t.Fatalf("issued = %+v, want a token that has not already run out", issued)
	}

	var me struct {
		Via   string `json:"via"`
		Email string `json:"email"`
		Role  string `json:"role"`
		OrgID string `json:"org_id"`
	}
	if status := asToken(t, ts, issued.Token, &me); status != http.StatusOK {
		t.Fatalf("/v1/me with the token: status = %d, want 200", status)
	}
	if me.Email != user.Email || me.Role != "member" || me.OrgID != "org_1" {
		t.Errorf("/v1/me = %+v, want the person who signed in", me)
	}
	if me.Via != string(authn.MethodCLI) {
		t.Errorf("via = %q, want %q", me.Via, authn.MethodCLI)
	}

	// And signing out ends it, on this machine, at once.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		ts.URL+httpx.ControlPrefix+"/auth/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+issued.Token)
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("signing out: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("signing out: status = %d, want 200", res.StatusCode)
	}
	if status := asToken(t, ts, issued.Token, nil); status != http.StatusUnauthorized {
		t.Errorf("/v1/me after signing out: status = %d, want 401", status)
	}
}

// The code travels in a URL, so it lands in browser and shell history. It
// must work only once.
func TestACodeCannotBeRedeemedTwice(t *testing.T) {
	verifier := authn.NewCLIVerifier()
	ts, code, _ := signedInAtATerminal(t, verifier)

	body := map[string]string{"code": code, "verifier": verifier, "label": "alice@thinkpad"}
	if status := redeem(t, ts, body, nil); status != http.StatusOK {
		t.Fatalf("redeeming the code: status = %d, want 200", status)
	}
	if status := redeem(t, ts, body, nil); status != http.StatusUnauthorized {
		t.Errorf("redeeming it again: status = %d, want 401", status)
	}
}

// Any process on the machine can bind a loopback port and get the code first.
// It does not have the verifier.
func TestACodeIsWorthNothingWithoutTheVerifier(t *testing.T) {
	verifier := authn.NewCLIVerifier()
	ts, code, _ := signedInAtATerminal(t, verifier)

	other := authn.NewCLIVerifier()
	if status := redeem(t, ts, map[string]string{
		"code": code, "verifier": other,
	}, nil); status != http.StatusUnauthorized {
		t.Errorf("redeeming with another process's verifier: status = %d, want 401", status)
	}
	// The code is spent either way, so a guesser gets one attempt.
	if status := redeem(t, ts, map[string]string{
		"code": code, "verifier": verifier,
	}, nil); status != http.StatusUnauthorized {
		t.Errorf("redeeming with the right verifier afterwards: status = %d, want 401", status)
	}
}

// A role change signs somebody out everywhere, and a terminal they are still
// signed in on is part of everywhere.
func TestARoleChangeEndsASignedInTerminal(t *testing.T) {
	verifier := authn.NewCLIVerifier()
	ts, code, user := signedInAtATerminal(t, verifier)

	var issued struct {
		Token string `json:"token"`
	}
	if status := redeem(t, ts, map[string]string{
		"code": code, "verifier": verifier,
	}, &issued); status != http.StatusOK {
		t.Fatalf("redeeming the code: status = %d, want 200", status)
	}

	// As the operator key, the way `keera user role` does it.
	raw, err := json.Marshal(map[string]string{"role": "admin"})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPatch,
		ts.URL+httpx.ControlPrefix+"/v1/users/"+user.ID, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testOperatorKey)
	req.Header.Set("Content-Type", "application/json")
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("changing the role: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("changing the role: status = %d, want 200", res.StatusCode)
	}

	if status := asToken(t, ts, issued.Token, nil); status != http.StatusUnauthorized {
		t.Errorf("/v1/me after the role change: status = %d, want 401", status)
	}
}
