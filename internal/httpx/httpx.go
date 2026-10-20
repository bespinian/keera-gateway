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
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/id"
)

// The one listener is split by path: the panel at the root, and each API under
// its prefix. They live here so the routes, the panel and the `keera` CLI agree
// on them in one place.
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

// SessionHeader is the header a client can name its own session in. It is
// here, not in the gateway, because the sandboxes set it for the agents they
// run. The value is hashed, so it is never stored as sent.
//
// A derived session is a guess; the header groups a client exactly, even when
// it rewrites its history or restarts a task with the same words.
const SessionHeader = "X-Keera-Session"

type ctxKey int

const (
	requestIDKey ctxKey = iota
	clientRequestIDKey
)

// RequestID returns the id assigned to r, for logs and for correlating a client
// report with a usage row.
func RequestID(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}

// ClientRequestIDHeader is OpenAI's header for an id the client picks itself.
// Unlike X-Request-Id, the client knows it even when no answer comes back.
const ClientRequestIDHeader = "X-Client-Request-Id"

// ClientRequestID returns the id the client sent in ClientRequestIDHeader, or
// "" when it sent none or one that was not valid.
func ClientRequestID(ctx context.Context) string {
	v, _ := ctx.Value(clientRequestIDKey).(string)
	return v
}

// LogIDs names the request in a log line: by its id, and by the client's own
// when it sent one.
func LogIDs(ctx context.Context) slog.Attr {
	rid := slog.String("request_id", RequestID(ctx))
	if cid := ClientRequestID(ctx); cid != "" {
		// A group without a key is written as its fields, side by side.
		return slog.Group("", rid, slog.String("client_request_id", cid))
	}
	return rid
}

// APIError is the error envelope. It matches the OpenAI shape, because that is
// what the clients parse; anything else reaches a developer as a blank failure.
type APIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

// MediaType is a Content-Type without its parameters, in lower case, since
// media types compare without regard to case.
func MediaType(ct string) string {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.ToLower(strings.TrimSpace(ct))
}

// Flag reads a boolean query parameter. Only "1" counts, so every switch on
// this API is spelled the same way.
func Flag(q url.Values, name string) bool { return q.Get(name) == "1" }

func WriteError(w http.ResponseWriter, status int, typ, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(ErrorBody(typ, code, msg))
}

// ErrorBody is the error envelope, encoded.
func ErrorBody(typ, code, msg string) []byte {
	body, _ := json.Marshal(map[string]APIError{"error": {Message: msg, Type: typ, Code: code}})
	return body
}

// WriteNotFound answers a route or model that is not there, for a path both
// OpenAI and Anthropic clients call. It is in both shapes at once: both read
// error.message, and Anthropic ones the type beside it.
func WriteNotFound(w http.ResponseWriter, code, msg string) {
	WriteJSON(w, http.StatusNotFound, map[string]any{
		"type":  "error",
		"error": APIError{Message: msg, Type: "not_found_error", Code: code},
	})
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// SetRetryAfter says when to try again, in whole seconds. It is never zero,
// which a client would read as "now" and retry at once.
func SetRetryAfter(w http.ResponseWriter, d time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(max(int(d.Seconds()), 1)))
}

// MaxJSONBody is the largest body ReadJSON takes. A longer one fails with an
// *http.MaxBytesError, which the caller answers with 413.
const MaxJSONBody = 1 << 20

// ReadJSON decodes a request body, rejecting unknown fields so a typo in a
// control-plane call is an error rather than a silently ignored setting.
func ReadJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, MaxJSONBody))
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
// The id is echoed on the response and logged, so a client's report and the
// log name the same request. As the caller chose it, it must look like an id,
// with nothing a log or header has to escape. Anything else is replaced, not
// repaired.
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

// maxClientRequestIDLen is the longest X-Client-Request-Id OpenAI accepts.
const maxClientRequestIDLen = 512

// clientRequestID accepts a client's X-Client-Request-Id, or returns "".
//
// OpenAI takes any ASCII up to 512 characters and refuses the request
// otherwise. Here an invalid one is dropped instead, as most models behind the
// gateway never read it. Control characters are dropped too, since the id is
// written to the log and sent on as a header.
func clientRequestID(presented string) string {
	if len(presented) > maxClientRequestIDLen {
		return ""
	}
	for i := range len(presented) {
		if c := presented[i]; c < ' ' || c > '~' {
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
		ctx := context.WithValue(r.Context(), requestIDKey, rid)
		if cid := clientRequestID(r.Header.Get(ClientRequestIDHeader)); cid != "" {
			ctx = context.WithValue(ctx, clientRequestIDKey, cid)
		}
		r = r.WithContext(ctx)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()

		defer func() {
			if p := recover(); p != nil {
				log.Error("panic serving request", "error", p, LogIDs(r.Context()),
					"path", r.URL.Path, "stack", string(debug.Stack()))
				if !rec.wrote {
					WriteError(rec, http.StatusInternalServerError, "server_error", "", "internal error")
				}
			}
			log.Debug("request", "method", r.Method, "path", r.URL.Path,
				"status", rec.status, "duration", time.Since(start), LogIDs(r.Context()))
		}()

		next.ServeHTTP(rec, r)
	})
}
