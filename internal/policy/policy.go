// Package policy holds Keera Gateway's tenancy model: the org → team → key
// hierarchy, the limits that attach at each level, and the rule for combining
// them.
package policy

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ScopeType names a level of the hierarchy that a policy can attach to.
type ScopeType string

// The levels of the hierarchy, outermost first.
const (
	ScopeOrg  ScopeType = "org"
	ScopeTeam ScopeType = "team"
	ScopeKey  ScopeType = "key"
)

// Period is the window a budget resets on.
type Period string

// The periods a budget can reset on.
const (
	PeriodDay   Period = "day"
	PeriodMonth Period = "month"
)

// Start returns the first instant of the period containing t. Budgets reset on
// UTC boundaries in every deployment.
func (p Period) Start(t time.Time) time.Time {
	t = t.UTC()
	if p == PeriodMonth {
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// Next returns the start of the period after the one containing t: when a
// spent budget works again.
func (p Period) Next(t time.Time) time.Time {
	start := p.Start(t)
	if p == PeriodMonth {
		return start.AddDate(0, 1, 0)
	}
	return start.AddDate(0, 0, 1)
}

// Valid reports whether p is a period Keera Gateway knows how to reset.
func (p Period) Valid() bool { return p == PeriodDay || p == PeriodMonth }

// Kind is the API surface a model is served on. A chat model cannot be reached
// through /v1/embeddings and vice versa.
type Kind string

// The API surfaces a model can be served on.
const (
	KindChat       Kind = "chat"
	KindCompletion Kind = "completion"
	KindEmbedding  Kind = "embedding"
)

// Valid reports whether k is a surface Keera Gateway serves.
func (k Kind) Valid() bool {
	return k == KindChat || k == KindCompletion || k == KindEmbedding
}

// Model is one entry in the model catalogue. Clients only ever name the Alias,
// so the model behind it can change without any client changing.
type Model struct {
	Alias        string   `json:"alias"`
	Kind         Kind     `json:"kind"`
	Backends     []string `json:"backends"`
	BackendModel string   `json:"backend_model"`
	// Provider is the hosted provider this entry was declared from, or empty
	// for a model you serve yourself. The inference path never reads it; it
	// lets an edit screen show the provider's values.
	Provider string `json:"provider,omitempty"`
	// Description says what the model is for. Clients see it on /v1/models,
	// and a Router reads it when choosing.
	Description string `json:"description,omitempty"`
	// Prices are micro-units per million tokens, so every money value stays an
	// integer.
	InputMicrosPerMTok  int64 `json:"input_micros_per_mtok"`
	OutputMicrosPerMTok int64 `json:"output_micros_per_mtok"`
	// CachedInputMicrosPerMTok is the price of an input token the provider
	// served from its prompt cache. Providers discount these heavily, and a
	// coding agent's prompt is mostly cached, so charging the full input price
	// overstates cost a lot. Zero means "not stated": see cachedInputMicros.
	CachedInputMicrosPerMTok int64 `json:"cached_input_micros_per_mtok"`
	// MaxContext is advertised on /v1/models, and a request that cannot fit in
	// it is refused. Without the tokeniser the check counts the least a request
	// can be, so a request near the limit is left to the inference plane.
	MaxContext int `json:"max_context"`
	// APIKeyEnv names the environment variable holding the backend's
	// credential. The secret stays out of the catalogue, so it can come from a
	// Kubernetes Secret or a vault, and several models can share it.
	APIKeyEnv string `json:"api_key_env,omitempty"`
	// APIKeyCiphertext is a credential set through the control plane, sealed
	// with a key only the gateway's environment holds.
	APIKeyCiphertext []byte `json:"-"`
	// APIKey is that credential decrypted when the catalogue loads. It lives
	// only in memory.
	APIKey string `json:"-"`
	// HasAPIKey tells the control panel a credential is stored, and nothing
	// more about it.
	HasAPIKey bool `json:"has_api_key,omitempty"`
	Enabled   bool `json:"enabled"`
	// Managed means the catalogue file declares this model. Only applying the
	// file may change it, since the next restart would undo any other edit.
	// It is derived from what is stored, never set by a caller.
	Managed bool `json:"managed"`
}

// SameDeclaration reports whether two entries match in every field a catalogue
// file can state. The credential is not one of them, so setting one is the only
// change a file-managed model accepts.
func (m Model) SameDeclaration(other Model) bool {
	return m.Alias == other.Alias && m.Kind == other.Kind &&
		slices.Equal(m.Backends, other.Backends) && m.BackendModel == other.BackendModel &&
		m.Provider == other.Provider &&
		m.InputMicrosPerMTok == other.InputMicrosPerMTok &&
		m.OutputMicrosPerMTok == other.OutputMicrosPerMTok &&
		m.CachedInputMicrosPerMTok == other.CachedInputMicrosPerMTok &&
		m.MaxContext == other.MaxContext && m.APIKeyEnv == other.APIKeyEnv &&
		m.Description == other.Description && m.Enabled == other.Enabled
}

// Credential returns the secret to present to this backend. A key typed into
// the control panel wins over one from the environment, because the operator
// meant it to be used.
func (m Model) Credential(fromEnv func(string) string) string {
	if m.APIKey != "" {
		return m.APIKey
	}
	if m.APIKeyEnv != "" && fromEnv != nil {
		return fromEnv(m.APIKeyEnv)
	}
	return ""
}

// Cost returns what the given token counts cost, in micro-units.
// cachedInputTokens is part of inputTokens, not added to it: that is how
// providers report it.
func (m Model) Cost(inputTokens, cachedInputTokens, outputTokens int) int64 {
	const perM = 1_000_000
	// The counts come from a provider's response. Clamp them so a bad cached
	// count cannot produce a negative cost and refund a budget.
	cached := min(max(cachedInputTokens, 0), inputTokens)
	// Divide once at the end: dividing each term would floor three times and
	// always round the total down further.
	return (int64(inputTokens-cached)*m.InputMicrosPerMTok +
		int64(cached)*m.cachedInputMicros() +
		int64(outputTokens)*m.OutputMicrosPerMTok) / perM
}

// cachedInputMicros is the rate for one cached input token: the stated rate, or
// the full input rate when none is stated.
//
// Here zero means "not stated", not "free". Models declared before this field
// existed have a zero in it, and treating that as free would suddenly drop most
// of every prompt out of every budget. The input rate errs high, as the gateway
// did before.
func (m Model) cachedInputMicros() int64 {
	if m.CachedInputMicrosPerMTok == 0 {
		return m.InputMicrosPerMTok
	}
	return m.CachedInputMicrosPerMTok
}

// Filter is a guardrail that reads a prompt before it is forwarded, and may
// change or stop it. It keeps credentials, customer names and client data from
// reaching a model outside the cluster. Its alias is scoped to an organisation.
type Filter struct {
	OrgID string `json:"org_id"`
	Alias string `json:"alias"`
	// Model is the model the filter runs on. It should run inside the same
	// infrastructure, or the prompt has left the cluster before it was
	// redacted. A pattern filter has none.
	Model  string     `json:"model"`
	Mode   FilterMode `json:"mode"`
	Prompt string     `json:"prompt"`
	// Rules are a pattern filter's whole definition, applied in order. Every
	// other mode has none.
	Rules []FilterRule `json:"rules,omitempty"`
	// Shadow runs the filter but enforces nothing: the request goes on as sent
	// and what the filter would have done is recorded. It lets an instruction
	// be tested on real traffic first. The filter still costs the same.
	Shadow bool `json:"shadow,omitempty"`
	// Description tells an administrator what the filter does.
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// FilterMode is what a filter does with the request it has read. Every mode can
// stop a request; they differ in what they cost.
//
// A rewrite filter answers with the whole conversation written back, which is
// the only way to take a credential out of free text. It costs a second
// generation as long as the request.
//
// A gate answers with one word and never edits the request. It costs a few
// tokens and judges more reliably than a rewrite filter.
//
// A pattern filter runs no model. It applies regular expressions to each
// segment, replacing matches or refusing the request. It suits things with a
// shape, like API keys, IBANs and card numbers, and is cheap enough to put in
// front of everything. It cannot recognise a name in a sentence, so the usual
// setup is one of each. See docs/filters.md.
//
// The zero value is a rewrite filter.
type FilterMode string

const (
	// FilterModeRewrite edits the request and lets it go.
	FilterModeRewrite FilterMode = "rewrite"
	// FilterModeGate lets the request go untouched or refuses it.
	FilterModeGate FilterMode = "gate"
	// FilterModePattern applies a list of rules, with no model.
	FilterModePattern FilterMode = "pattern"
)

// FilterModes is every mode a filter may run in.
var FilterModes = []FilterMode{FilterModeRewrite, FilterModeGate, FilterModePattern}

// ValidFilterMode reports whether s names a mode. The empty string does: it
// means rewrite.
func ValidFilterMode(s FilterMode) bool {
	return s == "" || slices.Contains(FilterModes, s)
}

// Rewrites reports whether this filter edits the request rather than only
// judging it. A pattern filter counts, whatever its rules say.
func (m FilterMode) Rewrites() bool { return m != FilterModeGate }

// UsesModel reports whether this filter runs a model. Only a pattern filter
// does not.
func (m FilterMode) UsesModel() bool { return m != FilterModePattern }

// UsesModel reports whether this filter decides with a model.
func (f Filter) UsesModel() bool { return f.Mode.UsesModel() }

// MaxFilterRules bounds the rules in one pattern filter. Each runs on every
// segment of every request, and a longer list is too long to read.
const MaxFilterRules = 64

// MaxFilterPatternLen bounds one rule's expression. RE2 cannot backtrack
// badly, so this is only about keeping rules readable.
const MaxFilterPatternLen = 512

// FilterRule is one rule of a pattern filter: an expression, and what happens
// to a segment it matches. A rule either replaces or refuses, never both.
type FilterRule struct {
	// Pattern is an RE2 expression in Go's regexp syntax, matched against each
	// segment of the request.
	Pattern string `json:"pattern"`
	// Replace is inserted literally: '$1' stays those two characters. The mode
	// exists to be predictable, and expansion would surprise.
	Replace string `json:"replace,omitempty"`
	// Refuse drops the request when this rule matches, instead of replacing.
	Refuse bool `json:"refuse,omitempty"`
	// Reason is the sentence a refusal gives the sender.
	Reason string `json:"reason,omitempty"`
}

// MaxRefusalReasonBytes bounds the sentence a refusal gives the sender. It ends
// up in an error body, a usage row and a log line, and a prompt can talk a
// model into writing a long one.
const MaxRefusalReasonBytes = 240

// Valid reports whether the gateway could apply this rule, and what is wrong
// when it could not. The message names the expression, not its index, because
// the rule's author reads it.
func (r FilterRule) Valid() error {
	switch {
	case strings.TrimSpace(r.Pattern) == "":
		return errors.New("a rule needs an expression to match")
	case len(r.Pattern) > MaxFilterPatternLen:
		return fmt.Errorf("the expression is %d bytes; the limit is %d",
			len(r.Pattern), MaxFilterPatternLen)
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
	// An expression that matches the empty string matches at every position,
	// so it would refuse everything or fill a request with replacements.
	if re.MatchString("") {
		return fmt.Errorf("the expression %q matches the empty string, so it matches every "+
			"request and every position in it. Anchor it or require at least one character",
			r.Pattern)
	}
	return nil
}

// Enforces reports whether this filter's answer is acted on. A shadow filter's
// is only recorded.
func (f Filter) Enforces() bool { return !f.Shadow }

// Router decides which model answers a request. A Filter changes a request; a
// router changes where it goes.
//
// Most requests do not need the largest model, and some must not reach the one
// outside the cluster. A client cannot judge that about its own prompt.
//
// A client uses a router by naming it in the 'model' field, like a model. To
// route everything a scope sends, narrow its AllowedModels to the router.
//
// A router has no shadow mode, because the client named it and something has to
// answer. It has Fallback instead.
//
// Only an instruction router reads the request with a model; see RouterMode for
// the others. The alias is scoped to an organisation and shares a namespace
// with model aliases, because clients name both in the same field.
type Router struct {
	OrgID string `json:"org_id"`
	Alias string `json:"alias"`
	// Mode decides what the other fields mean. See RouterMode.
	Mode RouterMode `json:"mode"`
	// Model makes the decision on an instruction router. It should be fast and
	// run inside the same infrastructure: it runs on every request, and a
	// hosted one sends the prompt out before deciding whether it may leave.
	// Only an instruction router has Model and Prompt.
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	// Destinations are the models this router may choose between, in the order
	// offered. A fallback router tries them in this order; latency and
	// least-busy routers use it only to break ties. Granting a router grants
	// every destination.
	Destinations []string `json:"destinations"`
	// Ceilings is a size router's whole configuration: the largest request, in
	// estimated tokens, each destination should take. A destination without a
	// positive ceiling takes what is too big for the rest, and a size router
	// needs exactly one. The estimate comes from the text's bytes, not a
	// tokeniser; see docs/routers.md.
	Ceilings map[string]int `json:"ceilings,omitempty"`
	// Fallback is where an instruction router sends a request it could not
	// decide. Empty refuses it. Neither is right in general: a router that
	// saves money can fall back, one that keeps prompts in the cluster cannot.
	// See docs/routers.md.
	Fallback string `json:"fallback,omitempty"`
	// Description tells an administrator what the router does.
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Refuses reports whether this router stops a request it could not decide
// instead of using a fallback.
func (r Router) Refuses() bool { return r.Fallback == "" }

// Offers reports whether alias is a destination this router may choose. Every
// decision is checked against it, because a model's answer is not trusted.
func (r Router) Offers(alias string) bool { return slices.Contains(r.Destinations, alias) }

// RouterMode is what chooses the model that answers a request.
//
// An instruction router has a small local model read the request and name a
// destination. It is the only mode that costs a generation, and the only one a
// prompt can argue with.
//
// Fallback, latency and least-busy routers never read the request. They suit
// several models that can serve the same traffic, and differ only in order:
//   - fallback tries destinations in the written order.
//   - latency tries the one that has answered fastest lately first.
//   - least-busy tries the one with fewest requests in flight first.
//
// Least-busy suits destinations that are alike; latency suits ones that are
// not, since queue depth cannot be compared across machines of different
// speeds. Both use only what this process has seen. See
// internal/gateway/load.go.
//
// A size router reads only the request's length and picks the destination
// meant for that size. It costs nothing and a prompt cannot sway it. A
// destination too small to hold the request always ranks last.
//
// The zero value is an instruction router.
type RouterMode string

const (
	// RouterModeInstruction reads the request with a model and sends it where
	// that model says.
	RouterModeInstruction RouterMode = "instruction"
	// RouterModeFallback tries the destinations in order and uses the first
	// one that answers.
	RouterModeFallback RouterMode = "fallback"
	// RouterModeLatency tries the destinations fastest first, by how long they
	// lately took to start answering.
	RouterModeLatency RouterMode = "latency"
	// RouterModeLeastBusy tries the destinations emptiest first, by requests
	// this gateway has in flight against each.
	RouterModeLeastBusy RouterMode = "least-busy"
	// RouterModeSize sends the request to the smallest destination meant for
	// its size that can hold it.
	RouterModeSize RouterMode = "size"
)

// RouterModes is every mode a router may run in.
var RouterModes = []RouterMode{
	RouterModeInstruction, RouterModeFallback, RouterModeLatency, RouterModeLeastBusy,
	RouterModeSize,
}

// ValidRouterMode reports whether s names a mode. The empty string does: it
// means instruction.
func ValidRouterMode(s RouterMode) bool {
	return s == "" || slices.Contains(RouterModes, s)
}

// Decides reports whether this mode asks a model to choose. Only an
// instruction router does, and only it has Model, Prompt and Fallback.
func (m RouterMode) Decides() bool { return m == "" || m == RouterModeInstruction }

// Decides reports whether this router asks a model to choose.
func (r Router) Decides() bool { return r.Mode.Decides() }

// Measures reports whether this mode orders destinations by what this process
// observed rather than the written order. Such an order is not worth recording
// in reports, since it only reflects recent traffic on one replica.
func (m RouterMode) Measures() bool {
	return m == RouterModeLatency || m == RouterModeLeastBusy
}

// Measures reports whether this router orders destinations by what this
// process observed.
func (r Router) Measures() bool { return r.Mode.Measures() }

// Sizes reports whether this mode chooses by how big the request is.
func (m RouterMode) Sizes() bool { return m == RouterModeSize }

// Sizes reports whether this router chooses by the size of the request.
func (r Router) Sizes() bool { return r.Mode.Sizes() }

// Ceiling returns the largest request, in estimated tokens, this router gives
// a destination, and whether there is one.
func (r Router) Ceiling(alias string) (int, bool) {
	n, ok := r.Ceilings[alias]
	return n, ok && n > 0
}

// Unbounded names the destinations with no ceiling: where a request larger
// than every ceiling goes. There must be exactly one, checked when the router
// is saved.
func (r Router) Unbounded() []string {
	out := make([]string, 0, 1)
	for _, alias := range r.Destinations {
		if _, bounded := r.Ceiling(alias); !bounded {
			out = append(out, alias)
		}
	}
	return out
}

// maxAliasLen bounds a model, filter or router alias. People type these, and
// `keera connect` writes them unquoted into editor configuration.
const maxAliasLen = 64

// MaxDestinations bounds how many models one router may choose between. The
// list goes into the instruction on every request, and a small model choosing
// between too many picks poorly.
const MaxDestinations = 16

// ValidFilterAlias reports whether s is an alias a filter may have.
func ValidFilterAlias(s string) bool { return validName(s, maxAliasLen) }

// ValidRouterAlias reports whether s is an alias a router may have. It has a
// model alias's shape, because clients name both in the same field.
func ValidRouterAlias(s string) bool { return validName(s, maxAliasLen) }

// ValidAlias reports whether s is a name a model may be called by.
func ValidAlias(s string) bool { return validName(s, maxAliasLen) }

// validName is the shape every name in this package shares: lowercase letters,
// digits, and hyphens that neither open nor close the name.
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

// Limits is one row of the guardrails table. A nil field means "inherit from
// the level above".
type Limits struct {
	AllowedModels []string `json:"allowed_models,omitempty"`
	// MaxOutputTokens caps what one request may generate. The gateway cannot
	// count prompt tokens without the tokeniser, but it can cap the output.
	MaxOutputTokens *int    `json:"max_output_tokens,omitempty"`
	RPM             *int    `json:"rpm,omitempty"`
	TPM             *int    `json:"tpm,omitempty"`
	BudgetMicros    *int64  `json:"budget_micros,omitempty"`
	BudgetPeriod    *Period `json:"budget_period,omitempty"`
	// SystemPrompt is put ahead of a chat request's own messages. Levels
	// concatenate rather than narrow: a level can add to what it inherits but
	// never remove it.
	SystemPrompt *string `json:"system_prompt,omitempty"`
	// Filters are aliases, in the order they run. Like SystemPrompt, a level
	// may add filters but never drop inherited ones.
	Filters []string `json:"filters,omitempty"`
	// AllowedTools is the MCP tools this scope may call through the gateway:
	// a server's alias for all of its tools, or 'alias/tool' for one. Nil
	// inherits; see Resolved.AllowsTool.
	AllowedTools []string `json:"allowed_tools,omitempty"`
	// BlockHostedTools takes out of a request the tools a hosted provider runs
	// on its own servers, such as web search or a remote MCP server, which
	// reach the outside world where no guardrail can see. Once a level sets
	// it, no level below can unset it.
	BlockHostedTools *bool `json:"block_hosted_tools,omitempty"`
	// SandboxLimits is embedded so a policy row stays one flat object in JSON,
	// SQL and on the command line.
	SandboxLimits
}

// Scope is one level of the hierarchy with its own limits, carried on a
// resolved key so the gateway can enforce each level separately.
type Scope struct {
	Type ScopeType
	ID   string
	// Zero means unlimited for all three.
	RPM          int
	TPM          int
	BudgetMicros int64
	Period       Period
}

// Key identifies the principal behind a request.
type Key struct {
	ID     string
	OrgID  string
	TeamID string
	UserID string
}

// Resolved is a key with its policy chain already combined, so the gateway
// never walks the hierarchy on the hot path.
type Resolved struct {
	Key Key
	// AllowedModels nil means every enabled model in the catalogue.
	AllowedModels   []string
	MaxOutputTokens int
	// SystemPrompt is every level's prompt joined, outermost first. Empty
	// means the conversation is forwarded as sent.
	SystemPrompt string
	// Filters is every level's filters in run order, outermost first, without
	// repeats. They are aliases so a filter's definition is read per request
	// rather than frozen into a cached key.
	Filters []string
	// AllowedTools nil means every tool of every MCP server in the catalogue.
	AllowedTools []string
	// BlockHostedTools says a request's hosted tools are taken out.
	BlockHostedTools bool
	// Scopes runs outermost first: org, team if any, then the key. Rate
	// limits and budgets are enforced at every level.
	Scopes []Scope
	// Sandbox is what this key may hold of the sandbox pool.
	Sandbox ResolvedSandbox
}

// AllowsModel reports whether alias is inside the resolved allow-list.
func (r *Resolved) AllowsModel(alias string) bool {
	return r.AllowedModels == nil || slices.Contains(r.AllowedModels, alias)
}

// Resolve combines the guardrails of a key's org, team and the key itself.
//
// A level can narrow what it inherits but never widen it: allow-lists
// intersect and ceilings take the minimum. Two exceptions:
//
//   - Budgets are kept per level and checked separately. An org cap of 10,000
//     and a team cap of 1,000 must both hold.
//   - System prompts concatenate outermost first, because text does not narrow.
func Resolve(key Key, org, team, own *Limits) *Resolved {
	r := &Resolved{Key: key}

	type level struct {
		typ ScopeType
		id  string
		lim *Limits
	}
	levels := []level{{ScopeOrg, key.OrgID, org}}
	if key.TeamID != "" {
		levels = append(levels, level{ScopeTeam, key.TeamID, team})
	}
	levels = append(levels, level{ScopeKey, key.ID, own})

	for _, lv := range levels {
		s := Scope{Type: lv.typ, ID: lv.id, Period: PeriodMonth}
		if lv.lim != nil {
			r.narrow(lv.lim)
			s.RPM = deref(lv.lim.RPM)
			s.TPM = deref(lv.lim.TPM)
			s.BudgetMicros = deref(lv.lim.BudgetMicros)
			if lv.lim.BudgetPeriod != nil && lv.lim.BudgetPeriod.Valid() {
				s.Period = *lv.lim.BudgetPeriod
			}
		}
		r.Scopes = append(r.Scopes, s)
	}
	return r
}

// narrow applies one level's limits to what the levels above it allowed.
func (r *Resolved) narrow(lim *Limits) {
	r.AllowedModels = intersect(r.AllowedModels, lim.AllowedModels)
	r.MaxOutputTokens = minPositive(r.MaxOutputTokens, deref(lim.MaxOutputTokens))
	r.SystemPrompt = joinPrompts(r.SystemPrompt, deref(lim.SystemPrompt))
	r.Filters = appendFilters(r.Filters, lim.Filters)
	r.AllowedTools = intersectTools(r.AllowedTools, lim.AllowedTools)
	r.BlockHostedTools = r.BlockHostedTools || deref(lim.BlockHostedTools)
	r.Sandbox.narrow(lim)
}

// ResolveOrg combines an organisation's limits for a caller without an API
// key, such as the control panel's playground. The organisation is the only
// scope.
func ResolveOrg(orgID, userID string, org *Limits) *Resolved {
	// Built on Resolve so the panel enforces exactly what a key would. The key
	// scope is dropped because there is no key.
	r := Resolve(Key{OrgID: orgID, UserID: userID}, org, nil, nil)
	r.Scopes = r.Scopes[:1]
	return r
}

// intersect narrows an allow-list. A nil list means unrestricted, not empty.
func intersect(cur, next []string) []string {
	switch {
	case next == nil:
		return cur
	case cur == nil:
		return slices.Clone(next)
	}
	out := make([]string, 0, min(len(cur), len(next)))
	for _, a := range cur {
		if slices.Contains(next, a) {
			out = append(out, a)
		}
	}
	return out
}

// joinPrompts appends one level's system prompt to the ones above it. The
// blank line keeps two instructions from running together.
func joinPrompts(cur, next string) string {
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

// appendFilters adds one level's filters to the ones above it, dropping
// repeats. The first mention wins, so an organisation's filter runs before its
// teams'. A second pass over the same text would find nothing new.
func appendFilters(cur, next []string) []string {
	for _, alias := range next {
		if alias != "" && !slices.Contains(cur, alias) {
			cur = append(cur, alias)
		}
	}
	return cur
}

// minPositive is the smaller of two limits, where zero or less means no limit.
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

// ErrBudgetExceeded is returned when a scope has spent its allowance for the
// period. A developer reads it in their editor, so it says which budget, how
// much, and when it resets.
type ErrBudgetExceeded struct {
	Scope  Scope
	Spent  int64
	Budget int64
	// ResetsAt is the start of the next period. Zero leaves it out of the
	// message.
	ResetsAt time.Time
}

func (e *ErrBudgetExceeded) Error() string {
	msg := fmt.Sprintf("%s has spent its %s budget: %s of %s",
		e.Scope.Type.Possessive(), e.Scope.Period,
		FormatMicros(e.Spent), FormatMicros(e.Budget))
	if !e.ResetsAt.IsZero() {
		msg += ". It resets on " + e.ResetsAt.UTC().Format("2 January 2006") + " (UTC)"
	}
	return msg
}

// Possessive names a scope the way the developer it stopped reads it, without
// an id.
func (t ScopeType) Possessive() string {
	switch t {
	case ScopeOrg:
		return "your organisation"
	case ScopeTeam:
		return "your team"
	default:
		return "this API key"
	}
}

// FormatMicros renders micro-units for a human. It keeps every significant
// digit and pads to at least two decimals, because rounding a small cost to two
// decimals would print "0.00".
func FormatMicros(micros int64) string {
	s := strconv.FormatFloat(float64(micros)/1e6, 'f', -1, 64)
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

// Errors returned by a Source.
var (
	ErrUnknownKey = errors.New("policy: unknown key")
	ErrKeyExpired = errors.New("policy: key expired")
	ErrKeyRevoked = errors.New("policy: key revoked")
)

// Source is what the gateway needs from the control plane. It is an interface
// so the hot path can be tested without a database.
type Source interface {
	// Resolve maps a presented key to its combined policy. It runs on every
	// inference request, so implementations should cache.
	Resolve(ctx context.Context, presented string) (*Resolved, error)
	Model(alias string) (Model, bool)
	// Models lists the catalogue, for /v1/models.
	Models() []Model
	// Filter looks up an organisation's filter. An unknown alias refuses the
	// request rather than skipping the filter.
	Filter(orgID, alias string) (Filter, bool)
	// Router looks up an organisation's router. It is asked on every request,
	// because only this says whether the 'model' field names a model or a
	// router.
	Router(orgID, alias string) (Router, bool)
	// Routers lists an organisation's routers, for /v1/models.
	Routers(orgID string) []Router
	// MCPServer looks up an MCP server in the catalogue.
	MCPServer(alias string) (MCPServer, bool)
}
