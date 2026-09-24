package forge

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/sandbox"
)

func TestParseRepo(t *testing.T) {
	for _, tc := range []struct {
		in, host, path string
	}{
		{"https://github.com/acme/app.git", "github.com", "acme/app"},
		{"https://github.com/acme/app", "github.com", "acme/app"},
		{"git@github.com:acme/app.git", "github.com", "acme/app"},
		{"ssh://git@gitlab.example.ch:2222/group/sub/app.git", "gitlab.example.ch", "group/sub/app"},
		{"https://GitLab.Example.ch/group/app/", "gitlab.example.ch", "group/app"},
	} {
		r, err := parseRepo(tc.in)
		if err != nil {
			t.Errorf("parseRepo(%q): %v", tc.in, err)
			continue
		}
		if r.host != tc.host || r.path != tc.path {
			t.Errorf("parseRepo(%q) = %s %s, want %s %s", tc.in, r.host, r.path, tc.host, tc.path)
		}
	}
	for _, bad := range []string{
		"", "acme/app", "https://github.com/app", "https://github.com/acme/../admin",
		"https://github.com/acme/app?x=1#y/z", "https://github.com/acme/%2e%2e", "git@github.com:",
	} {
		_, err := parseRepo(bad)
		var refused *sandbox.ErrRefused
		if !errors.As(err, &refused) {
			t.Errorf("parseRepo(%q) = %v, want a refusal", bad, err)
		}
	}
}

func TestRepoOn(t *testing.T) {
	web, _ := url.Parse("https://example.ch/gitlab")
	r, err := parseRepo("https://example.ch/gitlab/group/app.git")
	if err != nil {
		t.Fatal(err)
	}
	r, err = r.on(web)
	if err != nil || r.path != "group/app" {
		t.Fatalf("on = %q, %v; want group/app", r.path, err)
	}
	if got := r.cloneURL(web); got != "https://example.ch/gitlab/group/app.git" {
		t.Errorf("cloneURL = %s", got)
	}
	other, _ := parseRepo("https://github.com/acme/app")
	var refused *sandbox.ErrRefused
	if _, err := other.on(web); !errors.As(err, &refused) {
		t.Errorf("a repository on another host was not refused: %v", err)
	}
}

func TestGitHubWeb(t *testing.T) {
	for api, want := range map[string]string{
		"https://api.github.com":        "https://github.com",
		"https://ghe.example.ch/api/v3": "https://ghe.example.ch",
	} {
		u, _ := url.Parse(api)
		if got := githubWeb(u).String(); got != want {
			t.Errorf("githubWeb(%s) = %s, want %s", api, got, want)
		}
	}
}

func testKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key, pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

// checkJWT verifies an App JWT the way GitHub would.
func checkJWT(t *testing.T, key *rsa.PublicKey, header string) map[string]any {
	t.Helper()
	token := strings.TrimPrefix(header, "Bearer ")
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", header)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("the JWT's signature does not verify: %v", err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

func TestGitHubMint(t *testing.T) {
	key, pemKey := testKey(t)
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	var asked map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims := checkJWT(t, &key.PublicKey, r.Header.Get("Authorization"))
		if claims["iss"] != float64(1234) {
			t.Errorf("iss = %v, want the App id as a number", claims["iss"])
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /repos/acme/app/installation":
			_, _ = io.WriteString(w, `{"id": 99}`)
		case "GET /repos/acme/missing/installation":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message": "Not Found"}`)
		case "POST /app/installations/99/access_tokens":
			if err := json.NewDecoder(r.Body).Decode(&asked); err != nil {
				t.Error(err)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token": "ghs_secret", "expires_at": expires,
			})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer srv.Close()

	gh, err := NewGitHub(GitHubOptions{API: srv.URL, AppID: "1234", Key: pemKey})
	if err != nil {
		t.Fatal(err)
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	cred, err := gh.Mint(context.Background(), sandbox.GitRequest{
		Repo: "git@" + strings.Split(host, ":")[0] + ":acme/app.git", Until: time.Now().Add(8 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cred.Token != "ghs_secret" || cred.Username != "x-access-token" || cred.ID != "" {
		t.Errorf("credential = %+v", cred)
	}
	if !cred.Expires.Equal(expires) {
		t.Errorf("expires = %s, want %s", cred.Expires, expires)
	}
	if cred.Repo != srv.URL+"/acme/app.git" {
		t.Errorf("an ssh address was not rewritten to https: %s", cred.Repo)
	}
	// One repository, and only its contents.
	if repos, _ := asked["repositories"].([]any); len(repos) != 1 || repos[0] != "app" {
		t.Errorf("repositories = %v", asked["repositories"])
	}
	if perms, _ := asked["permissions"].(map[string]any); len(perms) != 1 || perms["contents"] != "write" {
		t.Errorf("permissions = %v", asked["permissions"])
	}

	_, err = gh.Mint(context.Background(), sandbox.GitRequest{Repo: srv.URL + "/acme/missing"})
	var refused *sandbox.ErrRefused
	if !errors.As(err, &refused) || !strings.Contains(refused.Reason, "not installed") {
		t.Errorf("a repository without the App = %v, want a refusal saying so", err)
	}
}

func TestGitLabMintAndRevoke(t *testing.T) {
	var created map[string]any
	var revoked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Private-Token") != "glpat-admin" {
			t.Errorf("the deployment's token was not sent")
		}
		switch r.Method + " " + r.URL.EscapedPath() {
		case "POST /api/v4/projects/group%2Fsub%2Fapp/access_tokens":
			if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
				t.Error(err)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id": 42, "token": "glpat-sandbox"}`)
		case "POST /api/v4/projects/group%2Fdenied/access_tokens":
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"message": "403 Forbidden"}`)
		case "DELETE /api/v4/projects/group%2Fsub%2Fapp/access_tokens/42":
			revoked = "42"
			w.WriteHeader(http.StatusNoContent)
		case "DELETE /api/v4/projects/group%2Fsub%2Fapp/access_tokens/43":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.EscapedPath())
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer srv.Close()

	gl, err := NewGitLab(GitLabOptions{URL: srv.URL, Token: "glpat-admin\n"})
	if err != nil {
		t.Fatal(err)
	}
	until := time.Date(2026, 9, 24, 17, 30, 0, 0, time.UTC)
	cred, err := gl.Mint(context.Background(), sandbox.GitRequest{
		Repo: srv.URL + "/group/sub/app.git", Until: until, Sandbox: "fix-login",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cred.Token != "glpat-sandbox" || cred.ID != "42" || cred.Repo != srv.URL+"/group/sub/app.git" {
		t.Errorf("credential = %+v", cred)
	}
	// The first day GitLab accepts that still covers the sandbox.
	if created["expires_at"] != "2026-09-25" || !cred.Expires.Equal(time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("expires_at = %v, expires = %s", created["expires_at"], cred.Expires)
	}
	if created["access_level"] != float64(30) || created["name"] != "keera sandbox fix-login" {
		t.Errorf("token asked for = %v", created)
	}

	_, err = gl.Mint(context.Background(), sandbox.GitRequest{Repo: srv.URL + "/group/denied", Until: until})
	var refused *sandbox.ErrRefused
	if !errors.As(err, &refused) || !strings.Contains(refused.Reason, "Maintainer") {
		t.Errorf("a project the token may not use = %v, want a refusal saying why", err)
	}

	if err := gl.Revoke(context.Background(), cred.Repo, cred.ID); err != nil || revoked != "42" {
		t.Errorf("Revoke = %v, revoked %q", err, revoked)
	}
	if err := gl.Revoke(context.Background(), cred.Repo, "43"); err != nil {
		t.Errorf("revoking a token already gone = %v, want nil", err)
	}
	if err := gl.Revoke(context.Background(), cred.Repo, "../x"); err == nil {
		t.Error("a token id that is not a number was sent")
	}
}

func TestDayAfter(t *testing.T) {
	zurich := time.FixedZone("CEST", 2*3600)
	for in, want := range map[time.Time]string{
		time.Date(2026, 9, 24, 17, 0, 0, 0, time.UTC): "2026-09-25",
		time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC):  "2026-09-25",
		// 01:00 in Zurich is still the day before in UTC.
		time.Date(2026, 9, 25, 1, 0, 0, 0, zurich): "2026-09-25",
	} {
		if got := dayAfter(in).Format(time.DateOnly); got != want {
			t.Errorf("dayAfter(%s) = %s, want %s", in, got, want)
		}
	}
}
