package gateway

import "testing"

func TestForwardableContentType(t *testing.T) {
	t.Run("the two types this gateway speaks are kept with their parameters", func(t *testing.T) {
		for _, ct := range []string{
			"application/json",
			"application/json; charset=utf-8",
			"text/event-stream",
			"text/event-stream; charset=utf-8",
			"Application/JSON",
		} {
			if got := forwardableContentType(ct); got != ct {
				t.Errorf("forwardableContentType(%q) = %q, want it unchanged", ct, got)
			}
		}
	})

	t.Run("anything a browser would render is answered as JSON", func(t *testing.T) {
		// The panel shares the origin, so text/html would render next to the
		// session cookie.
		for _, ct := range []string{
			"text/html",
			"text/html; charset=utf-8",
			"image/svg+xml",
			"application/xhtml+xml",
			"text/plain",
			"",
		} {
			if got := forwardableContentType(ct); got != "application/json" {
				t.Errorf("forwardableContentType(%q) = %q, want application/json", ct, got)
			}
		}
	})
}
