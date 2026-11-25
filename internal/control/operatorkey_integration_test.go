package control

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/secret"
	"github.com/bespinian/keera-gateway/internal/store"
)

// Changing a leaked operator key must end the panel sessions it signed in,
// and the tokens apps were handed for them.
func TestChangingTheOperatorKeyEndsItsSessions(t *testing.T) {
	const chat = "https://chat.example.ch"
	st, ctx := testStore(t)
	if _, err := st.CreateOrg(ctx, store.Org{ID: "org_1", Name: "Example Bank"}, store.OrgTemplate{}); err != nil {
		t.Fatalf("creating the organisation: %v", err)
	}
	box, err := secret.New("a-secret-key-long-enough-to-be-real")
	if err != nil {
		t.Fatal(err)
	}
	server := func(operatorKey string) http.Handler {
		return New(st, nil, nil, nil, Options{OperatorKey: operatorKey, Secrets: box, ChatURL: chat},
			slog.New(slog.DiscardHandler)).Handler()
	}
	old := server(testOperatorKey)

	r := httptest.NewRequest(http.MethodPost, httpx.ControlPrefix+"/auth/local",
		strings.NewReader(`{"key":"`+testOperatorKey+`"}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	old.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("signing in with the operator key: status = %d: %s", w.Code, w.Body)
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie")
	}

	me := func(h http.Handler) int {
		r := httptest.NewRequest(http.MethodGet, httpx.ControlPrefix+"/v1/me", nil)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if got := me(old); got != http.StatusOK {
		t.Fatalf("/v1/me with the same key: status = %d, want 200", got)
	}

	// An app is handed the sign-in, as for anyone signed in to the panel.
	verifier := authn.NewCLIVerifier()
	q := url.Values{
		"cli_redirect":  {chat + AppCallbackPath},
		"cli_challenge": {authn.CLIChallenge(verifier)},
		"cli_state":     {"state-1"},
	}
	r = httptest.NewRequest(http.MethodGet, httpx.ControlPrefix+"/auth/login?"+q.Encode(), nil)
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	old.ServeHTTP(w, r)
	to, err := url.Parse(w.Header().Get("Location"))
	if err != nil || !strings.HasPrefix(to.String(), chat+AppCallbackPath+"?") {
		t.Fatalf("the operator key's session was not handed to the app: %d %s", w.Code, to)
	}
	r = httptest.NewRequest(http.MethodPost, httpx.ControlPrefix+"/auth/cli/token",
		strings.NewReader(`{"code":"`+to.Query().Get("code")+`","verifier":"`+verifier+`"}`))
	r.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	old.ServeHTTP(w, r)
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(w.Body).Decode(&tok); err != nil || tok.Token == "" {
		t.Fatalf("redeeming the code: status = %d, %v", w.Code, err)
	}
	asApp := func(h http.Handler) int {
		r := httptest.NewRequest(http.MethodGet, httpx.ControlPrefix+"/v1/me", nil)
		r.Header.Set("Authorization", "Bearer "+tok.Token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if got := asApp(old); got != http.StatusOK {
		t.Fatalf("/v1/me with the app's token: status = %d, want 200", got)
	}

	changed := server("a-new-operator-key-long-enough")
	if got := asApp(changed); got != http.StatusUnauthorized {
		t.Errorf("/v1/me with the app's token after the key changed: status = %d, want 401", got)
	}
	if got := me(changed); got != http.StatusUnauthorized {
		t.Errorf("/v1/me after the key changed: status = %d, want 401", got)
	}
}
