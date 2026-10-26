package openapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func spec(t *testing.T) map[string]any {
	t.Helper()
	doc, err := Spec()
	if err != nil {
		t.Fatalf("Spec: %v", err)
	}
	var m map[string]any
	if err := doc.Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

// A $ref that points nowhere breaks every generator that reads the spec.
func TestEveryRefResolves(t *testing.T) {
	m := spec(t)
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			if ref, ok := v["$ref"].(string); ok {
				if !resolves(m, ref) {
					t.Errorf("$ref %q points nowhere", ref)
				}
			}
			for _, c := range v {
				walk(c)
			}
		case []any:
			for _, c := range v {
				walk(c)
			}
		}
	}
	walk(m)
}

func resolves(m map[string]any, ref string) bool {
	path, ok := strings.CutPrefix(ref, "#/")
	if !ok {
		return false
	}
	var v any = m
	for part := range strings.SplitSeq(path, "/") {
		obj, ok := v.(map[string]any)
		if !ok {
			return false
		}
		if v, ok = obj[strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")]; !ok {
			return false
		}
	}
	return true
}

var pathParam = regexp.MustCompile(`\{([^}]+)\}`)

// Generators name methods after operationId, and refuse a path whose
// parameters are not declared.
func TestOperationsAreWellFormed(t *testing.T) {
	m := spec(t)
	params, _ := m["components"].(map[string]any)["parameters"].(map[string]any)
	seen := map[string]string{}
	for path, item := range m["paths"].(map[string]any) {
		for method, op := range item.(map[string]any) {
			op, ok := op.(map[string]any)
			if !ok || method == "parameters" {
				continue
			}
			name := strings.ToUpper(method) + " " + path
			opID, _ := op["operationId"].(string)
			switch {
			case opID == "":
				t.Errorf("%s has no operationId", name)
			case seen[opID] != "":
				t.Errorf("%s and %s share operationId %s", seen[opID], name, opID)
			}
			seen[opID] = name
			if _, ok := op["responses"]; !ok {
				t.Errorf("%s has no responses", name)
			}

			declared := map[string]bool{}
			list, _ := op["parameters"].([]any)
			shared, _ := item.(map[string]any)["parameters"].([]any)
			for _, p := range append(list, shared...) {
				p := p.(map[string]any)
				if ref, ok := p["$ref"].(string); ok {
					p, _ = params[strings.TrimPrefix(ref, "#/components/parameters/")].(map[string]any)
				}
				if p["in"] == "path" {
					declared[p["name"].(string)] = true
				}
			}
			for _, want := range pathParam.FindAllStringSubmatch(path, -1) {
				if !declared[want[1]] {
					t.Errorf("%s does not declare path parameter %s", name, want[1])
				}
			}
		}
	}
}

// The JSON is built from the YAML by hand, so check it against a decoder.
func TestJSONSaysWhatTheYAMLSays(t *testing.T) {
	get := func(h http.Handler) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body)
		}
		return w
	}
	yw, jw := get(YAML()), get(JSON())
	if ct := jw.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("JSON Content-Type = %q", ct)
	}
	if ct := yw.Header().Get("Content-Type"); ct != "application/yaml" {
		t.Errorf("YAML Content-Type = %q", ct)
	}

	var fromYAML, fromJSON any
	if err := yaml.Unmarshal(yw.Body.Bytes(), &fromYAML); err != nil {
		t.Fatalf("served YAML: %v", err)
	}
	if err := json.Unmarshal(jw.Body.Bytes(), &fromJSON); err != nil {
		t.Fatalf("served JSON: %v", err)
	}
	// A round trip through JSON makes the YAML's numbers and keys alike.
	b, err := json.Marshal(fromYAML)
	if err != nil {
		t.Fatalf("YAML as JSON: %v", err)
	}
	var want any
	_ = json.Unmarshal(b, &want)
	if !jsonEqual(want, fromJSON) {
		t.Error("the JSON and the YAML differ")
	}
	if !strings.HasPrefix(jw.Body.String(), `{"openapi":`) {
		t.Errorf("JSON starts %.20q, want the keys in written order", jw.Body.String())
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// A client generated from the spec says which build it was made for.
func TestVersionIsTheBuild(t *testing.T) {
	info := spec(t)["info"].(map[string]any)
	if v, _ := info["version"].(string); v == "" || strings.HasPrefix(v, "v") {
		t.Errorf("info.version = %q, want the build version without its v", v)
	}
}
