package httpx

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A request id is echoed on the response, written to the log and quoted in the
// message a 500 carries. Honouring the client's own is what lets their report
// and an operator's log line name the same request; holding it to the shape of
// an id is what keeps it from being a place to put something else.
func TestRequestIDAcceptsAnIDAndNothingElse(t *testing.T) {
	tests := []struct {
		name, presented string
		kept            bool
	}{
		{"one of ours", "req_06c1k2rt8g3m4n5p6q7r8s9t0v", true},
		{"a uuid", "3f2504e0-4f89-11d3-9a0c-0305e82c3301", true},
		{"a w3c trace id", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", true},
		{"a dotted id", "ingress.7.2026-09-06", true},
		{"nothing", "", false},
		{"a header split", "req_1\r\nX-Keera-Model: other", false},
		{"a newline", "req_1\nreq_2", false},
		{"a space", "req 1", false},
		{"an html tag", "<img src=x>", false},
		{"longer than an id", strings.Repeat("a", maxRequestIDLen+1), false},
		{"exactly as long as one may be", strings.Repeat("a", maxRequestIDLen), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := requestID(tc.presented)
			if (got != "") != tc.kept || (tc.kept && got != tc.presented) {
				t.Errorf("requestID(%q) = %q, want kept = %v", tc.presented, got, tc.kept)
			}
		})
	}
}

func TestMiddlewareMintsAnIDRatherThanRepeatingSomethingElse(t *testing.T) {
	var seen string
	h := Middleware(slog.New(slog.DiscardHandler),
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			seen = RequestID(r.Context())
		}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.Header.Set("X-Request-Id", "not an id\nat all")
	h.ServeHTTP(w, r)

	if got := w.Header().Get("X-Request-Id"); got != seen {
		t.Errorf("the response says %q and the handler saw %q; they have to be "+
			"the same request", got, seen)
	}
	if !strings.HasPrefix(seen, "req_") {
		t.Errorf("request id = %q, want one of ours", seen)
	}
}

func TestMiddlewareKeepsTheClientsOwnID(t *testing.T) {
	h := Middleware(slog.New(slog.DiscardHandler),
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.Header.Set("X-Request-Id", "req_from-the-ingress")
	h.ServeHTTP(w, r)

	if got := w.Header().Get("X-Request-Id"); got != "req_from-the-ingress" {
		t.Errorf("X-Request-Id = %q; an id an ingress already assigned has to "+
			"survive, or nothing correlates", got)
	}
}
