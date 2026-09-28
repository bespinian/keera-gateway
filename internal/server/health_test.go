package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadyURL(t *testing.T) {
	for addr, want := range map[string]string{
		":8080":          "http://127.0.0.1:8080/readyz",
		"0.0.0.0:8080":   "http://127.0.0.1:8080/readyz",
		"[::]:8080":      "http://127.0.0.1:8080/readyz",
		"10.0.0.5:9000":  "http://10.0.0.5:9000/readyz",
		"[fd00::1]:9000": "http://[fd00::1]:9000/readyz",
	} {
		got, err := readyURL(addr)
		if err != nil || got != want {
			t.Errorf("readyURL(%q) = %q, %v; want %q", addr, got, err, want)
		}
	}
	if _, err := readyURL("8080"); err == nil {
		t.Error("readyURL accepted an address with no port")
	}
}

func TestHealth(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	if err := health(context.Background(), addr); err != nil {
		t.Fatalf("ready server: %v", err)
	}
	status = http.StatusServiceUnavailable
	if err := health(context.Background(), addr); err == nil {
		t.Fatal("a server answering 503 passed")
	}
}
