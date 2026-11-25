package gateway

import (
	"bytes"
	"cmp"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// billed reports whether a request may call any billed model: one on the
// deployment's own provider key, or one with a gateway fee. Such a request may
// need prepaid credit. It runs before anything is spent, so a model a route
// step might pick counts even if it picks another.
func (s *Server) billed(c *call) bool {
	return len(s.billedDeciders(c)) > 0 || len(s.billedAnswerers(c)) > 0 ||
		s.filtersBilled(c.res.Key.OrgID, s.callFilters(c))
}

// billedAnswerers is the billed models that may answer a request: the one it
// names, or every one its router may reach.
func (s *Server) billedAnswerers(c *call) []policy.Model {
	if !c.viaRouter() {
		if c.model.Billing != nil {
			return []policy.Model{c.model}
		}
		return nil
	}
	var out []policy.Model
	for _, alias := range c.router.Reaches() {
		if m, ok := s.billedModel(c.res.Key.OrgID, alias); ok {
			out = append(out, m)
		}
	}
	return out
}

// billedDeciders is the deciding models of a request's route steps that are
// billed.
func (s *Server) billedDeciders(c *call) []policy.Model {
	var out []policy.Model
	for _, rt := range c.router.Routes() {
		if !rt.Decides() {
			continue
		}
		if m, ok := s.billedModel(c.res.Key.OrgID, rt.Model); ok {
			out = append(out, m)
		}
	}
	return out
}

// callFilters is every filter a request may pass: its router's, then the key's.
func (s *Server) callFilters(c *call) []string {
	var out []string
	for step := range c.router.Steps() {
		if step.Kind == policy.StepFilter {
			out = append(out, step.Ref)
		}
	}
	return append(out, c.res.Filters...)
}

func (s *Server) filtersBilled(org string, filters []string) bool {
	return len(s.billedFilterModels(org, filters)) > 0
}

func (s *Server) billedFilterModels(org string, filters []string) []policy.Model {
	var out []policy.Model
	for _, alias := range filters {
		f, ok := s.src.Filter(org, alias)
		if !ok || !f.UsesModel() {
			continue
		}
		if m, ok := s.billedModel(org, f.Model); ok {
			out = append(out, m)
		}
	}
	return out
}

func (s *Server) billedModel(org, alias string) (policy.Model, bool) {
	if alias == "" {
		return policy.Model{}, false
	}
	m, ok := s.src.Model(org, alias)
	return m, ok && m.Billing != nil
}

// mostTokens is all a call may use: its whole body as prompt and its output
// ceiling as answer. A call on the deployment's key with no usage record is
// billed this, so losing the record never means paying less.
func mostTokens(bodyBytes, outputCeiling int) policy.Tokens {
	return policy.Tokens{Input: bodyBytes / bytesPerToken, Output: outputCeiling}
}

// most is all a call may use: mostTokens, or for a transcription its text
// fields and the longest audio its file can hold, in minutes and in tokens.
// prompt is the part of the input a filter reads: all of it, or a
// transcription's text fields.
func (c *call) most(b *body) (t policy.Tokens, prompt int) {
	t = mostTokens(c.received, c.outputTokens(b))
	if c.audio == nil {
		return t, t.Input
	}
	prompt = len(b.encode()) / bytesPerToken
	t.AudioMillis = mostAudioMillis(len(c.audio.data))
	t.Input = prompt + audioTokensAtMost(t.AudioMillis)
	return t, prompt
}

// billUnreported bills a request on the deployment's key that came back
// without a usage record. See mostTokens. A stream cut before its output
// count keeps its exact input, and its output is billed the ceiling. A model
// priced by the minute that reports tokens but no length is billed its
// longest audio, or it would be free.
func (s *Server) billUnreported(c *call, b *body) {
	if !c.model.PlatformKey {
		return
	}
	noTokens := !c.ev.Tokens().Any()
	noAudio := c.model.AudioMicrosPerMinute > 0 && c.ev.AudioMillis == 0
	if !noTokens && !noAudio && !c.outputOpen {
		return
	}
	t, _ := c.most(c.sent(b))
	switch {
	case noTokens:
		c.ev.InputTokens, c.ev.OutputTokens = t.Input, t.Output
	case c.outputOpen:
		c.ev.OutputTokens = max(c.ev.OutputTokens, t.Output)
	}
	c.ev.AudioMillis, c.ev.Estimated = t.AudioMillis, true
	s.log.Warn("a request on the deployment's key came back without a whole usage record; "+
		"it is billed the most it may have used", "model", c.alias,
		httpx.LogIDs(c.r.Context()))
}

// stripPremium removes and returns the fields that make a provider charge
// more than list price: Anthropic's fast mode and data residency, and dearer
// service tiers. The usage record would not show the extra cost.
func stripPremium(b *body) []string {
	var removed []string
	for _, field := range []string{"speed", "inference_geo"} {
		if b.remove(field) {
			removed = append(removed, field)
		}
	}
	if _, present := b.value("service_tier"); present {
		tier, _ := b.str("service_tier")
		if !slices.Contains(listPriceTiers, tier) {
			b.remove("service_tier")
			removed = append(removed, "service_tier")
		}
	}
	return removed
}

var listPriceTiers = []string{"auto", "default", "flex", "standard_only"}

// unstatedOutputTokens is held for a request with no output ceiling. It is not
// a bound, so the hold can fall short.
const unstatedOutputTokens = 64_000

// mostOutputTokens caps a ceiling so a nonsense one cannot overflow the hold.
const mostOutputTokens = 10_000_000

// creditHold is the most a request may take from the credit: mostTokens at the
// dearest model it may reach, plus each route step's decision twice, plus a
// full rewrite by each billed filter. It errs high and is released once the
// request is charged.
func (s *Server) creditHold(c *call, b *body) int64 {
	t, in := c.most(b)
	media := mediaIn(b)
	var most int64
	for _, m := range s.billedAnswerers(c) {
		answer := t
		answer.Input = media.mostInput(answer.Input, cmp.Or(m.MaxContext, unknownContextTokens))
		if line, ok := m.Bill(answer, answer); ok {
			most = max(most, line.CreditMicros)
		}
	}
	// An unreadable decision by letter is asked again by name.
	decision := policy.Tokens{Input: in, Output: routeOutputTokens}
	for _, m := range s.billedDeciders(c) {
		if line, ok := m.Bill(decision, decision); ok {
			most += 2 * line.CreditMicros
		}
	}
	return most + s.filtersHold(c.res.Key.OrgID, s.callFilters(c), in)
}

// unknownContextTokens is held as the prompt of a model with no stated
// context that is sent content its bytes do not bound.
const unknownContextTokens = 1_000_000

// media is what a body sends that is billed by more than its bytes. A few
// bytes can stand for a large image, or for a whole context of tokens.
type media struct {
	// images are inline images. A provider bills one by its pixels, up to
	// imageTokensAtMost.
	images int
	// unbounded is content whose cost only the model's context bounds: an
	// image, document or file the provider fetches itself, or an inline
	// document, audio or video. A compressed PDF can hold a hundred pages
	// in a few kilobytes.
	unbounded bool
}

// mostInput is the most prompt a request of input tokens by its bytes may be
// billed, at a model with a context of context tokens.
func (m media) mostInput(input, context int) int {
	switch {
	case m.unbounded:
		return max(input, context)
	case m.images > 0:
		return max(input, min(input+m.images*imageTokensAtMost, context))
	}
	return input
}

// mediaIn finds the media in a body's messages or input.
func mediaIn(b *body) media {
	var m media
	for _, field := range []string{"messages", "input"} {
		raw, ok := b.value(field)
		if !ok || !mayCarryMedia(raw) {
			continue
		}
		var v any
		if json.Unmarshal(raw, &v) == nil {
			m.scan(v)
		}
	}
	return m
}

// mayCarryMedia spares decoding most bodies again: those with no key or value
// that names media. Nested values pass through as the client wrote them, so a
// key spelled with a \u escape counts too.
func mayCarryMedia(raw []byte) bool {
	for _, marker := range []string{`url"`, `"file_id"`, `"file_data"`, `"input_audio"`, `"base64"`, `\u`} {
		if bytes.Contains(raw, []byte(marker)) {
			return true
		}
	}
	return false
}

func (m *media) scan(v any) {
	switch v := v.(type) {
	case []any:
		for _, inner := range v {
			m.scan(inner)
		}
	case map[string]any:
		// Anthropic's source of an image or document: {"type": "url", "url": ...}
		// or {"type": "base64", "data": ...}. Only an image block's is
		// bounded by its pixels.
		if v["type"] == "url" && remote(v["url"]) {
			m.unbounded = true
		}
		if src, ok := v["source"].(map[string]any); ok && src["type"] == "base64" {
			if v["type"] == "image" {
				m.images++
			} else {
				m.unbounded = true
			}
		}
		for key, inner := range v {
			switch key {
			case "file_id", "file_url", "file_data":
				if s, _ := inner.(string); s != "" {
					m.unbounded = true
				}
			case "input_audio", "audio_url", "video_url":
				if inner != nil {
					m.unbounded = true
				}
			case "image_url":
				if obj, ok := inner.(map[string]any); ok {
					inner = obj["url"]
				}
				if s, _ := inner.(string); s != "" {
					if remote(s) {
						m.unbounded = true
					} else {
						m.images++
					}
				}
			// A tool's arguments are the model's words, not media.
			case "input", "arguments":
				if v["type"] == "tool_use" || v["type"] == "server_tool_use" {
					continue
				}
			}
			m.scan(inner)
		}
	}
}

// remote reports whether a URL is fetched, as opposed to inline data.
func remote(v any) bool {
	s, _ := v.(string)
	return s != "" && !strings.HasPrefix(s, "data:")
}

// filtersHold is the most the billed filters may cost to read in tokens. Each
// may rewrite all of them.
func (s *Server) filtersHold(org string, filters []string, in int) int64 {
	var most int64
	rewrite := policy.Tokens{Input: in, Output: in}
	for _, m := range s.billedFilterModels(org, filters) {
		if line, ok := m.Bill(rewrite, rewrite); ok {
			most += line.CreditMicros
		}
	}
	return most
}

// outputTokens is the most a request may write; an embedding or a rerank
// writes none.
func (c *call) outputTokens(b *body) int {
	if c.surf.kind == policy.KindEmbedding || c.surf.kind == policy.KindRerank {
		return 0
	}
	return outputCeiling(b)
}

// mostAnswers caps how many answers a request is held for, so the hold cannot
// overflow. It is OpenAI's own limit on `n`.
const mostAnswers = 128

// outputCeiling is the ceiling of each answer times the answers asked for.
// `n` and `best_of` ask for several, and the provider bills every one.
func outputCeiling(b *body) int {
	asked, answers := 0, 1
	if b != nil {
		for _, field := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
			if v, ok := b.ceiling(field); ok && v > 0 {
				asked = max(asked, int(min(v, mostOutputTokens)))
			}
		}
		for _, field := range []string{"n", "best_of"} {
			if v, ok := b.ceiling(field); ok && v > 1 {
				answers = max(answers, int(min(v, mostAnswers)))
			}
		}
	}
	if asked == 0 {
		asked = unstatedOutputTokens
	}
	return asked * answers
}

// checkCredit says why a check may not call these models, or "" if it may. A
// billed model needs credit, as for a request.
func (s *Server) checkCredit(org string, models ...policy.Model) string {
	if !slices.ContainsFunc(models, func(m policy.Model) bool { return m.Billing != nil }) {
		return ""
	}
	// Holding 0 only asks whether the organisation has credit.
	if _, err := s.budgets.HoldCredit(org, 0); err != nil {
		return err.Error()
	}
	return ""
}

// billCheck bills what a check spent on billed models. It writes no usage
// row, since a check is not a request.
func (s *Server) billCheck(org string, bills []policy.BillLine) {
	if len(bills) == 0 {
		return
	}
	s.record(store.Event{TS: time.Now(), OrgID: org, Bills: bills, BillsOnly: true})
}

// record charges the local view of the organisation's credit and hands the
// event to the usage writer, which charges it for good.
func (s *Server) record(ev store.Event) {
	var credit int64
	for _, b := range ev.Bills {
		credit += b.CreditMicros
	}
	s.budgets.ChargeCredit(ev.OrgID, credit)
	s.sink.Record(ev)
}
