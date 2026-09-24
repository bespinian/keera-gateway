package httpx

import (
	"bytes"
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serve runs one request through Compress around a handler that writes what it
// is told, and returns the recorded response.
func serve(t *testing.T, accept, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := Compress(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = io.WriteString(w, body)
	}))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if accept != "" {
		r.Header.Set("Accept-Encoding", accept)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// unzip reads a gzipped response body back, failing the test if it is not one.
func unzip(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatalf("response is not gzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("reading the gzip body: %v", err)
	}
	return string(out)
}

func TestALargeJSONResponseIsCompressed(t *testing.T) {
	body := `{"rows":[` + strings.Repeat(`{"alias":"keera-speed","status":200},`, 200) + `]}`
	w := serve(t, "gzip", "application/json", body)

	if got := w.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if unzip(t, w) != body {
		t.Error("the decompressed body is not what the handler wrote")
	}
	if w.Body.Len() >= len(body) {
		t.Errorf("compressed to %d bytes from %d, which is no saving",
			w.Body.Len(), len(body))
	}
	// A length describing the uncompressed body would be read as the length of
	// this one, and the client would stop short or hang waiting for the rest.
	if w.Header().Get("Content-Length") != "" {
		t.Error("Content-Length survived compression")
	}
}

func TestASmallResponseIsSentAsItIs(t *testing.T) {
	const body = `{"status":"ok"}`
	w := serve(t, "gzip", "application/json", body)

	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want none: the gzip header would be most of it", got)
	}
	if w.Body.String() != body {
		t.Errorf("body = %q, want %q", w.Body.String(), body)
	}
}

func TestAClientThatDidNotAskIsNotSentGzip(t *testing.T) {
	body := strings.Repeat("keera ", 1000)
	w := serve(t, "", "application/json", body)

	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q on a client that sent no Accept-Encoding", got)
	}
	if w.Body.String() != body {
		t.Error("the body was altered for a client that asked for no encoding")
	}
	// The resource still varies even though this response did not: a cache that
	// was told otherwise would hand this answer to a client that did ask.
	if !strings.Contains(w.Header().Get("Vary"), "Accept-Encoding") {
		t.Error("Vary is missing, so a shared cache cannot tell the two apart")
	}
}

func TestAStreamIsNotHeldInTheCompressor(t *testing.T) {
	// The request log's stream: events written and flushed one at a time, with
	// the reader expected to see each one as it happens.
	seen := make(chan string, 4)
	h := Compress(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		for _, ev := range []string{"data: one\n\n", "data: two\n\n"} {
			_, _ = io.WriteString(w, ev)
			if err := rc.Flush(); err != nil {
				t.Errorf("Flush: %v", err)
			}
			seen <- ev
		}
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(w, r)
	close(seen)

	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q: a stream must not be compressed", got)
	}
	if got := w.Body.String(); got != "data: one\n\ndata: two\n\n" {
		t.Errorf("the stream arrived as %q", got)
	}
	if len(seen) != 2 {
		t.Errorf("%d events were flushed, want 2", len(seen))
	}
}

func TestAnAlreadyEncodedResponsePassesThrough(t *testing.T) {
	// What the panel's embedded assets do: compressed once at startup, served
	// with their own Content-Encoding. Compressing them again per request is
	// the whole thing this must not do.
	var squeezed bytes.Buffer
	zw := gzip.NewWriter(&squeezed)
	_, _ = io.WriteString(zw, strings.Repeat("export const view = () => {};\n", 200))
	_ = zw.Close()
	want := squeezed.Bytes()

	h := Compress(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(want)
	}))
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(w, r)

	if !bytes.Equal(w.Body.Bytes(), want) {
		t.Error("an already-gzipped body was re-encoded on its way out")
	}
}

func TestAStatusWithNoBodyIsStillSent(t *testing.T) {
	h := Compress(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotModified)
	}))
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(w, r)

	if w.Code != http.StatusNotModified {
		t.Errorf("status = %d, want 304", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Error("a 304 must carry no body")
	}
}

func TestTheErrorEnvelopeSurvivesCompression(t *testing.T) {
	// Every refusal the control plane writes goes through WriteJSON, and a
	// client that cannot parse one is a developer looking at a blank failure.
	h := Compress(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		WriteError(w, http.StatusForbidden, "invalid_request_error", "csrf",
			strings.Repeat("this request is missing its CSRF token. ", 60))
	}))
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
	if got := w.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if !strings.Contains(unzip(t, w), `"code":"csrf"`) {
		t.Error("the error envelope did not survive the round trip")
	}
}

func TestAPanicStillReachesTheClientAsAnError(t *testing.T) {
	// The compressor holds a response until it knows whether to gzip it, and
	// a handler that panics before the threshold has written nothing the
	// client can use. What it must not do is send that nothing: the middleware
	// above answers a panic with a 500, and it can only do so while the
	// response line is still unsent.
	h := Middleware(slog.New(slog.DiscardHandler), Compress(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"partial":`)
			panic("while rendering the report")
		})))
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), "internal error") {
		t.Errorf("body = %q, want the error envelope", w.Body.String())
	}
}
