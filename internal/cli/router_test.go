package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// guardedRouter is the example in docs/routers.md: a pattern filter, a
// budget whose exceeded branch goes to the local model, and a route step
// whose hosted branch is redacted first.
func guardedRouter() policy.Router {
	return policy.Router{Alias: "guarded", Start: &policy.Step{
		ID: "keys", Kind: policy.StepFilter, Ref: "redact-keys",
		Next: &policy.Step{
			ID: "hosted-budget", Kind: policy.StepBudget, BudgetMicros: 50_000_000, Period: policy.PeriodMonth,
			Exceeded: &policy.Step{ID: "local", Kind: policy.StepModel, Ref: "keera-local"},
			Next: &policy.Step{
				ID: "route", Kind: policy.StepRoute, Mode: policy.RouteModeInstruction,
				Model: "keera-decider", Prompt: "Keep client data local.",
				Destinations: []string{"keera-speed", "qwen3"},
				Branches: map[string]*policy.Step{
					"qwen3": {
						ID: "names", Kind: policy.StepFilter, Ref: "redact-names",
						Next: &policy.Step{ID: "hosted", Kind: policy.StepModel, Ref: "qwen3"},
					},
				},
			},
		},
	}}
}

// plain makes the printers write no escapes, whatever an earlier test's Run
// decided for this process.
func plain(t *testing.T) {
	t.Helper()
	was := style
	style = false
	t.Cleanup(func() { style = was })
}

func TestStepTreeDrawsEveryBranch(t *testing.T) {
	plain(t)
	got := strings.Join(stepTree(guardedRouter().Start), "\n")
	want := strings.Join([]string{
		"filter redact-keys",
		"budget CHF 50.00 per month (hosted-budget)",
		"├─ within",
		"│  route decided by keera-decider, refuses the request when it cannot (route)",
		"│  ├─ to keera-speed, sent straight there",
		"│  └─ to qwen3",
		"│     filter redact-names",
		"│     model qwen3",
		"└─ exceeded",
		"   model keera-local",
	}, "\n")
	if got != want {
		t.Errorf("tree:\n%s\nwant:\n%s", got, want)
	}
}

// A limit with no exceeded branch refuses, so the steps after it stay on the
// same line of the tree rather than under a lone 'within'.
func TestStepTreeKeepsALimitWithoutBranchesInLine(t *testing.T) {
	plain(t)
	start := &policy.Step{
		ID: "rl", Kind: policy.StepRateLimit, Requests: 60, Tokens: 100000,
		Next: &policy.Step{ID: "m", Kind: policy.StepModel, Ref: "keera-local"},
	}
	got := stepTree(start)
	want := []string{
		"rate limit 60 requests and 100000 tokens per minute (rl), refuses what is over it",
		"model keera-local",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("tree = %q, want %q", got, want)
	}
}

// A system prompt is shown by its start, on one line, and a daily limit says
// its period.
func TestStepTreeShowsAPromptAndAPeriod(t *testing.T) {
	plain(t)
	start := &policy.Step{
		ID: "p", Kind: policy.StepSystemPrompt,
		Prompt: "Answer in German.\nNever name a customer, and keep every answer short enough to read.",
		Next: &policy.Step{
			ID: "rl", Kind: policy.StepRateLimit, Requests: 500, Period: policy.PeriodDay,
			Next: &policy.Step{ID: "m", Kind: policy.StepModel, Ref: "keera-local"},
		},
	}
	got := stepTree(start)
	want := []string{
		`system prompt "Answer in German. Never name a customer, and keep every ans…"`,
		"rate limit 500 requests per day (rl), refuses what is over it",
		"model keera-local",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("tree = %q, want %q", got, want)
	}
}

// A branch for a destination the route step no longer has is still drawn, so
// an administrator can see it and take it out. A size step's branches say
// what each one takes.
func TestStepTreeShowsABranchForAGoneDestination(t *testing.T) {
	plain(t)
	start := &policy.Step{
		ID: "r", Kind: policy.StepRoute, Mode: policy.RouteModeSize,
		Destinations: []string{"small", "big"}, Ceilings: map[string]int{"small": 4000},
		Branches: map[string]*policy.Step{"old": {ID: "m", Kind: policy.StepModel, Ref: "old"}},
	}
	got := strings.Join(stepTree(start), "\n")
	for _, want := range []string{
		"route by size (r)", "├─ to small (up to 4k tokens), sent straight there",
		"└─ to old\n   model old",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("tree lacks %q:\n%s", want, got)
		}
	}
}

func TestNameStepsFillsOnlyMissingIDs(t *testing.T) {
	p := policy.Router{Start: &policy.Step{
		Kind: policy.StepFilter, Ref: "a",
		Next: &policy.Step{
			ID: "filter-1", Kind: policy.StepFilter, Ref: "b",
			Next: &policy.Step{
				Kind: policy.StepRateLimit, Requests: 1,
				Next: &policy.Step{Kind: policy.StepModel, Ref: "m"},
			},
		},
	}}
	nameSteps(p)
	var ids []string
	for s := range p.Steps() {
		ids = append(ids, s.ID)
	}
	// filter-1 is taken by the file, so the unnamed filter gets the next one.
	want := "filter-2 filter-1 rate-limit-1 model-1"
	if got := strings.Join(ids, " "); got != want {
		t.Errorf("ids = %q, want %q", got, want)
	}
}

func TestReadRouterFileTakesAStepOrAWholeRouter(t *testing.T) {
	dir := t.TempDir()
	step := filepath.Join(dir, "step.json")
	whole := filepath.Join(dir, "whole.json")
	if err := os.WriteFile(step, []byte(`{"kind":"model","ref":"m"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(whole, []byte(`{"alias":"x","display_name":"Safe",
		"start":{"kind":"model","ref":"n"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var p policy.Router
	if err := readRouterFile(step, &p); err != nil {
		t.Fatal(err)
	}
	if p.Start.Ref != "m" || p.DisplayName != "" {
		t.Errorf("from a step: %+v", p)
	}
	if err := readRouterFile(whole, &p); err != nil {
		t.Fatal(err)
	}
	if p.Start.Ref != "n" || p.DisplayName != "Safe" {
		t.Errorf("from a whole router: %+v %+v", p, p.Start)
	}
	if err := os.WriteFile(step, []byte(`[]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := readRouterFile(step, &p); err == nil {
		t.Error("a JSON array was read as a router")
	}
	// The control plane refuses a field it does not know, so the file is
	// read the same way rather than losing a misspelt branch.
	if err := os.WriteFile(step, []byte(`{"kind":"rate_limit","requests":5,
		"exceded":{"kind":"model","ref":"m"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := readRouterFile(step, &p); err == nil || !strings.Contains(err.Error(), "exceded") {
		t.Errorf("err = %v, want the misspelt field named", err)
	}
}

func TestRouterAddSendsTheFile(t *testing.T) {
	f := newFakeControl(t, map[string]any{
		"GET /v1/routers": map[string]any{"data": []any{}},
		"PUT /v1/routers/guarded": map[string]any{
			"alias": "guarded", "start": map[string]any{"id": "model-1", "kind": "model", "ref": "m"},
			"warnings": []string{"the route step 'r' reads the request after the filter 'x' rewrote it"},
		},
	})
	t.Setenv("KEERA_ORG", "org_1")
	file := filepath.Join(t.TempDir(), "p.json")
	if err := os.WriteFile(file, []byte(`{"kind":"model","ref":"m"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, read := quiet(t)
	err := Run(context.Background(), []string{
		"router", "add", "guarded",
		"--file", file, "--display-name", "Guarded",
	})
	if err != nil {
		t.Fatal(err)
	}
	put := f.request("PUT", "/v1/routers/guarded")
	start, _ := put.body["start"].(map[string]any)
	if start["ref"] != "m" || start["id"] != "model-1" || put.body["display_name"] != "Guarded" {
		t.Errorf("PUT body = %v", put.body)
	}
	if said := read(); !strings.Contains(said, "reads the request after the filter") {
		t.Errorf("the warning was not passed on: %q", said)
	}
}

// The route flags without --file write the simplest router: one route step.
func TestRouterAddWithOneRouteStep(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/routers": map[string]any{"data": []any{}},
		"PUT /v1/routers/auto": map[string]any{
			"alias": "auto", "start": map[string]any{"id": "route", "kind": "route"},
		},
	})
	t.Setenv("KEERA_ORG", "org_1")
	if err := Run(context.Background(), []string{
		"router", "add", "auto", "--model", "picker",
		"--destinations", "small,big", "--fallback", "small", "--prompt", "Choose.",
	}); err != nil {
		t.Fatal(err)
	}
	start, _ := f.request("PUT", "/v1/routers/auto").body["start"].(map[string]any)
	if start["kind"] != "route" || start["id"] != "route" || start["mode"] != "instruction" ||
		start["model"] != "picker" || start["fallback"] != "small" || start["prompt"] != "Choose." {
		t.Errorf("start = %v, want one route step", start)
	}
}

// On 'set', the route flags change one route step of the stored tree and keep
// the rest of it. With two route steps, --step says which.
func TestRouterSetChangesOneRouteStep(t *testing.T) {
	quiet(t)
	stored := map[string]any{"alias": "guarded", "start": map[string]any{
		"id": "keys", "kind": "filter", "ref": "redact-keys",
		"next": map[string]any{
			"id": "first", "kind": "route", "mode": "fallback",
			"destinations": []string{"a", "b"},
			"branches": map[string]any{"b": map[string]any{
				"id": "second", "kind": "route",
				"mode": "fallback", "destinations": []string{"c", "d"},
			}},
		},
	}}
	f := newFakeControl(t, map[string]any{
		"GET /v1/routers":         map[string]any{"data": []any{stored}},
		"PUT /v1/routers/guarded": stored,
	})
	t.Setenv("KEERA_ORG", "org_1")

	err := Run(context.Background(), []string{"router", "set", "guarded", "--destinations", "c,e"})
	if err == nil || !strings.Contains(err.Error(), "--step") {
		t.Fatalf("two route steps and no --step: err = %v", err)
	}
	if err := Run(context.Background(), []string{
		"router", "set", "guarded", "--step", "second",
		"--destinations", "c,e",
	}); err != nil {
		t.Fatal(err)
	}
	start, _ := f.request("PUT", "/v1/routers/guarded").body["start"].(map[string]any)
	first, _ := start["next"].(map[string]any)
	second, _ := first["branches"].(map[string]any)["b"].(map[string]any)
	if start["ref"] != "redact-keys" || first["id"] != "first" ||
		strings.Join(toStrings(second["destinations"]), ",") != "c,e" {
		t.Errorf("start = %v, want only the second step's destinations changed", start)
	}
}

func toStrings(v any) []string {
	var out []string
	list, _ := v.([]any)
	for _, s := range list {
		str, _ := s.(string)
		out = append(out, str)
	}
	return out
}

// Only a step that decides has a fallback or a deciding model, and the control
// plane would refuse either elsewhere, so the flags are refused rather than
// dropped.
func TestRouterDecidingFlagsNeedADecidingStep(t *testing.T) {
	newFakeControl(t, map[string]any{"GET /v1/routers": map[string]any{"data": []any{}}})
	t.Setenv("KEERA_ORG", "org_1")
	for _, flag := range []string{"--fallback", "--model"} {
		err := Run(context.Background(), []string{
			"router", "add", "x", "--mode", "least-busy",
			"--destinations", "small,big", flag, "small",
		})
		if err == nil || !strings.Contains(err.Error(), flag+" is for a route step that decides") {
			t.Errorf("%s: err = %v", flag, err)
		}
	}
}

// Without --destinations, a header step's destinations are where its values
// send, and the fallback.
func TestRouterAddWithAHeaderStep(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/routers": map[string]any{"data": []any{}},
		"PUT /v1/routers/byapp": map[string]any{
			"alias": "byapp", "start": map[string]any{"id": "route", "kind": "route"},
		},
	})
	t.Setenv("KEERA_ORG", "org_1")
	if err := Run(context.Background(), []string{
		"router", "add", "byapp", "--mode", "header",
		"--header", "X-Keera-Route", "--values", "hard=big", "--fallback", "small",
	}); err != nil {
		t.Fatal(err)
	}
	start, _ := f.request("PUT", "/v1/routers/byapp").body["start"].(map[string]any)
	values, _ := start["values"].(map[string]any)
	if start["mode"] != "header" || start["header"] != "X-Keera-Route" ||
		values["hard"] != "big" || start["fallback"] != "small" ||
		strings.Join(toStrings(start["destinations"]), ",") != "big,small" {
		t.Errorf("start = %v, want a header step sending to big and small", start)
	}
}

func TestRouterHeaderFlagsNeedAHeaderStep(t *testing.T) {
	newFakeControl(t, map[string]any{"GET /v1/routers": map[string]any{"data": []any{}}})
	t.Setenv("KEERA_ORG", "org_1")
	for _, flag := range []string{"--header", "--values"} {
		err := Run(context.Background(), []string{
			"router", "add", "x", "--mode", "fallback",
			"--destinations", "small,big", flag, "hard=big",
		})
		if err == nil || !strings.Contains(err.Error(), "use --mode header") {
			t.Errorf("%s: err = %v", flag, err)
		}
	}
}

func TestRouterAddNeedsAFileOrARouteStep(t *testing.T) {
	newFakeControl(t, map[string]any{})
	t.Setenv("KEERA_ORG", "org_1")
	err := Run(context.Background(), []string{"router", "add", "x"})
	if err == nil || !strings.Contains(err.Error(), "--file or a route step's flags") {
		t.Errorf("err = %v", err)
	}
}

func TestPrintRoutersNamesWhereEachSends(t *testing.T) {
	plain(t)
	var buf bytes.Buffer
	w := newTable(&buf)
	printRouters(w, []policy.Router{guardedRouter()})
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, want := range []string{"guarded", "6", "keera-speed, qwen3, keera-local"} {
		if !strings.Contains(got, want) {
			t.Errorf("listing lacks %q:\n%s", want, got)
		}
	}
}
