package control

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
)

func TestAccessSaysTheOperatorKeyIsNobody(t *testing.T) {
	// The operator key belongs to a deployment, not to a person, so it has no keys
	// and no budget of its own. Answering with an empty screen would read as a
	// deployment with nothing in it - and reaching for a person's rows would
	// reach for a person who is not there.
	s := New(nil, nil, nil, nil, Options{
		OperatorKey: testOperatorKey, Currency: "CHF",
	}, slog.New(slog.DiscardHandler))

	r := httptest.NewRequest(http.MethodGet, httpx.ControlPrefix+"/v1/access", nil)
	r.Header.Set("Authorization", "Bearer "+testOperatorKey)
	w := httptest.NewRecorder()
	s.authenticated(s.access).ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got struct {
		Anonymous bool              `json:"anonymous"`
		Currency  string            `json:"currency"`
		Keys      []json.RawMessage `json:"keys"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Anonymous {
		t.Error("the operator key is not a person and the screen has to say so")
	}
	if got.Currency != "CHF" {
		t.Errorf("currency = %q, want CHF", got.Currency)
	}
	if got.Keys == nil {
		t.Error("keys must be an empty list rather than null, which renders as a broken screen")
	}
}

func TestAllowedModelsIsWhatTheKeyCanActuallyCall(t *testing.T) {
	models := []policy.Model{
		{Alias: "keera-code", Enabled: true},
		{Alias: "keera-frontier", Enabled: true},
		{Alias: "keera-retired"}, // disabled
	}
	tests := []struct {
		name  string
		limit *policy.Limits
		want  []string
	}{
		{
			// A nil allow-list means every enabled alias. Resolving it to the
			// real list is what lets the screen name them instead of saying
			// "no restriction" and leaving the developer to guess.
			name:  "no restriction resolves to the catalogue",
			limit: nil,
			want:  []string{"keera-code", "keera-frontier"},
		},
		{
			name:  "an allow-list narrows it",
			limit: &policy.Limits{AllowedModels: []string{"keera-code", "keera-retired"}},
			want:  []string{"keera-code"},
		},
		{
			name:  "an empty allow-list allows nothing",
			limit: &policy.Limits{AllowedModels: []string{}},
			want:  []string{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"}, tc.limit, nil, nil)
			if got := allowedModels(res, models); !slices.Equal(got, tc.want) {
				t.Errorf("allowedModels = %v, want %v", got, tc.want)
			}
		})
	}
}

// A developer is shown each level's own wording rather than the joined text the
// gateway sends. The joined text says what reaches the model; it does not say
// which of three people to ask about changing it, and that is the question
// somebody reading this screen actually has.
func TestScopeStatesNameEachLevelsOwnPrompt(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))

	const (
		fromOrg = "Answer in British English."
		fromKey = "Prefer the standard library."
	)
	// The project sets none, which must read as a level that says nothing rather
	// than as the level above it repeated.
	res := policy.Resolve(
		policy.Key{ID: "key_1", OrgID: "org_1", ProjectID: "project_1"},
		&policy.Limits{SystemPrompt: new(fromOrg)},
		&policy.Limits{},
		&policy.Limits{SystemPrompt: new(fromKey)},
	)
	got := s.scopeStates(res, "Example Bank",
		map[string]string{"project_1": "Payments Platform"}, "a developer's laptop",
		time.Now(), map[policy.ScopeType]scopeSays{
			policy.ScopeOrg: {prompt: fromOrg, filters: []string{"redact-secrets"}},
			policy.ScopeKey: {prompt: fromKey},
		})

	want := []struct {
		scope  policy.ScopeType
		name   string
		prompt string
	}{
		{policy.ScopeOrg, "Example Bank", fromOrg},
		{policy.ScopeProject, "Payments Platform", ""},
		{policy.ScopeKey, "a developer's laptop", fromKey},
	}
	if len(got) != len(want) {
		t.Fatalf("%d scopes, want %d - outermost first", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Type != w.scope {
			t.Errorf("scope %d is %q, want %q: they are sent in this order",
				i, got[i].Type, w.scope)
		}
		if got[i].Name != w.name {
			t.Errorf("scope %d name = %q, want %q", i, got[i].Name, w.name)
		}
		if got[i].SystemPrompt != w.prompt {
			t.Errorf("scope %d prompt = %q, want %q", i, got[i].SystemPrompt, w.prompt)
		}
	}
	// Filters are reported against the level that applied them, so a developer
	// reading this screen can see who put their prompt through what.
	if len(got[0].Filters) != 1 || got[0].Filters[0] != "redact-secrets" {
		t.Errorf("the organisation's filters = %q, want them named", got[0].Filters)
	}
	if len(got[1].Filters) != 0 {
		t.Errorf("the project's filters = %q; a level that applies none must not "+
			"inherit the display of one above it", got[1].Filters)
	}
}

func TestSaysOf(t *testing.T) {
	// A scope with no guardrail row at all, and one whose prompt was cleared, are
	// both levels that say nothing.
	if got := saysOf(nil); got.prompt != "" || got.filters != nil {
		t.Errorf("saysOf(nil) = %+v, want nothing", got)
	}
	if got := saysOf(&policy.Limits{}); got.prompt != "" || got.filters != nil {
		t.Errorf("saysOf(unset) = %+v, want nothing", got)
	}
	got := saysOf(&policy.Limits{
		SystemPrompt: new("Be brief."), Filters: []string{"redact-secrets"},
	})
	if got.prompt != "Be brief." {
		t.Errorf("prompt = %q, want the wording", got.prompt)
	}
	if len(got.filters) != 1 || got.filters[0] != "redact-secrets" {
		t.Errorf("filters = %q, want the filter named", got.filters)
	}
}
