package gateway

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// readWith reads body as it would arrive with the given Content-Encoding, and
// returns what was read, or the status of the refusal.
func readWith(t *testing.T, encoding string, body []byte, limit int64) ([]byte, int) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", bytes.NewReader(body))
	if encoding != "" {
		r.Header.Set("Content-Encoding", encoding)
	}
	status := 0
	raw, ok := readBody(httptest.NewRecorder(), r, limit, func(s int, _, _ string) { status = s })
	if !ok && status == 0 {
		t.Fatal("the body was neither read nor refused")
	}
	return raw, status
}

func gzipped(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestACompressedBodyIsReadUnpacked(t *testing.T) {
	want := []byte(`{"model":"m"}`)
	var deflated bytes.Buffer
	zw := zlib.NewWriter(&deflated)
	_, _ = zw.Write(want)
	_ = zw.Close()

	for enc, body := range map[string][]byte{"": want, "gzip": gzipped(t, want), "deflate": deflated.Bytes()} {
		got, status := readWith(t, enc, body, 1<<20)
		if status != 0 || !bytes.Equal(got, want) {
			t.Errorf("%q: got %q (status %d), want %q", enc, got, status, want)
		}
	}
}

func TestAnUnknownEncodingIsRefusedAsSuch(t *testing.T) {
	// It used to be parsed as JSON and refused as "invalid character".
	if _, status := readWith(t, "zstd", []byte("(\xb5/\xfd"), 1<<20); status != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", status)
	}
	if _, status := readWith(t, "gzip", []byte(`{"model":"m"}`), 1<<20); status != http.StatusBadRequest {
		t.Errorf("a body that is not gzip: status = %d, want 400", status)
	}
}

func TestTheLimitHoldsForWhatABodyUnpacksTo(t *testing.T) {
	// A few kilobytes of gzip unpack to megabytes.
	bomb := gzipped(t, []byte(strings.Repeat("a", 4<<20)))
	if len(bomb) > 64<<10 {
		t.Fatalf("the compressed body is %d bytes, too big to test the limit", len(bomb))
	}
	if _, status := readWith(t, "gzip", bomb, 1<<20); status != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", status)
	}
}
