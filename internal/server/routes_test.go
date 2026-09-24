package server

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bespinian/keera-gateway/internal/control"
	"github.com/bespinian/keera-gateway/internal/gateway"
	"github.com/bespinian/keera-gateway/internal/ratelimit"
)

const testOperatorKey = "an-operator-key-long-enough"

// The two planes are told apart only by path. Each case checks which handler
// answered: a 401 means a plane knew the route, a 404 that nothing did.
func TestRoutesSendEachPathToTheRightPlane(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	gw := gateway.New(nil, nil, ratelimit.New(), nil, nil, gateway.Options{}, log)
	ctl := control.New(nil, nil, nil, nil, control.Options{
		OperatorKey: testOperatorKey, ServeUI: true,
	}, log)
	h := routes(gw, ctl)

	tests := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		// The panel routes in the browser, so its paths return the shell.
		{"the panel is at the root", http.MethodGet, "/", http.StatusOK},
		{"a panel route is the shell too", http.MethodGet, "/teams", http.StatusOK},

		// The probes stay at the root.
		{"liveness", http.MethodGet, "/healthz", http.StatusOK},
		{"the exposition needs a credential", http.MethodGet, "/metrics", http.StatusUnauthorized},

		// The inference plane: a 401 shows the route exists.
		{"openai chat", http.MethodPost, "/api/v1/chat/completions", http.StatusUnauthorized},
		{"openai completions", http.MethodPost, "/api/v1/completions", http.StatusUnauthorized},
		{"openai embeddings", http.MethodPost, "/api/v1/embeddings", http.StatusUnauthorized},
		{"anthropic messages", http.MethodPost, "/api/v1/messages", http.StatusUnauthorized},
		{"the model catalogue clients read", http.MethodGet, "/api/v1/models", http.StatusUnauthorized},
		// Anthropic clients append /api/hello to a base URL that already
		// ends in /api, so the doubled path is what arrives.
		{"the connection-warming probe", http.MethodHead, "/api/api/hello", http.StatusOK},

		// The control plane shares /v1/models and differs only by prefix.
		{"the control model catalogue", http.MethodGet, "/control/v1/models", http.StatusUnauthorized},
		{"who am i", http.MethodGet, "/control/v1/me", http.StatusUnauthorized},
		{"the sign-in flow", http.MethodGet, "/control/auth/config", http.StatusOK},

		// The old two-port paths belong to the panel now.
		{"the inference plane is not at the root", http.MethodPost, "/v1/chat/completions", http.StatusMethodNotAllowed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(tt.method, tt.path, nil))
			if w.Code != tt.want {
				t.Errorf("%s %s = %d, want %d", tt.method, tt.path, w.Code, tt.want)
			}
		})
	}
}
