package control

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-ldap/ldap/v3"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/authn/ldaptest"
	"github.com/bespinian/keera-gateway/internal/httpx"
)

// passwordServer is a control server with no store, in front of one LDAP
// directory called corp. A refused password never reaches the store.
func passwordServer(t *testing.T) http.Handler {
	t.Helper()
	alice := ldaptest.Person("alice", "alice@corp.example.ch", "5b0f4c2e-1d3a-4f6b-9c8d-7e6f5a4b3c2d")
	dir := &ldaptest.Directory{
		Entries:   []*ldap.Entry{alice},
		Passwords: map[string]string{alice.DN: "right"},
	}
	corp := ldaptest.New(t, ldaptest.Config("corp"), dir)
	return New(nil, nil, nil, nil, Options{
		OperatorKey: testOperatorKey, Providers: authn.Providers{corp},
	}, slog.New(slog.DiscardHandler)).Handler()
}

func postPassword(h http.Handler, addr, contentType, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, httpx.ControlPrefix+"/auth/password",
		strings.NewReader(body))
	r.Header.Set("Content-Type", contentType)
	r.RemoteAddr = addr
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAWrongPasswordIsRefusedWithoutSayingWhy(t *testing.T) {
	h := passwordServer(t)
	for _, user := range []string{"alice", "nobody"} {
		w := postPassword(h, "198.51.100.7:5000", "application/json",
			`{"provider":"corp","username":"`+user+`","password":"wrong"}`)
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), passwordRefused) {
			t.Errorf("%s: %d %s, want 401 with the one message", user, w.Code, w.Body)
		}
	}
}

func TestAPasswordSignInMustBeJSON(t *testing.T) {
	// A cross-site form can post urlencoded, so that would let another site
	// sign a browser in to an account of its choosing.
	w := postPassword(passwordServer(t), "198.51.100.7:5000", "application/x-www-form-urlencoded",
		"provider=corp&username=alice&password=right")
	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", w.Code)
	}
}

func TestAPasswordSignInNamesAPasswordProvider(t *testing.T) {
	w := postPassword(passwordServer(t), "198.51.100.7:5000", "application/json",
		`{"provider":"google","username":"alice","password":"right"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestPasswordsAreThrottledPerAccount(t *testing.T) {
	h := passwordServer(t)
	// Each attempt from another address, so only the account's throttle
	// applies.
	attempt := func(i int, user string) int {
		return postPassword(h, fmt.Sprintf("198.51.100.%d:5000", i), "application/json",
			`{"provider":"corp","username":"`+user+`","password":"wrong"}`).Code
	}
	for i := range passwordPerAccountRPM {
		if got := attempt(i, "alice"); got != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i, got)
		}
	}
	if got := attempt(100, " ALICE "); got != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 for the same account spelled another way", got)
	}
	if got := attempt(101, "bob"); got != http.StatusUnauthorized {
		t.Errorf("another account got %d, want 401", got)
	}
}

func TestPasswordsAreThrottledPerAccountUnderAnyUsername(t *testing.T) {
	h := passwordServer(t)
	// alice may type her uid or her address. Both are one account.
	users := []string{"alice", "alice@corp.example.ch"}
	for i := range passwordPerAccountRPM {
		w := postPassword(h, fmt.Sprintf("198.51.100.%d:5000", i), "application/json",
			`{"provider":"corp","username":"`+users[i%2]+`","password":"wrong"}`)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i, w.Code)
		}
	}
	w := postPassword(h, "198.51.100.100:5000", "application/json",
		`{"provider":"corp","username":"alice@corp.example.ch","password":"wrong"}`)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 for the account under its other username", w.Code)
	}
}

func TestAnUnusableEntryIsNotDescribedToThePerson(t *testing.T) {
	carol := ldaptest.Person("carol", "", "11111111-2222-3333-4444-555555555555")
	dir := &ldaptest.Directory{
		Entries:   []*ldap.Entry{carol},
		Passwords: map[string]string{carol.DN: "right"},
	}
	corp := ldaptest.New(t, ldaptest.Config("corp"), dir)
	h := New(nil, nil, nil, nil, Options{
		OperatorKey: testOperatorKey, Providers: authn.Providers{corp},
	}, slog.New(slog.DiscardHandler)).Handler()
	w := postPassword(h, "198.51.100.7:5000", "application/json",
		`{"provider":"corp","username":"carol","password":"right"}`)
	if w.Code != http.StatusForbidden || strings.Contains(strings.ToLower(w.Body.String()), "ou=people") {
		t.Errorf("%d %s, want 403 that does not name the entry", w.Code, w.Body)
	}
}

func TestTheFormIsToldWhereASignInGoesNext(t *testing.T) {
	const chat = "https://chat.example.ch"
	for _, tc := range []struct {
		redirect, want string
	}{
		{"", ""},
		{"http://127.0.0.1:53682/callback", ".cli"},
		{chat + ChatCallbackPath, ".chat"},
	} {
		if got := (cliLogin{Redirect: tc.redirect}).panelHint(chat); got != tc.want {
			t.Errorf("%q: hint = %q, want %q", tc.redirect, got, tc.want)
		}
	}
}

// With a passkey as a second way in, a link that names no provider is refused,
// as `keera login` refuses it, rather than sent to the directory.
func TestASignInNamesAProviderBesidePasskeys(t *testing.T) {
	corp := ldaptest.New(t, ldaptest.Config("corp"), &ldaptest.Directory{})
	rp, err := authn.NewRelyingParty("https://keera.example.ch")
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil, nil, nil, nil, Options{
		OperatorKey: testOperatorKey,
		Providers:   authn.Providers{corp},
	}, slog.New(slog.DiscardHandler))
	if p, err := s.providerFor(""); err != nil || p != corp {
		t.Errorf("one directory: %v, %v; want it chosen", p, err)
	}
	s.opts.Passkeys = rp
	if _, err := s.providerFor(""); err == nil || !strings.Contains(err.Error(), "corp, passkey") {
		t.Errorf("a directory and passkeys: %v, want both named", err)
	}
}

func TestAnOverlongUsernameGetsNoBucket(t *testing.T) {
	// Each such bucket would hold the name in memory for minutes. Refused
	// before the directory is asked, the name needs no throttle of its own.
	h := passwordServer(t)
	user := strings.Repeat("a", authn.LDAPUsernameMax+1)
	for i := range passwordPerAccountRPM + 1 {
		w := postPassword(h, fmt.Sprintf("198.51.100.%d:5000", i), "application/json",
			`{"provider":"corp","username":"`+user+`","password":"wrong"}`)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i, w.Code)
		}
	}
}
