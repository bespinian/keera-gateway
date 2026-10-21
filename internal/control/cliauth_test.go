package control

import (
	"net/url"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/authn"
)

// Where a command-line sign-in comes back to decides where a credential is
// delivered, and anybody can write that URL. Only a loopback listener survives.
func TestOnlyALoopbackListenerCanReceiveASignIn(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{"http://127.0.0.1:1234/callback", "http://127.0.0.1:1234/callback"},
		{"http://127.0.0.1:9/", "http://127.0.0.1:9/"},
		{"http://[::1]:1234/callback", "http://[::1]:1234/callback"},
		// A caller's query is dropped: the code and state are appended here,
		// and must not sit next to whatever somebody else put in the URL.
		{"http://127.0.0.1:1234/callback?next=%2Fevil", "http://127.0.0.1:1234/callback"},

		// Not loopback: the one-time code would go to a host somebody else
		// names.
		{"http://evil.example:1234/callback", ""},
		{"http://10.0.0.5:1234/callback", ""},
		{"http://169.254.169.254/callback", ""},
		// Not even by name: "localhost" may resolve somewhere else.
		{"http://localhost:1234/callback", ""},
		// Without a port there is no listener.
		{"http://127.0.0.1/callback", ""},
		// Another scheme is another program.
		{"https://127.0.0.1:1234/callback", ""},
		{"file:///etc/passwd", ""},
		{"javascript:alert(1)", ""},
		// Credentials in the authority make a host look like a path.
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

// Keera Chat takes sign-ins back at one exact address, and only on a
// deployment that names it. Anything near it is still refused, so the rule
// stays "loopback, or that one address".
func TestKeeraChatCanReceiveASignInWhereTheOperatorNamedIt(t *testing.T) {
	const chat = "https://chat.example.ch"
	for _, tc := range []struct {
		raw     string
		chatURL string
		ok      bool
	}{
		{"https://chat.example.ch/auth/callback", chat, true},
		// Loopback still works beside it.
		{"http://127.0.0.1:1234/callback", chat, true},

		// Not named: the gateway does not know this chat.
		{"https://chat.example.ch/auth/callback", "", false},
		// Near misses: another path, a query, another host, another scheme.
		{"https://chat.example.ch/auth/callback/", chat, false},
		{"https://chat.example.ch/other", chat, false},
		{"https://chat.example.ch/auth/callback?next=x", chat, false},
		{"https://chat.example.ch.evil.example/auth/callback", chat, false},
		{"http://chat.example.ch/auth/callback", chat, false},
		{"https://user@chat.example.ch/auth/callback", chat, false},
	} {
		got, err := handOverTo(tc.raw, tc.chatURL)
		if tc.ok != (err == nil) {
			t.Errorf("handOverTo(%q, %q) = %q, %v; want ok=%v", tc.raw, tc.chatURL, got, err, tc.ok)
		}
		if tc.ok && got == "" {
			t.Errorf("handOverTo(%q, %q) accepted it but returned no address", tc.raw, tc.chatURL)
		}
	}

	// A chat this gateway does not know gets a message an operator can act
	// on, without the address, which anyone can write into a link.
	_, err := handOverTo("https://evil.example/auth/callback", "")
	if err == nil || !strings.Contains(err.Error(), "KEERA_CHAT_URL") ||
		strings.Contains(err.Error(), "evil") {
		t.Errorf("unknown chat: %v", err)
	}
}

// Only a credential no browser holds may name Keera Chat as the client. A
// session cookie is the panel's own playground, whatever the page says.
func TestOnlyKeeraChatsSignInMayNameKeeraChat(t *testing.T) {
	for _, tc := range []struct {
		via    authn.Method
		stated string
		want   string
	}{
		{authn.MethodCLI, "keera-chat", "keera-chat"},
		{authn.MethodSession, "keera-chat", "keera-playground"},
		{authn.MethodCLI, "claude-code", "keera-playground"},
		{authn.MethodSession, "", "keera-playground"},
	} {
		if got := playgroundClient(&authn.Principal{Via: tc.via}, tc.stated); got != tc.want {
			t.Errorf("playgroundClient(%s, %q) = %q, want %q", tc.via, tc.stated, got, tc.want)
		}
	}
}

// A sign-in with a loopback port but no proof key could be finished by
// anything on that port. It is refused at the start, before the person types a
// password.
func TestAHalfWrittenHandshakeIsRefusedAtTheStart(t *testing.T) {
	good := url.Values{
		"cli_redirect":  {"http://127.0.0.1:1234/callback"},
		"cli_challenge": {strings.Repeat("a", 43)},
		"cli_state":     {"state"},
	}
	if got, err := cliLoginFrom(good, ""); err != nil || got.Redirect == "" {
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
			if got, err := cliLoginFrom(q, ""); err == nil {
				t.Errorf("cliLoginFrom(%v) = %+v, want it refused", q, got)
			}
		})
	}

	// The ordinary sign-in names none of them.
	got, err := cliLoginFrom(url.Values{"next": {"/projects"}}, "")
	if err != nil || got != (cliLogin{}) {
		t.Errorf("cliLoginFrom(panel sign-in) = %+v, %v; want an empty handshake", got, err)
	}
}

// The challenge is checked before a browser opens, not after the person has
// gone through the consent screen.
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
