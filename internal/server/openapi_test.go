package server

import (
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/control"
	"github.com/bespinian/keera-gateway/internal/gateway"
	"github.com/bespinian/keera-gateway/internal/openapi"
	"github.com/bespinian/keera-gateway/internal/ratelimit"
	"github.com/bespinian/keera-gateway/internal/sandbox"
)

// The spec is written by hand: every route must be in it, and everything in it
// must be a route.
func TestOpenAPIDescribesEveryRoute(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	gw := gateway.New(nil, nil, ratelimit.New(), nil, nil, gateway.Options{}, log)
	// With sandboxes on, so their routes are registered too.
	ctl := control.New(nil, nil, nil, nil, control.Options{
		OperatorKey: testOperatorKey, ServeUI: true,
		Sandboxes: sandbox.NewManager(nil, nil, sandbox.ManagerOptions{Log: log}),
	}, log)

	var served []string
	for _, p := range slices.Concat(gw.Routes(), ctl.Routes()) {
		method, path, ok := strings.Cut(p, " ")
		// A pattern without a method is a catch-all that answers 404.
		if !ok {
			continue
		}
		served = append(served, method+" "+strings.ReplaceAll(path, "...}", "}"))
	}

	doc, err := openapi.Spec()
	if err != nil {
		t.Fatalf("Spec: %v", err)
	}
	var spec struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := doc.Decode(&spec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var described []string
	for path, ops := range spec.Paths {
		for method := range ops {
			if slices.Contains(httpMethods, method) {
				described = append(described, strings.ToUpper(method)+" "+path)
			}
		}
	}

	for _, r := range served {
		if !slices.Contains(described, r) {
			t.Errorf("%s is served but not in internal/openapi/openapi.yaml", r)
		}
	}
	for _, r := range described {
		if !slices.Contains(served, r) {
			t.Errorf("%s is in internal/openapi/openapi.yaml but not served", r)
		}
	}
}

// httpMethods are the keys of an OpenAPI path item that are operations.
var httpMethods = []string{"get", "head", "post", "put", "patch", "delete", "options"}
