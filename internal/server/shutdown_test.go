package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Shutdown waits for requests still running, since they record their usage
// as they return.
func TestRequestsWaitsForTheRequestsBeingServed(t *testing.T) {
	var q requests
	release := make(chan struct{})
	started := make(chan struct{})
	h := q.track(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	}))
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		close(done)
	}()
	<-started

	if left := q.wait(30 * time.Millisecond); left != 1 {
		t.Fatalf("left = %d, want 1 while the request runs", left)
	}
	close(release)
	<-done
	if left := q.wait(time.Second); left != 0 {
		t.Fatalf("left = %d, want 0 once it returned", left)
	}
}
