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

// The half of a command-line sign-in that does not need an identity provider:
// the code has been issued, and what happens from there is the exchange and the
// credential it produces. What these cover is that the token really does
// authenticate as the person who signed in - with their role and their
// organisation and not the operator's - and that the two things which must not
// work do not.

// signedInAtATerminal is a person with a code waiting to be redeemed, and the
// server to redeem it against.
func signedInAtATerminal(t *testing.T, verifier string) (*httptest.Server, string, store.User) {
	t.Helper()
	st, ctx := streamStore(t)
	if _, err := st.Pool().Exec(ctx,
		"TRUNCATE users, cli_codes, cli_tokens, sessions RESTART IDENTITY CASCADE"); err != nil {
		t.Fatalf("emptying the tables: %v", err)
	}
	if _, err := st.CreateOrg(ctx, "org_1", "Example Bank"); err != nil {
		t.Fatalf("creating the organisation: %v", err)
	}
	user, err := st.UpsertUser(ctx, "user_1", "org_1", "alice@example.ch", "sso:alice", "member")
	if err != nil {
		t.Fatalf("creating the person: %v", err)
	}

	code, hash, err := authn.NewCLICode()
	if err != nil {
		t.Fatalf("minting the code: %v", err)
	}
	if err := st.CreateCLICode(ctx, hash, user.ID, authn.CLIChallenge(verifier),
		time.Now().Add(authn.CLICodeTTL)); err != nil {
		t.Fatalf("recording the code: %v", err)
	}

	srv := New(st, nil, nil, nil, Options{OperatorKey: testOperatorKey, Currency: "CHF"},
		slog.New(slog.DiscardHandler))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, code, user
}

// redeem posts one JSON body to the code-redeeming route, with no credential.
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

// The point of the whole feature: what a terminal ends up holding is that
// person, with their role and their organisation - not the operator key, which
// is what it would have been holding before.
func TestARedeemedCodeSignsTheTerminalInAsThePerson(t *testing.T) {
	verifier, err := authn.NewCLIVerifier()
	if err != nil {
		t.Fatal(err)
	}
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

// The code travels in a URL, so it lands in a browser's history and sometimes in
// a shell's. Redeeming it has to be something that works exactly once.
func TestACodeCannotBeRedeemedTwice(t *testing.T) {
	verifier, err := authn.NewCLIVerifier()
	if err != nil {
		t.Fatal(err)
	}
	ts, code, _ := signedInAtATerminal(t, verifier)

	body := map[string]string{"code": code, "verifier": verifier, "label": "alice@thinkpad"}
	if status := redeem(t, ts, body, nil); status != http.StatusOK {
		t.Fatalf("redeeming the code: status = %d, want 200", status)
	}
	if status := redeem(t, ts, body, nil); status != http.StatusUnauthorized {
		t.Errorf("redeeming it again: status = %d, want 401", status)
	}
}

// Loopback ports are not private: any process on the machine can bind one, and
// any of them could reach the listener before the one that started the sign-in.
// The verifier is what that other process does not have.
func TestACodeIsWorthNothingWithoutTheVerifier(t *testing.T) {
	verifier, err := authn.NewCLIVerifier()
	if err != nil {
		t.Fatal(err)
	}
	ts, code, _ := signedInAtATerminal(t, verifier)

	other, err := authn.NewCLIVerifier()
	if err != nil {
		t.Fatal(err)
	}
	if status := redeem(t, ts, map[string]string{
		"code": code, "verifier": other,
	}, nil); status != http.StatusUnauthorized {
		t.Errorf("redeeming with another process's verifier: status = %d, want 401", status)
	}
	// And the code is spent either way, so a guesser gets one attempt rather
	// than as many as they can make before it expires.
	if status := redeem(t, ts, map[string]string{
		"code": code, "verifier": verifier,
	}, nil); status != http.StatusUnauthorized {
		t.Errorf("redeeming with the right verifier afterwards: status = %d, want 401", status)
	}
}

// A role change signs somebody out everywhere, and a terminal they are still
// signed in on is part of everywhere.
func TestARoleChangeEndsASignedInTerminal(t *testing.T) {
	verifier, err := authn.NewCLIVerifier()
	if err != nil {
		t.Fatal(err)
	}
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
