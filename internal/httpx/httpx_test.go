package httpx

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A request id is echoed on the response, logged and quoted in a 500. Honouring
// the client's own lets their report and the log name the same request; holding
// it to the shape of an id keeps it from being a place to put something else.
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

// OpenAI takes any ASCII up to 512 characters as X-Client-Request-Id. The id
// is also logged and sent on as a header, so control characters are dropped.
func TestClientRequestIDTakesWhatOpenAITakes(t *testing.T) {
	tests := []struct {
		name, presented string
		kept            bool
	}{
		{"a uuid", "3f2504e0-4f89-11d3-9a0c-0305e82c3301", true},
		{"a trace id with a space", "batch 7/job 12", true},
		{"exactly as long as one may be", strings.Repeat("a", maxClientRequestIDLen), true},
		{"longer than OpenAI takes", strings.Repeat("a", maxClientRequestIDLen+1), false},
		{"a header split", "id\r\nX-Keera-Model: other", false},
		{"a tab", "id\t1", false},
		{"not ascii", "réquête", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := clientRequestID(tc.presented)
			if (got != "") != tc.kept || (tc.kept && got != tc.presented) {
				t.Errorf("clientRequestID(%q) = %q, want kept = %v", tc.presented, got, tc.kept)
			}
		})
	}
}

// The client's own id sits next to the request id, so a log line can be found
// by either. It never replaces the request id.
func TestTheClientsRequestIDIsLoggedBesideOurs(t *testing.T) {
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := Middleware(log, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.Header.Set("X-Request-Id", "req_from-the-ingress")
	r.Header.Set(ClientRequestIDHeader, "job-12")
	h.ServeHTTP(w, r)

	line := buf.String()
	for _, want := range []string{"request_id=req_from-the-ingress", "client_request_id=job-12"} {
		if !strings.Contains(line, want) {
			t.Errorf("log line %q lacks %s", line, want)
		}
	}
	if got := w.Header().Get("X-Request-Id"); got != "req_from-the-ingress" {
		t.Errorf("X-Request-Id = %q, want the ingress's id kept", got)
	}
}

// Media types compare without regard to case or parameters, so a server that
// answers Application/JSON is read as JSON.
func TestMediaType(t *testing.T) {
	for in, want := range map[string]string{
		"application/json":                 "application/json",
		"Application/JSON; charset=utf-8":  "application/json",
		" text/event-stream ;charset=utf8": "text/event-stream",
		"":                                 "",
	} {
		if got := MediaType(in); got != want {
			t.Errorf("MediaType(%q) = %q, want %q", in, got, want)
		}
	}
}
