package gateway

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/auth"
	"github.com/bespinian/keera-gateway/internal/policy"
)

// Claude subscriptions.
//
// Claude Code signed in to a Claude Team or Enterprise plan can be pointed here
// with ANTHROPIC_BASE_URL alone. Its requests then carry the person's own Claude
// sign-in in Authorization, and the plan pays. The gateway forwards that
// sign-in unchanged to Anthropic for a subscription model, and never stores or
// logs it. It still applies the key's guardrails, and records the tokens.
//
// Who is asking comes from the Keera key in KeyHeader, which Claude Code sends
// from ANTHROPIC_CUSTOM_HEADERS. That key is a subscription key: it reaches
// subscription models only, so a copied settings file cannot spend the
// organisation's money. See docs/subscriptions.md.

// KeyHeader carries the Keera key when Authorization carries the caller's
// Claude sign-in.
const KeyHeader = "X-Keera-Key"

// claudeSignInPrefix starts the token of a Claude sign-in. An Anthropic API
// key starts differently, with sk-ant-api.
const claudeSignInPrefix = "sk-ant-oat"

// claudeSignIn reports whether the request carries a Claude sign-in.
func claudeSignIn(r *http.Request) bool {
	token, err := auth.FromHeader(r.Header.Get("Authorization"))
	return err == nil && strings.HasPrefix(token, claudeSignInPrefix)
}

// connectHint is what to run to set Claude Code up for a subscription.
const connectHint = "run `keera login`, then `keera connect claude-code --subscription`"

// subscriptionRefusal says why a request cannot reach a subscription model, or
// returns nil when it can. A subscription model answers only Claude Code's
// own API, and only with the caller's sign-in to forward.
func subscriptionRefusal(c *call) *refusal {
	switch {
	case c.surf.dialect == nil || c.surf.dialect.provider() != "anthropic":
		return &refusal{
			status: http.StatusBadRequest,
			typ:    "invalid_request_error", code: "subscription_model",
			msg: "the model '" + c.alias + "' is paid by your Claude subscription, " +
				"and answers only the Anthropic Messages API that Claude Code speaks",
		}
	case !claudeSignIn(c.r):
		return &refusal{
			status: http.StatusUnauthorized,
			typ:    "authentication_error", code: "missing_claude_sign_in",
			msg: "the model '" + c.alias + "' is paid by your Claude subscription, and this " +
				"request carries no Claude sign-in; sign in to Claude Code with `/login`, and " +
				"leave ANTHROPIC_AUTH_TOKEN and ANTHROPIC_API_KEY unset",
		}
	}
	return nil
}

// subscriptionAuth forwards the caller's sign-in and the headers Claude Code
// identifies itself with, instead of a credential of the gateway's. Anthropic
// accepts a Claude sign-in only from Claude Code, so the request has to reach
// it as Claude Code sent it. The anthropic-* headers are an open list: a new
// capability arrives as a new one.
func subscriptionAuth(client http.Header) func(http.Header, string) {
	return func(h http.Header, _ string) {
		for name, values := range client {
			if forwardedHeader(name) {
				h[name] = values
			}
		}
		if h.Get("Anthropic-Version") == "" {
			h.Set("Anthropic-Version", anthropicVersion)
		}
	}
}

// forwardedHeader reports whether a client header goes on to Anthropic with
// a subscription request. name is in canonical form.
func forwardedHeader(name string) bool {
	switch name {
	case "Authorization", "User-Agent", "X-App":
		return true
	}
	return strings.HasPrefix(name, "Anthropic-") || strings.HasPrefix(name, "X-Claude-Code-")
}

// planHeaderPrefix starts the headers in which Anthropic reports a plan's usage.
const planHeaderPrefix = "Anthropic-Ratelimit-Unified-"

// passPlanHeaders hands Anthropic's plan headers to the client. Claude Code
// reads them to show the plan's limits and to tell a plan limit from a passing
// throttle, and retries on the two retry headers.
func passPlanHeaders(w http.ResponseWriter, upstream http.Header) {
	for name, values := range upstream {
		if strings.HasPrefix(name, planHeaderPrefix) || name == "Retry-After" ||
			name == "X-Should-Retry" {
			w.Header()[name] = values
		}
	}
}

// planUsage reads how much of the plan is used off an answer's headers, or
// returns nil when the answer reports none.
func planUsage(h http.Header, now time.Time) *policy.PlanUsage {
	p := &policy.PlanUsage{
		FiveHour:         fraction(h.Get(planHeaderPrefix + "5h-Utilization")),
		FiveHourResetsAt: unixTime(h.Get(planHeaderPrefix + "5h-Reset")),
		SevenDay:         fraction(h.Get(planHeaderPrefix + "7d-Utilization")),
		SevenDayResetsAt: unixTime(h.Get(planHeaderPrefix + "7d-Reset")),
		Status:           strings.TrimSpace(h.Get(planHeaderPrefix + "Status")),
		UpdatedAt:        now,
	}
	if p.FiveHour == nil && p.SevenDay == nil && p.Status == "" {
		return nil
	}
	// The status is a short word. Anything longer is not one, and is not kept.
	if len(p.Status) > 32 {
		p.Status = ""
	}
	return p
}

// fraction reads a utilisation, which Anthropic sends as a fraction from 0 to 1.
func fraction(v string) *float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || f < 0 || f > 10 {
		return nil
	}
	return &f
}

// unixTime reads a reset time, which Anthropic sends in Unix seconds.
func unixTime(v string) *time.Time {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n <= 0 {
		return nil
	}
	t := time.Unix(n, 0).UTC()
	return &t
}

// appendSystem puts the guardrail's prompt after the client's own system
// blocks, not before. Claude Code's first block identifies it, and Anthropic
// removes that block only when it comes first.
func appendSystem(b *body, prompt string) error { return setSystem(b, prompt, false) }
