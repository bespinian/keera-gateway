// Package httpx holds the HTTP plumbing shared by the inference and control
// planes, which are served from one listener and told apart by path.
package httpx

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"time"

	"github.com/bespinian/keera-gateway/internal/id"
)

// The one listener is split by path: the panel at the root, and each API under
// its prefix. They live here so the routes, the panel and the `keera` CLI agree
// without the CLI linking the whole server.
//
// InferencePrefix comes before the version because clients append /v1/...
// themselves: an OpenAI SDK at <origin>/api/v1 and an Anthropic one at
// <origin>/api both work unedited.
//
// SandboxPrefix carries a byte stream to a port inside a sandbox over an HTTP
// upgrade, so attaching needs no second port or certificate.
const (
	InferencePrefix = "/api"
	ControlPrefix   = "/control"
	SandboxPrefix   = "/sandbox"
)

type ctxKey int

const requestIDKey ctxKey = iota

// RequestID returns the id assigned to r, for logs and for correlating a client
// report with a usage row.
func RequestID(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}

// APIError is the error envelope. It matches the OpenAI shape, because that is
// what the clients parse; anything else reaches a developer as a blank failure.
type APIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Param   string `json:"param,omitempty"`
}

// Flag reads a boolean query parameter. Only "1" counts, so every switch on
// this API is spelled the same way.
func Flag(q url.Values, name string) bool { return q.Get(name) == "1" }

// WriteError sends an error envelope.
func WriteError(w http.ResponseWriter, status int, typ, code, msg string) {
	WriteJSON(w, status, map[string]APIError{
		"error": {Message: msg, Type: typ, Code: code},
	})
}

// WriteJSON sends v as JSON.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// ReadJSON decodes a request body, rejecting unknown fields so a typo in a
// control-plane call is an error rather than a silently ignored setting.
func ReadJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// statusRecorder remembers the status code for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status, s.wrote = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.status, s.wrote = http.StatusOK, true
	}
	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.NewResponseController reach the real writer, so Flush works
// through this wrapper for streaming.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// maxRequestIDLen bounds an id adopted from a client: long enough for a UUID or
// a W3C trace id, short enough to repeat safely.
const maxRequestIDLen = 64

// requestID accepts a client's own request id, or returns "".
//
// The id is echoed on the response and written to the log, so a client's
// report and the log name the same request. As the caller chose it, it must
// look like an id, with nothing a log or header has to escape. Anything else
// is replaced, not repaired.
func requestID(presented string) string {
	if presented == "" || len(presented) > maxRequestIDLen {
		return ""
	}
	for i := range len(presented) {
		switch c := presented[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == ':':
		default:
			return ""
		}
	}
	return presented
}

// Middleware assigns a request id, logs the outcome, and turns a panic into a
// 500 rather than a dropped connection.
func Middleware(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := requestID(r.Header.Get("X-Request-Id"))
		if rid == "" {
			rid = id.New("req")
		}
		w.Header().Set("X-Request-Id", rid)
		// The panel and the APIs share one origin, so a body a browser sniffs
		// as HTML would run where the session cookie lives. Every route states
		// its own media type.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey, rid))

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()

		defer func() {
			if p := recover(); p != nil {
				log.Error("panic serving request", "error", p, "request_id", rid,
					"path", r.URL.Path, "stack", string(debug.Stack()))
				if !rec.wrote {
					WriteError(rec, http.StatusInternalServerError, "server_error", "", "internal error")
				}
			}
			log.Debug("request", "method", r.Method, "path", r.URL.Path,
				"status", rec.status, "duration", time.Since(start), "request_id", rid)
		}()

		next.ServeHTTP(rec, r)
	})
}
