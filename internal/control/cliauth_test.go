package control

import (
	"net/url"
	"strings"
	"testing"
)

// Where a command-line sign-in comes back to is the one parameter that decides
// where a credential is delivered, and it arrives in a URL somebody can write.
// Only a listener on the machine the browser is on survives.
func TestOnlyALoopbackListenerCanReceiveASignIn(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{"http://127.0.0.1:1234/callback", "http://127.0.0.1:1234/callback"},
		{"http://127.0.0.1:9/", "http://127.0.0.1:9/"},
		{"http://[::1]:1234/callback", "http://[::1]:1234/callback"},
		// A query a caller wrote is dropped: the code and the state are
		// appended here, and reflecting one back would put whatever somebody
		// else put in the URL next to a credential.
		{"http://127.0.0.1:1234/callback?next=%2Fevil", "http://127.0.0.1:1234/callback"},

		// Not loopback. These are the whole point of the check: a redirect
		// carrying a one-time code to a host somebody else names is that code
		// delivered to them, at the moment the person is most likely to follow
		// it because they have just signed in successfully.
		{"http://evil.example:1234/callback", ""},
		{"http://10.0.0.5:1234/callback", ""},
		{"http://169.254.169.254/callback", ""},
		// Not even by name. What "localhost" resolves to is the machine's own
		// resolver's business, and on a host where somebody has pointed it
		// elsewhere this would be the case above wearing a friendly name.
		{"http://localhost:1234/callback", ""},
		// A port is what makes it a listener. Without one there is nothing on
		// the machine this could be.
		{"http://127.0.0.1/callback", ""},
		// Another scheme is another machine, or another program entirely.
		{"https://127.0.0.1:1234/callback", ""},
		{"file:///etc/passwd", ""},
		{"javascript:alert(1)", ""},
		// Credentials in the authority are a way of making a host look like a
		// path to a reader.
		{"http://127.0.0.1:1234@evil.example/callback", ""},
		{"", ""},
	} {
		got, err := loopbackRedirect(tc.raw)
		if tc.want == "" {
			if err == nil {
				t.Errorf("loopbackRedirect(%q) = %q, want it refused", tc.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("loopbackRedirect(%q) = %v, want %q", tc.raw, err, tc.want)
			continue
		}
		if got != tc.want {
			t.Errorf("loopbackRedirect(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// A sign-in that names a loopback port but no proof key is one anything
// listening on that port could finish. It is refused where it is started rather
// than where it comes back, so that the person is told before they have gone to
// a browser and typed a password.
func TestAHalfWrittenHandshakeIsRefusedAtTheStart(t *testing.T) {
	good := url.Values{
		"cli_redirect":  {"http://127.0.0.1:1234/callback"},
		"cli_challenge": {strings.Repeat("a", 43)},
		"cli_state":     {"state"},
	}
	if got, err := cliLoginFrom(good); err != nil || got.Redirect == "" {
		t.Fatalf("cliLoginFrom(complete) = %+v, %v; want it accepted", got, err)
	}

	for name, drop := range map[string]string{
		"no redirect":  "cli_redirect",
		"no challenge": "cli_challenge",
		"no state":     "cli_state",
	} {
		t.Run(name, func(t *testing.T) {
			q := url.Values{}
			for k, v := range good {
				if k != drop {
					q[k] = v
				}
			}
			if got, err := cliLoginFrom(q); err == nil {
				t.Errorf("cliLoginFrom(%v) = %+v, want it refused", q, got)
			}
		})
	}

	// And the ordinary sign-in, which names none of them and is not one of
	// these at all.
	got, err := cliLoginFrom(url.Values{"next": {"/teams"}})
	if err != nil || got != (cliLogin{}) {
		t.Errorf("cliLoginFrom(panel sign-in) = %+v, %v; want an empty handshake", got, err)
	}
}

// The challenge is checked for shape before a browser is opened, because the
// alternative is a sign-in that fails five minutes later - at the one moment
// the person has left the terminal and is looking at a consent screen.
func TestAChallengeThatCouldNeverMatchIsRefusedEarly(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want bool
	}{
		{"an S256 challenge", strings.Repeat("a", 43), true},
		{"every character base64url allows", strings.Repeat("-_09azAZ", 5) + "abc", true},
		{"empty", "", false},
		{"too short", strings.Repeat("a", 42), false},
		{"too long", strings.Repeat("a", 44), false},
		{"padded, so not raw base64url", strings.Repeat("a", 42) + "=", false},
		{"standard base64's alphabet", strings.Repeat("a", 41) + "+/", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validChallenge(tc.in); got != tc.want {
				t.Errorf("validChallenge(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
