// Package policy holds the tenancy model: the org → project → key hierarchy,
// the limits at each level, and how they combine.
package policy

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ScopeType names a level of the hierarchy that a policy can attach to.
type ScopeType string

const (
	ScopeOrg     ScopeType = "org"
	ScopeProject ScopeType = "project"
	ScopeKey     ScopeType = "key"
)

// Period is the window a budget resets on.
type Period string

const (
	PeriodDay   Period = "day"
	PeriodMonth Period = "month"
	// PeriodMinute is for rate limits only. A token bucket has no start, so
	// Valid leaves it out and no budget can reset on it.
	PeriodMinute Period = "minute"
)

// Start returns the start of the period containing t. Budgets always reset in
// UTC.
func (p Period) Start(t time.Time) time.Time {
	t = t.UTC()
	if p == PeriodMonth {
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// Next returns the start of the next period, when a spent budget works again.
func (p Period) Next(t time.Time) time.Time {
	start := p.Start(t)
	if p == PeriodMonth {
		return start.AddDate(0, 1, 0)
	}
	return start.AddDate(0, 0, 1)
}

func (p Period) Valid() bool { return p == PeriodDay || p == PeriodMonth }

// Kind is the API a model is served on. A chat model cannot be reached through
// /v1/embeddings and vice versa.
type Kind string

const (
	KindChat       Kind = "chat"
	KindCompletion Kind = "completion"
	KindEmbedding  Kind = "embedding"
	// KindDecision answers a typed question with a probability per answer and
	// writes no text, such as Laya behind laya-serve. Only route steps and gate
	// filters use one; clients cannot name it.
	KindDecision Kind = "decision"
)

func (k Kind) Valid() bool {
	return k == KindChat || k == KindCompletion || k == KindEmbedding || k == KindDecision
}

// Model is one of an organisation's models. Clients only ever name the Alias,
// so the model behind it can change without any client changing.
type Model struct {
	Alias string `json:"alias"`
	// Only OrgID can call the model, so two organisations can each have a 'fast'.
	OrgID        string   `json:"org_id"`
	Kind         Kind     `json:"kind"`
	Backends     []string `json:"backends"`
	BackendModel string   `json:"backend_model"`
	// Provider is the hosted provider, or empty for a self-served model. It
	// lets the gateway pass the provider's own API through and fix the few
	// fields OpenAI refuses.
	Provider string `json:"provider,omitempty"`
	// DisplayName is only shown, such as Claude Haiku 5.5. Clients name the alias.
	DisplayName string `json:"display_name,omitempty"`
	// Description is shown on /v1/models, and a route step reads it when choosing.
	Description string `json:"description,omitempty"`
	// Prices are micro-units per million tokens, so money stays an integer.
	InputMicrosPerMTok  int64 `json:"input_micros_per_mtok"`
	OutputMicrosPerMTok int64 `json:"output_micros_per_mtok"`
	// CachedInputMicrosPerMTok prices an input token read from the provider's
	// prompt cache. A coding agent's prompt is mostly cached, so the full input
	// price would overstate cost. Zero means not stated: see cachedInputMicros.
	CachedInputMicrosPerMTok int64 `json:"cached_input_micros_per_mtok"`
	// CacheWriteMicrosPerMTok prices an input token written to the prompt
	// cache, which Anthropic and OpenAI charge more for. Zero means the input
	// price.
	CacheWriteMicrosPerMTok int64 `json:"cache_write_micros_per_mtok"`
	// LongPrompt prices a prompt above its threshold, for providers that charge
	// more for long prompts. Nil means one price for every length.
	LongPrompt *PriceTier `json:"long_prompt,omitempty"`
	// MaxContext is shown on /v1/models, and a request that cannot fit is
	// refused. Without a tokeniser the check counts the least a request can be,
	// so a request near the limit is left to the inference plane.
	MaxContext int `json:"max_context_tokens"`
	// ReleaseDate is YYYY-MM-DD, or empty when not known.
	ReleaseDate string `json:"release_date,omitempty"`
	// Location is where the model runs, and so where prompts go: a country,
	// such as ch or usa, or onprem.
	Location string `json:"location"`
	// APIKeyCiphertext is sealed with a key only the gateway's environment holds.
	APIKeyCiphertext []byte `json:"-"`
	// APIKey is the decrypted credential. It lives only in memory.
	APIKey string `json:"-"`
	// HasAPIKey tells the panel a credential is stored, and nothing more.
	HasAPIKey bool `json:"has_api_key,omitempty"`
	// Subscription says each caller's own Claude subscription pays. The
	// gateway forwards the caller's sign-in, and the prices only say what the
	// API would cost. See docs/subscriptions.md.
	Subscription bool `json:"subscription,omitempty"`
	// PlatformKey says the deployment holds the provider key, so the
	// organisation sets none. Requests go to the provider at list prices and
	// are billed. See docs/billing.md.
	PlatformKey bool `json:"platform_key,omitempty"`
	// Billing is set by the registry for a model on the deployment's key. It
	// never leaves the gateway.
	Billing *Billing `json:"-"`
	// Limited says the organisation signed itself up and has not paid, so the
	// gateway connects only to public addresses for it. Locked says it may not
	// use this model yet: the model runs inside the deployment's network, or
	// would be billed after the fact. OrgName lets a refusal name the
	// organisation. The registry sets all three. See docs/sso.md.
	Limited bool   `json:"-"`
	Locked  bool   `json:"-"`
	OrgName string `json:"-"`
	Enabled bool   `json:"enabled"`
}

// Key names the model across all tenants, for per-model state.
func (m Model) Key() string { return ModelKey(m.OrgID, m.Alias) }

func ModelKey(orgID, alias string) string { return orgID + "/" + alias }

// Tokens is what one answer used, as the provider reported.
type Tokens struct {
	Input int
	// CachedInput and CacheWrite are parts of Input, as providers report them.
	CachedInput int
	CacheWrite  int
	// CacheWriteHour is the part of CacheWrite kept for an hour, not five
	// minutes. Anthropic charges it at twice the input price.
	CacheWriteHour int
	Output         int
}

// clamped keeps the cache counts inside the input count, so a bad provider
// response cannot make a negative cost and refund a budget.
func (t Tokens) clamped() Tokens {
	t.CachedInput = min(max(t.CachedInput, 0), t.Input)
	t.CacheWrite = min(max(t.CacheWrite, 0), t.Input-t.CachedInput)
	t.CacheWriteHour = min(max(t.CacheWriteHour, 0), t.CacheWrite)
	return t
}

// Cost returns what t costs, in micro-units.
func (m Model) Cost(t Tokens) int64 {
	const perM = 1_000_000
	t = t.clamped()
	p := m.prices(t.Input)
	// Divide once, so the total is rounded down only once.
	return (int64(t.Input-t.CachedInput-t.CacheWrite)*p.InputMicrosPerMTok +
		int64(t.CachedInput)*p.cachedInputMicros() +
		int64(t.CacheWrite-t.CacheWriteHour)*p.cacheWriteMicros() +
		int64(t.CacheWriteHour)*2*p.InputMicrosPerMTok +
		int64(t.Output)*p.OutputMicrosPerMTok) / perM
}

func (m Model) prices(inputTokens int) PriceTier {
	if t := m.LongPrompt; t != nil && inputTokens > t.AboveTokens {
		return *t
	}
	return PriceTier{
		InputMicrosPerMTok:       m.InputMicrosPerMTok,
		OutputMicrosPerMTok:      m.OutputMicrosPerMTok,
		CachedInputMicrosPerMTok: m.CachedInputMicrosPerMTok,
		CacheWriteMicrosPerMTok:  m.CacheWriteMicrosPerMTok,
	}
}

// PriceTier replaces a model's prices for a long prompt. The prompt counts
// every input token, cached or not, as providers measure it.
type PriceTier struct {
	// AboveTokens is the longest prompt still charged the model's own prices.
	AboveTokens              int   `json:"above_tokens" yaml:"above_tokens"`
	InputMicrosPerMTok       int64 `json:"input_micros_per_mtok" yaml:"input_micros_per_mtok"`
	OutputMicrosPerMTok      int64 `json:"output_micros_per_mtok" yaml:"output_micros_per_mtok"`
	CachedInputMicrosPerMTok int64 `json:"cached_input_micros_per_mtok" yaml:"cached_input_micros_per_mtok"`
	CacheWriteMicrosPerMTok  int64 `json:"cache_write_micros_per_mtok" yaml:"cache_write_micros_per_mtok"`
}

func (t PriceTier) Check() error {
	if t.AboveTokens <= 0 {
		return errors.New("long_prompt needs above_tokens, the longest prompt " +
			"charged the normal prices")
	}
	return nil
}

// cachedInputMicros falls back to the input rate. Zero means not stated, not
// free: a forgotten price should err high.
func (t PriceTier) cachedInputMicros() int64 {
	if t.CachedInputMicrosPerMTok == 0 {
		return t.InputMicrosPerMTok
	}
	return t.CachedInputMicrosPerMTok
}

// cacheWriteMicros falls back to the input rate, for providers that charge
// nothing extra for a write.
func (t PriceTier) cacheWriteMicros() int64 {
	if t.CacheWriteMicrosPerMTok == 0 {
		return t.InputMicrosPerMTok
	}
	return t.CacheWriteMicrosPerMTok
}

// Filter is a guardrail that reads a prompt before it is forwarded, and may
// change or stop it, so credentials and client data stay in the cluster. Its
// alias is scoped to an organisation.
type Filter struct {
	OrgID string `json:"org_id"`
	Alias string `json:"alias"`
	// Model should run in the same infrastructure, or the prompt leaves the
	// cluster before it is redacted. A pattern filter has none.
	Model  string     `json:"model"`
	Mode   FilterMode `json:"mode"`
	Prompt string     `json:"prompt"`
	// Rules define a pattern filter, applied in order. Other modes have none.
	Rules []FilterRule `json:"rules,omitempty"`
	// Shadow runs the filter but enforces nothing: the request goes on as sent
	// and the outcome is recorded, to test on real traffic. It costs the same.
	Shadow      bool      `json:"shadow,omitempty"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// FilterMode is what a filter does with a request. Every mode can stop one:
//   - rewrite writes the conversation back, the only way to take a credential
//     out of free text. It costs a second generation as long as the request.
//   - gate answers with one word and never edits. It is cheap and more reliable.
//   - pattern runs no model: regular expressions replace or refuse. It suits
//     keys and card numbers but cannot recognise a name.
//
// The zero value is rewrite. See docs/filters.md.
type FilterMode string

const (
	FilterModeRewrite FilterMode = "rewrite"
	FilterModeGate    FilterMode = "gate"
	FilterModePattern FilterMode = "pattern"
)

var filterModes = []FilterMode{FilterModeRewrite, FilterModeGate, FilterModePattern}

// Valid reports whether m names a mode. Empty means rewrite.
func (m FilterMode) Valid() bool { return m == "" || slices.Contains(filterModes, m) }

func (m FilterMode) Gates() bool { return m == FilterModeGate }

// Rewrites reports whether the filter may edit the request. A pattern filter
// counts, whatever its rules say.
func (m FilterMode) Rewrites() bool { return !m.Gates() }

func (m FilterMode) UsesModel() bool { return m != FilterModePattern }

func (f Filter) UsesModel() bool { return f.Mode.UsesModel() }

// ReadsWith is the model the filter reads with, or "" for a pattern filter.
// The allow-list and subscription checks use it.
func (f Filter) ReadsWith() string {
	if !f.UsesModel() {
		return ""
	}
	return f.Model
}

// MaxFilterRules bounds a pattern filter. Each rule runs on every segment of
// every request.
const MaxFilterRules = 64

// maxFilterPatternLen only keeps rules readable: RE2 cannot backtrack badly.
const maxFilterPatternLen = 512

// FilterRule is one rule of a pattern filter. It either replaces or refuses,
// never both.
type FilterRule struct {
	// Pattern is matched against each segment of the request.
	Pattern string `json:"pattern"`
	// Replace is inserted literally: '$1' is not expanded, to stay predictable.
	Replace string `json:"replace,omitempty"`
	Refuse  bool   `json:"refuse,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// MaxRefusalReasonBytes bounds a refusal's reason. It goes into an error body,
// a usage row and a log line, and a prompt can talk a model into a long one.
const MaxRefusalReasonBytes = 240

// Valid says what is wrong with the rule. Messages name the expression, not
// its index, for the rule's author.
func (r FilterRule) Valid() error {
	switch {
	case strings.TrimSpace(r.Pattern) == "":
		return errors.New("a rule needs an expression to match")
	case len(r.Pattern) > maxFilterPatternLen:
		return fmt.Errorf("the expression is %d bytes; the limit is %d",
			len(r.Pattern), maxFilterPatternLen)
	case r.Refuse && r.Replace != "":
		return fmt.Errorf("the rule %q both replaces and refuses; a rule does one or the "+
			"other, because a credential redacted out of a request that is then dropped "+
			"was redacted for nobody", r.Pattern)
	case len(r.Reason) > MaxRefusalReasonBytes:
		return fmt.Errorf("the reason on %q is %d bytes; the limit is %d",
			r.Pattern, len(r.Reason), MaxRefusalReasonBytes)
	case !r.Refuse && r.Reason != "":
		return fmt.Errorf("the rule %q carries a reason and does not refuse; a reason is the "+
			"sentence a refusal gives the sender, and a rule that replaces gives none",
			r.Pattern)
	}
	re, err := regexp.Compile(r.Pattern)
	if err != nil {
		return fmt.Errorf("the expression %q is not a valid regular expression: %w",
			r.Pattern, err)
	}
	// A match on the empty string matches at every position.
	if re.MatchString("") {
		return fmt.Errorf("the expression %q matches the empty string, so it matches every "+
			"request and every position in it. Anchor it or require at least one character",
			r.Pattern)
	}
	return nil
}

// Route is how a route step picks the model that answers a request. A Filter
// changes a request; a route step changes where it goes.
//
// It has no shadow mode, because something has to answer. It has Fallback
// instead. Router.Route builds it from a step.
type Route struct {
	OrgID  string
	Router string
	Step   string
	// Mode decides what the other fields mean.
	Mode RouteMode
	// Model decides on an instruction step, the only mode with Model and
	// Prompt. It should be fast and local: it runs on every request, and a
	// hosted one sends the prompt out before deciding whether it may leave.
	Model  string
	Prompt string
	// Destinations are the models the step may choose. A fallback step tries
	// them in order; latency and least-busy use the order to break ties.
	// Allowing the router allows every destination.
	Destinations []string
	// Ceilings is a size step's largest request per destination, in tokens
	// estimated from bytes. Exactly one destination has no ceiling and takes
	// what is too big for the rest. See docs/routers.md.
	Ceilings map[string]int
	// Header is the request header a header step reads, and Values maps its
	// values to destinations.
	Header string
	Values map[string]string
	// Fallback is where a step sends a request it could not decide. Empty
	// refuses it: a step that keeps prompts in the cluster must not fall back.
	Fallback string
}

func (r Route) Refuses() bool { return r.Fallback == "" }

// RouteMode is what chooses the model that answers a request.
//
// An instruction step has a small local model read the request and name a
// destination. It is the only mode that costs a generation, and the only one a
// prompt can argue with. The others never read the text:
//   - fallback tries destinations in the written order.
//   - latency tries the one that lately answered fastest first.
//   - least-busy tries the one with fewest requests in flight first.
//   - size picks the destination meant for the request's length.
//   - header sends the request where one of its headers says.
//
// Least-busy suits destinations that are alike; latency suits ones that are
// not. Both use only what this process has seen. See internal/gateway/load.go.
//
// The zero value is instruction.
type RouteMode string

const (
	RouteModeInstruction RouteMode = "instruction"
	RouteModeFallback    RouteMode = "fallback"
	RouteModeLatency     RouteMode = "latency"
	RouteModeLeastBusy   RouteMode = "least-busy"
	RouteModeSize        RouteMode = "size"
	RouteModeHeader      RouteMode = "header"
)

var routeModes = []RouteMode{
	RouteModeInstruction, RouteModeFallback, RouteModeLatency, RouteModeLeastBusy,
	RouteModeSize, RouteModeHeader,
}

// Valid reports whether m names a mode. Empty means instruction.
func (m RouteMode) Valid() bool { return m == "" || slices.Contains(routeModes, m) }

// Decides reports whether this mode asks a model to choose.
func (m RouteMode) Decides() bool { return m == "" || m == RouteModeInstruction }

// Chooses reports whether this mode picks one destination, and so has a
// Fallback. The other modes try their destinations in turn.
func (m RouteMode) Chooses() bool { return m.Decides() || m == RouteModeHeader }

// Chooses reports whether this step picks one destination before sending. An
// instruction step with one destination does not.
func (r Route) Chooses() bool { return r.Decides() || r.Mode == RouteModeHeader }

// Decides reports whether this step asks a model. With one destination there
// is nothing to ask.
func (r Route) Decides() bool { return r.Mode.Decides() && len(r.Destinations) > 1 }

// ReadsWith is the model the step reads with, or "". An instruction step with
// one destination still counts its model for the allow-list and subscription
// checks, since it decides once it has a second.
func (r Route) ReadsWith() string {
	if !r.Mode.Decides() {
		return ""
	}
	return r.Model
}

// Measures reports whether this mode orders destinations by what this process
// observed. Such an order reflects one replica, so reports leave it out.
func (m RouteMode) Measures() bool {
	return m == RouteModeLatency || m == RouteModeLeastBusy
}

func (r Route) Measures() bool { return r.Mode.Measures() }

func (m RouteMode) Sizes() bool { return m == RouteModeSize }

func (r Route) Sizes() bool { return r.Mode.Sizes() }

// Ceiling returns a destination's ceiling, and whether it has one.
func (r Route) Ceiling(alias string) (int, bool) {
	n, ok := r.Ceilings[alias]
	return n, ok && n > 0
}

// Unbounded names the destinations with no ceiling. Saving a router checks
// there is exactly one.
func (r Route) Unbounded() []string {
	out := make([]string, 0, 1)
	for _, alias := range r.Destinations {
		if _, bounded := r.Ceiling(alias); !bounded {
			out = append(out, alias)
		}
	}
	return out
}

// maxAliasLen bounds an alias. People type them, and `keera connect` writes
// them unquoted into editor configuration.
const maxAliasLen = 64

// MaxHeaderValues bounds how many values a header step knows.
const MaxHeaderValues = 32

// MaxDestinations bounds a route step. The list goes into every instruction,
// and a small model picks poorly among too many.
const MaxDestinations = 16

// ValidAlias reports whether s can name a model, router, filter or MCP server.
// They share one shape: models and routers go in the same field, and an MCP
// alias is part of a URL.
func ValidAlias(s string) bool { return validName(s, maxAliasLen) }

// ValidModelAlias also allows a provider prefix, as in
// "anthropic/claude-sonnet-4-5".
func ValidModelAlias(s string) bool {
	if len(s) > maxAliasLen {
		return false
	}
	for part := range strings.SplitSeq(s, "/") {
		if !validName(part, maxAliasLen) {
			return false
		}
	}
	return true
}

// LocationOnPrem is the location of a model the deployment serves itself.
const LocationOnPrem = "onprem"

// ValidLocation takes any short name, not a fixed list, so a new country needs
// no new build.
func ValidLocation(s string) bool { return validName(s, 16) }

func ValidReleaseDate(s string) bool {
	_, err := time.Parse(time.DateOnly, s)
	return err == nil
}

// validName allows lowercase letters, digits, and inner hyphens.
func validName(s string, maxLen int) bool {
	if s == "" || len(s) > maxLen {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0 && i < len(s)-1:
		default:
			return false
		}
	}
	return true
}

// Limits is one row of the guardrails table. A nil field inherits from the
// level above.
//
// The allow-lists have no omitempty: an empty list allows nothing, while null
// inherits.
type Limits struct {
	AllowedModels []string `json:"allowed_models"`
	// RateLimits and Budgets hold at most one per period.
	RateLimits []RateLimit `json:"rate_limits,omitempty"`
	Budgets    []Budget    `json:"budgets,omitempty"`
	// SystemPrompt goes before a chat request's messages. A level can add to
	// what it inherits but never remove it.
	SystemPrompt *string `json:"system_prompt,omitempty"`
	// Filters are aliases in run order. A level may add but never drop them.
	Filters []string `json:"filters,omitempty"`
	// AllowedTools lists MCP servers by alias for all their tools, or
	// 'alias/tool' for one. Nil inherits.
	AllowedTools []string `json:"allowed_tools"`
	// Embedded so a row stays one flat object in JSON, SQL and the CLI.
	SandboxLimits
}

// RateLimit is the most a scope may use per Period. Zero means no limit.
type RateLimit struct {
	Requests int64 `json:"requests,omitempty"`
	Tokens   int64 `json:"tokens,omitempty"`
	// Period is minute, day or month. Empty is a minute.
	Period Period `json:"period,omitempty"`
}

func (r RateLimit) Per() Period {
	if r.Period == "" {
		return PeriodMinute
	}
	return r.Period
}

// Budget is the most a scope may spend per Period.
type Budget struct {
	BudgetMicros int64 `json:"budget_micros"`
	// Period is day or month. Empty is a month.
	Period Period `json:"period,omitempty"`
}

func (b Budget) Per() Period {
	if b.Period == "" {
		return PeriodMonth
	}
	return b.Period
}

func (r RateLimit) PerMinute() bool { return r.Per() == PeriodMinute }

// Problem says what is wrong, in words that follow "a rate limit", or "".
func (r RateLimit) Problem() string {
	switch {
	case !r.PerMinute() && !r.Period.Valid():
		return "has the period '" + string(r.Period) + "'; use minute, day or month"
	case r.Requests < 0 || r.Tokens < 0 || r.Requests == 0 && r.Tokens == 0:
		return "needs a positive 'requests', 'tokens' or both"
	case r.PerMinute() && max(r.Requests, r.Tokens) > MaxPerMinute:
		return fmt.Sprintf("allows more than %d a minute; use a 'period' of day or month "+
			"for a limit that large", MaxPerMinute)
	}
	return ""
}

// Problem says what is wrong, in words that follow "a budget", or "".
func (b Budget) Problem() string {
	switch {
	case !b.Per().Valid():
		return "has the period '" + string(b.Period) + "'; use day or month"
	case b.BudgetMicros <= 0:
		return "needs a positive 'budget_micros'"
	}
	return ""
}

// count sets a per-minute limit on the month's scope, and returns quotas for
// one per day or month.
func (r RateLimit) count(month *Scope) []Scope {
	if p := r.Per(); p.Valid() {
		return quotas(month.Type, month.ID, p, r.Requests, r.Tokens)
	}
	month.RPM = int(min(r.Requests, MaxPerMinute))
	month.TPM = int(min(r.Tokens, MaxPerMinute))
	return nil
}

// IsZero compares with the zero value, so a new field needs no change here.
func (l Limits) IsZero() bool { return reflect.DeepEqual(l, Limits{}) }

// Scope is one level's limits, carried on a resolved key so each level is
// enforced separately.
type Scope struct {
	Type ScopeType `json:"type"`
	ID   string    `json:"id"`
	// Zero means unlimited. A rate limit per day or month is a scope in
	// Resolved.Quotas. A budget per day gets a second scope without RPM or TPM.
	RPM          int    `json:"rpm"`
	TPM          int    `json:"tpm"`
	BudgetMicros int64  `json:"budget_micros"`
	Period       Period `json:"period"`
	// Meter is what the scope counts: money when empty, or requests or tokens
	// for a daily or monthly rate limit. Quota is the most; zero is no limit.
	Meter Meter `json:"meter,omitempty"`
	Quota int64 `json:"quota,omitempty"`
}

// Meter is what a scope's window counts.
type Meter string

const (
	MeterCost     Meter = ""
	MeterRequests Meter = "requests"
	MeterTokens   Meter = "tokens"
)

// Amount is what one request adds to a window of this meter.
func (m Meter) Amount(costMicros, tokens int64) int64 {
	switch m {
	case MeterRequests:
		return 1
	case MeterTokens:
		return tokens
	}
	return costMicros
}

// KeyKind says what a key may reach.
type KeyKind string

const (
	KeyStandard KeyKind = "standard"
	// KeySubscription reaches only subscription models. It sits in a settings
	// file in plain text, so a copy must not spend the organisation's money.
	KeySubscription KeyKind = "subscription"
)

// Valid reports whether k is a kind of key. Empty is not.
func (k KeyKind) Valid() bool { return k == KeyStandard || k == KeySubscription }

// Key identifies the principal behind a request.
type Key struct {
	ID        string
	OrgID     string
	ProjectID string
	UserID    string
	Kind      KeyKind
}

func (k Key) Subscription() bool { return k.Kind == KeySubscription }

// PlanUsage is how much of a Claude plan's limit is used, as a fraction from
// 0 to 1, as Anthropic reports it on each answer.
type PlanUsage struct {
	FiveHour         *float64   `json:"five_hour,omitempty"`
	FiveHourResetsAt *time.Time `json:"five_hour_resets_at,omitempty"`
	SevenDay         *float64   `json:"seven_day,omitempty"`
	SevenDayResetsAt *time.Time `json:"seven_day_resets_at,omitempty"`
	// Status is allowed, allowed_warning or rejected.
	Status    string    `json:"status,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Resolved is a key with its policy chain already combined, so the gateway
// never walks the hierarchy on the hot path.
type Resolved struct {
	Key Key
	// AllowedModels nil means every enabled model of the organisation.
	AllowedModels []string
	// SystemPrompt is every level's prompt joined, outermost first.
	SystemPrompt string
	// Filters are every level's filters in run order, without repeats. They
	// are aliases so a filter's definition is not frozen into a cached key.
	Filters []string
	// AllowedTools nil means every tool of every MCP server of the organisation.
	AllowedTools []string
	// Scopes run outermost first: org, project if any, then key. Each level
	// has a month scope, which carries its per-minute rate limit, and a day
	// scope when it has a daily budget.
	Scopes []Scope
	// Quotas are the rate limits per day or month. They are apart from Scopes
	// because a request counts against them only once let through.
	Quotas  []Scope
	Sandbox ResolvedSandbox
}

func (r *Resolved) allowsModel(alias string) bool {
	return r.AllowedModels == nil || slices.Contains(r.AllowedModels, alias)
}

// MayUse reports whether the key may call m. A subscription model takes only a
// subscription key, because only its requests carry the caller's sign-in.
func (r *Resolved) MayUse(m Model) bool {
	return r.Key.Subscription() == m.Subscription && r.allowsModel(m.Alias)
}

// MayCallRouter reports whether the key may name the router. Allowing a router
// allows where it sends. A subscription key may call none, since a router
// would put it on models the organisation pays for.
func (r *Resolved) MayCallRouter(alias string) bool {
	return r.allowsModel(alias) && !r.Key.Subscription()
}

// Resolve combines the guardrails of a key's org, project and the key itself.
//
// A level can narrow what it inherits but never widen it: allow-lists
// intersect and ceilings take the minimum. The exceptions:
//
//   - Budgets and rate limits are kept per level, and each must hold.
//   - System prompts concatenate outermost first.
//   - Filters add up level by level.
//   - Only the organisation grants repositories; the levels below narrow them.
func Resolve(key Key, org, project, own *Limits) *Resolved {
	r := &Resolved{Key: key}

	type level struct {
		typ ScopeType
		id  string
		lim *Limits
	}
	levels := []level{{ScopeOrg, key.OrgID, org}}
	if key.ProjectID != "" {
		levels = append(levels, level{ScopeProject, key.ProjectID, project})
	}
	levels = append(levels, level{ScopeKey, key.ID, own})

	for _, lv := range levels {
		month := Scope{Type: lv.typ, ID: lv.id, Period: PeriodMonth}
		var day *Scope
		if lv.lim != nil {
			r.narrow(lv.lim, lv.typ == ScopeOrg)
			for _, rl := range lv.lim.RateLimits {
				r.Quotas = append(r.Quotas, rl.count(&month)...)
			}
			for _, b := range lv.lim.Budgets {
				if b.Per() == PeriodDay {
					day = &Scope{Type: lv.typ, ID: lv.id, Period: PeriodDay, BudgetMicros: b.BudgetMicros}
				} else {
					month.BudgetMicros = b.BudgetMicros
				}
			}
		}
		// The month scope exists even without a budget: spend is counted on it.
		r.Scopes = append(r.Scopes, month)
		if day != nil {
			r.Scopes = append(r.Scopes, *day)
		}
	}
	return r
}

// narrow applies one level's limits. top is the organisation, the only level
// that grants repositories.
func (r *Resolved) narrow(lim *Limits, top bool) {
	r.AllowedModels = intersect(r.AllowedModels, lim.AllowedModels)
	r.SystemPrompt = JoinPrompts(r.SystemPrompt, deref(lim.SystemPrompt))
	r.Filters = appendFilters(r.Filters, lim.Filters)
	r.AllowedTools = intersectTools(r.AllowedTools, lim.AllowedTools)
	r.Sandbox.narrow(lim, top)
}

// intersect narrows an allow-list. A nil list means unrestricted, not empty.
func intersect(cur, next []string) []string {
	return narrowAllowList(cur, next, func(a, b string) bool { return a == b })
}

func narrowAllowList(cur, next []string, covers func(a, b string) bool) []string {
	switch {
	case next == nil:
		return cur
	case cur == nil:
		return slices.Clone(next)
	}
	return narrowList(cur, next, covers)
}

// narrowList keeps what both lists allow: the narrower of two matching entries.
func narrowList(cur, next []string, covers func(a, b string) bool) []string {
	out := []string{}
	for _, a := range cur {
		for _, b := range next {
			var keep string
			switch {
			case covers(a, b):
				keep = b
			case covers(b, a):
				keep = a
			default:
				continue
			}
			if !slices.Contains(out, keep) {
				out = append(out, keep)
			}
		}
	}
	return out
}

// JoinPrompts appends a system prompt, with a blank line between.
func JoinPrompts(cur, next string) string {
	next = strings.TrimSpace(next)
	switch {
	case next == "":
		return cur
	case cur == "":
		return next
	default:
		return cur + "\n\n" + next
	}
}

// appendFilters drops repeats. The first mention wins, so an organisation's
// filter runs before its projects'.
func appendFilters(cur, next []string) []string {
	for _, alias := range next {
		if alias != "" && !slices.Contains(cur, alias) {
			cur = append(cur, alias)
		}
	}
	return cur
}

// minPositive treats zero or less as no limit.
func minPositive(a, b int) int {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	default:
		return min(a, b)
	}
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// ErrBudgetExceeded says which budget is spent, how much, and when it resets,
// since a developer reads it in their editor.
type ErrBudgetExceeded struct {
	Scope  Scope
	Spent  int64
	Budget int64
	// ResetsAt is the start of the next period. Zero leaves it out.
	ResetsAt time.Time
}

func (e *ErrBudgetExceeded) Error() string {
	msg := fmt.Sprintf("%s has spent its %s budget: %s of %s",
		e.Scope.Type.Possessive(), e.Scope.Period,
		FormatMoney(e.Spent), FormatMoney(e.Budget))
	if !e.ResetsAt.IsZero() {
		msg += ". It resets on " + e.ResetsAt.UTC().Format("2 January 2006") + " (UTC)"
	}
	return msg
}

// Possessive names a scope for the developer it stopped, without an id.
func (t ScopeType) Possessive() string {
	switch t {
	case ScopeOrg:
		return "your organisation"
	case ScopeProject:
		return "your project"
	case ScopeRouter:
		return "this router"
	default:
		return "this API key"
	}
}

// FormatMicros renders micro-units exactly, with at least two decimals, for
// prices, exports and headers. A billed 0.125 must not read as 0.13.
func FormatMicros(micros int64) string {
	return twoDecimals(strconv.FormatFloat(float64(micros)/1e6, 'f', -1, 64))
}

// FormatAmount renders money for people: two decimals from 1 up, and two
// significant digits below, so a small cost does not print as "0.00".
func FormatAmount(micros int64) string {
	v := float64(micros) / 1e6
	switch {
	case micros == 0:
		return "0.00"
	case math.Abs(v) >= 1:
		return strconv.FormatFloat(v, 'f', 2, 64)
	}
	// 'g' may write an exponent, so the rounded value is written again.
	rounded, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'g', 2, 64), 64)
	return twoDecimals(strconv.FormatFloat(rounded, 'f', -1, 64))
}

func twoDecimals(s string) string {
	dot := strings.IndexByte(s, '.')
	switch {
	case dot < 0:
		return s + ".00"
	case len(s)-dot-1 < 2:
		return s + "0"
	default:
		return s
	}
}

// FormatMoney is FormatAmount with the currency first: CHF 3.00.
func FormatMoney(micros int64) string {
	return Currency + " " + FormatAmount(micros)
}

// FormatPrice is FormatMicros with the currency first.
func FormatPrice(micros int64) string {
	return Currency + " " + FormatMicros(micros)
}

// Errors returned by a Source.
var (
	ErrUnknownKey = errors.New("policy: unknown key")
	ErrKeyExpired = errors.New("policy: key expired")
	ErrKeyRevoked = errors.New("policy: key revoked")
	// ErrKeyPending is a key not yet activated. It has no secret, so only
	// lookups by id, such as the playground, meet it.
	ErrKeyPending = errors.New("policy: key not activated")
)

// Source is what the gateway needs from the control plane. It is an interface
// so the hot path can be tested without a database.
type Source interface {
	// Resolve runs on every inference request, so implementations should cache.
	Resolve(ctx context.Context, presented string) (*Resolved, error)
	Model(orgID, alias string) (Model, bool)
	Models(orgID string) []Model
	// An unknown filter alias refuses the request rather than skipping it.
	Filter(orgID, alias string) (Filter, bool)
	// Router is asked on every request: only it says whether 'model' names a
	// model or a router.
	Router(orgID, alias string) (Router, bool)
	Routers(orgID string) []Router
	MCPServer(orgID, alias string) (MCPServer, bool)
}
