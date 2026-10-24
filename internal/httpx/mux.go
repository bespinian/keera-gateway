package httpx

import "net/http"

// Mux is a ServeMux that remembers its patterns, so a test can check that the
// OpenAPI spec documents every route.
type Mux struct {
	*http.ServeMux
	patterns []string
}

func NewMux() *Mux { return &Mux{ServeMux: http.NewServeMux()} }

func (m *Mux) Handle(pattern string, h http.Handler) {
	m.patterns = append(m.patterns, pattern)
	m.ServeMux.Handle(pattern, h)
}

func (m *Mux) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	m.Handle(pattern, http.HandlerFunc(h))
}

// Patterns are the patterns registered so far, in order.
func (m *Mux) Patterns() []string { return m.patterns }
