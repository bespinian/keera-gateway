package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A command-line sign-in is two rows, both credentials: redeeming a code
// deletes it, an expired row is invisible, and deleting a person removes both.

func signedInUser(t *testing.T, st *Store, ctx context.Context) string {
	t.Helper()
	if _, err := st.CreateOrg(ctx, Org{ID: "org_1", Name: "Example Bank"}, OrgTemplate{}); err != nil {
		t.Fatalf("creating the organisation: %v", err)
	}
	u, err := st.AddUser(ctx, "user_1", "org_1", "alice@example.ch", "sso:alice", "member")
	if err != nil {
		t.Fatalf("creating the person: %v", err)
	}
	return u.ID
}

// The code travels in a URL, so it works only once.
func TestACLICodeIsRedeemedExactlyOnce(t *testing.T) {
	st, ctx := db(t)
	userID := signedInUser(t, st, ctx)

	hash := []byte("code-hash-one-code-hash-one-0123")
	if err := st.CreateCLICode(ctx, hash, userID, "challenge",
		time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("recording the code: %v", err)
	}

	code, err := st.TakeCLICode(ctx, hash)
	if err != nil {
		t.Fatalf("redeeming the code: %v", err)
	}
	if code.UserID != userID || code.Challenge != "challenge" {
		t.Errorf("redeemed %+v, want the person and challenge it was written with", code)
	}
	if _, err := st.TakeCLICode(ctx, hash); !errors.Is(err, ErrNotFound) {
		t.Errorf("redeeming it again = %v, want ErrNotFound", err)
	}
}

// An expired code reads as gone before the sweep removes it.
func TestAnExpiredCLICodeIsAlreadyGone(t *testing.T) {
	st, ctx := db(t)
	userID := signedInUser(t, st, ctx)

	hash := []byte("code-hash-two-code-hash-two-0123")
	if err := st.CreateCLICode(ctx, hash, userID, "challenge",
		time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("recording the code: %v", err)
	}
	if _, err := st.TakeCLICode(ctx, hash); !errors.Is(err, ErrNotFound) {
		t.Errorf("redeeming an expired code = %v, want ErrNotFound", err)
	}
}

// A token lookup carries the person behind it, whose role and organisation
// authorise the request.
func TestACLITokenResolvesToThePersonBehindIt(t *testing.T) {
	st, ctx := db(t)
	userID := signedInUser(t, st, ctx)

	hash := []byte("token-hash-one-token-hash-one-01")
	if err := st.CreateCLIToken(ctx, hash, userID,
		time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("storing the token: %v", err)
	}
	tu, err := st.LookupCLIToken(ctx, hash)
	if err != nil {
		t.Fatalf("looking the token up: %v", err)
	}
	if tu.User.ID != userID || tu.User.Email != "alice@example.ch" ||
		tu.User.Role != "member" || tu.User.OrgID != "org_1" {
		t.Errorf("resolved %+v, want the person the token was issued to", tu.User)
	}

	if err := st.DeleteCLIToken(ctx, hash); err != nil {
		t.Fatalf("deleting the token: %v", err)
	}
	if _, err := st.LookupCLIToken(ctx, hash); !errors.Is(err, ErrNotFound) {
		t.Errorf("looking up a deleted token = %v, want ErrNotFound", err)
	}
}

func TestAnExpiredCLITokenIsNotAToken(t *testing.T) {
	st, ctx := db(t)
	userID := signedInUser(t, st, ctx)

	hash := []byte("token-hash-two-token-hash-two-01")
	if err := st.CreateCLIToken(ctx, hash, userID,
		time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("storing the token: %v", err)
	}
	if _, err := st.LookupCLIToken(ctx, hash); !errors.Is(err, ErrNotFound) {
		t.Errorf("looking up an expired token = %v, want ErrNotFound", err)
	}
}

// Signing somebody out also ends their command-line tokens and half-finished
// sign-ins, which could mint a fresh token.
func TestSigningSomebodyOutReachesTheirTerminals(t *testing.T) {
	st, ctx := db(t)
	userID := signedInUser(t, st, ctx)

	token := []byte("token-hash-out-token-hash-out-01")
	code := []byte("code-hash-out--code-hash-out--01")
	if err := st.CreateCLIToken(ctx, token, userID,
		time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("storing the token: %v", err)
	}
	if err := st.CreateCLICode(ctx, code, userID, "challenge",
		time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("recording the code: %v", err)
	}

	if err := st.DeleteUserSessions(ctx, userID); err != nil {
		t.Fatalf("signing them out: %v", err)
	}
	if _, err := st.LookupCLIToken(ctx, token); !errors.Is(err, ErrNotFound) {
		t.Errorf("the token survived a sign-out: %v", err)
	}
	if _, err := st.TakeCLICode(ctx, code); !errors.Is(err, ErrNotFound) {
		t.Errorf("the half-finished sign-in survived a sign-out: %v", err)
	}
}

// The loopback handshake is stored on the login flow, because the callback
// must not read where to send the answer from its own URL.
func TestALoginFlowCarriesTheLoopbackHandshake(t *testing.T) {
	st, ctx := db(t)

	if err := st.CreateLoginFlow(ctx, LoginFlow{
		State: "state", Verifier: "verifier", Nonce: "nonce", Provider: "google",
		CLIRedirect: "http://127.0.0.1:1234/callback", CLIChallenge: "challenge",
		CLIState: "cli-state",
	}, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("starting the sign-in: %v", err)
	}
	flow, err := st.TakeLoginFlow(ctx, "state")
	if err != nil {
		t.Fatalf("completing the sign-in: %v", err)
	}
	if !flow.CLI() {
		t.Error("the flow does not read as a command-line sign-in")
	}
	if flow.CLIRedirect != "http://127.0.0.1:1234/callback" ||
		flow.CLIChallenge != "challenge" || flow.CLIState != "cli-state" {
		t.Errorf("flow = %+v, want the handshake it was started with", flow)
	}
}

// An ordinary sign-in carries none of it, and must not read as a command-line
// one.
func TestAPanelSignInIsNotACommandLineOne(t *testing.T) {
	st, ctx := db(t)

	if err := st.CreateLoginFlow(ctx, LoginFlow{
		State: "state", Verifier: "verifier", Nonce: "nonce", Provider: "google",
		RedirectTo: "/projects",
	}, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("starting the sign-in: %v", err)
	}
	flow, err := st.TakeLoginFlow(ctx, "state")
	if err != nil {
		t.Fatalf("completing the sign-in: %v", err)
	}
	if flow.CLI() {
		t.Errorf("flow = %+v, want it to read as a sign-in that ends in the panel", flow)
	}
}
