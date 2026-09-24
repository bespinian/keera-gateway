package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

func TestFilterModelRefusal(t *testing.T) {
	t.Run("it says what a filter would do with the request", func(t *testing.T) {
		msg := filterModelRefusal("keera-code", policy.Model{
			Alias:    "keera-code",
			Backends: []string{"http://vllm:8000/v1"},
		})
		if !strings.Contains(msg, "'keera-code'") {
			t.Errorf("the refusal does not name the model: %q", msg)
		}
		// The reason is the ordering, not the symmetry with what a key may
		// call: that is what makes it worth refusing rather than warning.
		if !strings.Contains(msg, "before anything is redacted") {
			t.Errorf("the refusal does not say why a filter is different: %q", msg)
		}
	})

	t.Run("a hosted model is named with the endpoint it would reach", func(t *testing.T) {
		msg := filterModelRefusal("claude-opus-5", policy.Model{
			Alias:    "claude-opus-5",
			Backends: []string{"https://api.anthropic.com/v1"},
		})
		if !strings.Contains(msg, "api.anthropic.com") {
			t.Errorf("a refusal about an external model has to say where it is: %q", msg)
		}
	})

	t.Run("an internal model is not described as hosted anywhere", func(t *testing.T) {
		msg := filterModelRefusal("keera-code", policy.Model{
			Alias:    "keera-code",
			Backends: []string{"http://vllm.svc:8000/v1"},
		})
		if strings.Contains(msg, "hosted at") {
			t.Errorf("an in-cluster model was described as hosted elsewhere: %q", msg)
		}
	})
}

/* ------------------------------------------------------ the pattern filter */

// filterStore is routerStore with the filters table cleared as well, so that
// what these write is the only thing in it.
func filterStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	st, ctx := routerStore(t)
	if _, err := st.Pool().Exec(ctx, "TRUNCATE filters CASCADE"); err != nil {
		t.Fatalf("truncate filters: %v", err)
	}
	return st, ctx
}

func putFilterTo(t *testing.T, srv *Server, alias string, body map[string]any) (int, string) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut,
		httpx.ControlPrefix+"/v1/filters/"+alias+"?org_id=org_1", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+testOperatorKey)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// patternBody is a whole pattern filter: rules, and nothing that would read
// the request with a model.
func patternBody() map[string]any {
	return map[string]any{
		"mode": string(policy.FilterModePattern),
		"rules": []map[string]any{
			{"pattern": `(?i)\bsk-[a-z0-9]{16,}\b`, "replace": "[CREDENTIAL]"},
			{"pattern": `(?i)\bexport all customers\b`, "refuse": true,
				"reason": "that moves the customer list out of the organisation"},
		},
		"description": "Takes what has a shape out of everything",
	}
}

func TestPutFilterWritesAPatternFilter(t *testing.T) {
	st, ctx := filterStore(t)

	if code, out := putFilterTo(t, routerServer(ctx, t, st), "redact-keys",
		patternBody()); code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", code, out)
	}
	saved, err := st.Filter(ctx, "org_1", "redact-keys")
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if saved.UsesModel() {
		t.Errorf("mode = %q, want a filter that runs no model", saved.Mode)
	}
	if len(saved.Rules) != 2 {
		t.Fatalf("rules = %+v, want both of them", saved.Rules)
	}
	if saved.Rules[0].Replace != "[CREDENTIAL]" {
		t.Errorf("rule 0 = %+v", saved.Rules[0])
	}
	if !saved.Rules[1].Refuse || saved.Rules[1].Reason == "" {
		t.Errorf("rule 1 = %+v, want a refusal carrying its sentence", saved.Rules[1])
	}
	// The order is part of what the rules mean - a specific rule above a broad
	// one is how the broad one gets narrowed - so it has to survive the round
	// trip through the column.
	if saved.Rules[0].Pattern != `(?i)\bsk-[a-z0-9]{16,}\b` {
		t.Errorf("the rules came back in a different order: %+v", saved.Rules)
	}
	if saved.Model != "" || saved.Prompt != "" {
		t.Errorf("saved = %+v, want nothing that would read the request", saved)
	}
}

func TestPutFilterRefusesAPatternFilterThatCouldNotRun(t *testing.T) {
	st, ctx := filterStore(t)
	srv := routerServer(ctx, t, st)

	tests := []struct {
		name string
		why  string
		edit func(map[string]any)
	}{
		{
			"no rules",
			"it would read every request the guardrail covers and change nothing, which " +
				"is not a filter that passes everything - it is one somebody thinks works",
			func(b map[string]any) { b["rules"] = []map[string]any{} },
		},
		{
			"an expression that will not compile",
			"a filter fails closed, so a typo in a regular expression is a department " +
				"offline - and the person who typed it is the only one who can fix it",
			func(b map[string]any) {
				b["rules"] = []map[string]any{{"pattern": `([`, "replace": "x"}}
			},
		},
		{
			"an expression that matches the empty string",
			"it matches at every position of every segment, so it would insert its " +
				"replacement between every character of somebody's request",
			func(b map[string]any) {
				b["rules"] = []map[string]any{{"pattern": `x*`, "replace": "[X]"}}
			},
		},
		{
			"a rule that both replaces and refuses",
			"a credential redacted out of a request that is then dropped was redacted " +
				"for nobody",
			func(b map[string]any) {
				b["rules"] = []map[string]any{
					{"pattern": `x`, "replace": "y", "refuse": true},
				}
			},
		},
		{
			"a deciding model",
			"it applies its rules in the gateway and runs no model at all",
			func(b map[string]any) { b["model"] = "keera-picker" },
		},
		{
			"an instruction",
			"nothing reads prose, so there is nothing to give an instruction to",
			func(b map[string]any) { b["prompt"] = "redact everything" },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := patternBody()
			tc.edit(body)
			code, out := putFilterTo(t, srv, "redact-keys", body)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 - %s: %s", code, tc.why, out)
			}
		})
	}
}

func TestPutFilterRefusesRulesOnAFilterThatReadsWithAModel(t *testing.T) {
	// Rules on a rewrite or gate filter are a list nothing applies, and one an
	// administrator would later read as the thing the filter does.
	st, ctx := filterStore(t)
	srv := routerServer(ctx, t, st)

	for _, mode := range []policy.FilterMode{
		policy.FilterModeRewrite, policy.FilterModeGate,
	} {
		t.Run(string(mode), func(t *testing.T) {
			code, out := putFilterTo(t, srv, "redact", map[string]any{
				"mode":   string(mode),
				"model":  "keera-picker",
				"prompt": "Replace every credential with [CREDENTIAL].",
				"rules":  []map[string]any{{"pattern": `x`, "replace": "y"}},
			})
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", code, out)
			}
		})
	}
}

func TestAPatternFilterIsNotStrandedByAnAllowList(t *testing.T) {
	// It runs no model, so there is nothing for an allow-list to strand it
	// outside of - and it is the one filter that could never carry a request
	// anywhere, because its rules are applied in this process.
	st, ctx := filterStore(t)
	srv := routerServer(ctx, t, st)

	if code, out := putFilterTo(t, srv, "redact-keys", patternBody()); code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", code, out)
	}

	raw, err := json.Marshal(map[string]any{"allowed_models": []string{"keera-small"}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut,
		httpx.ControlPrefix+"/v1/guardrails/org/org_1", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+testOperatorKey)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 - a pattern filter has no model to be stranded "+
			"outside the list: %s", w.Code, w.Body.String())
	}
}
