// Package oidctest is a minimal but real OpenID provider, for tests of
// signing in and of asking the directory again: discovery, a JWK set, and a
// token endpoint that mints properly signed id_tokens. It exists so the whole
// exchange is exercised, signature and nonce checks included, rather than
// assumed to work.
package oidctest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// IDP is the provider. Its fields bend what the next token response says;
// set them before the request they are for.
type IDP struct {
	*httptest.Server
	// Key signs id_tokens. Replacing it makes signatures fail, because the
	// JWK set keeps advertising the original.
	Key *rsa.PrivateKey
	// Claims edits what the next id_token carries.
	Claims func(m map[string]any)
	// RefreshToken is returned with every token response. Empty returns none.
	RefreshToken string
	// RefuseRefresh is the OAuth error code a refresh is answered with, such
	// as "invalid_grant". Empty lets refreshes through.
	RefuseRefresh string
	// FailRefresh answers a refresh with a 500, like a provider that is down.
	FailRefresh bool
	// NoIDTokenOnRefresh leaves the id_token out of a refresh's answer, as
	// some providers do.
	NoIDTokenOnRefresh bool

	mu       sync.Mutex
	lastForm url.Values
	refresh  int
}

// New starts a provider that lives as long as the test.
func New(t *testing.T) *IDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &IDP{Key: key}

	mux := http.NewServeMux()
	idp.Server = httptest.NewServer(mux)
	t.Cleanup(idp.Close)

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"issuer":                                idp.URL,
			"authorization_endpoint":                idp.URL + "/authorize",
			"token_endpoint":                        idp.URL + "/token",
			"jwks_uri":                              idp.URL + "/jwks",
			"end_session_endpoint":                  idp.URL + "/logout",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: key.Public(), KeyID: "test", Algorithm: "RS256", Use: "sig",
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		idp.mu.Lock()
		idp.lastForm = r.PostForm
		refresh := r.PostForm.Get("grant_type") == "refresh_token"
		if refresh {
			idp.refresh++
		}
		idp.mu.Unlock()
		switch {
		case refresh && idp.FailRefresh:
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "server_error"})
			return
		case refresh && idp.RefuseRefresh != "":
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": idp.RefuseRefresh})
			return
		}
		claims := map[string]any{
			"iss":    idp.URL,
			"aud":    "keera",
			"sub":    "sub-123",
			"exp":    time.Now().Add(time.Hour).Unix(),
			"iat":    time.Now().Unix(),
			"email":  "ada@example.ch",
			"name":   "Ada Lovelace",
			"groups": []string{"engineering", "keera-admins"},
		}
		if idp.Claims != nil {
			idp.Claims(claims)
		}
		out := map[string]any{
			"access_token": "at",
			"token_type":   "Bearer",
			"expires_in":   3600,
		}
		if !refresh || !idp.NoIDTokenOnRefresh {
			out["id_token"] = idp.Sign(t, claims)
		}
		if idp.RefreshToken != "" {
			out["refresh_token"] = idp.RefreshToken
		}
		writeJSON(w, http.StatusOK, out)
	})
	return idp
}

// LastForm is what the client posted to the token endpoint last, which is
// where PKCE either happened or did not.
func (idp *IDP) LastForm() url.Values {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	return idp.lastForm
}

// Refreshes counts the refresh grants the provider has seen.
func (idp *IDP) Refreshes() int {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	return idp.refresh
}

// Sign makes a signed JWT of claims.
func (idp *IDP) Sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: idp.Key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test"))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
