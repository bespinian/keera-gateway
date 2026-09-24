package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// The Messages API forwarded to Anthropic as it is. See native.go.

// anthropicVersion is the API version sent when the client names none. It is
// the only one Anthropic has published.
const anthropicVersion = "2023-06-01"

// anthropicDialect implements dialect for `/v1/messages`.
type anthropicDialect struct{}

func (anthropicDialect) provider() string { return "anthropic" }

func (anthropicDialect) path() string { return "/messages" }

// text is the system prompt, and the text and tool results of every turn.
// Thinking blocks are left alone: they are signed, and one that was changed is
// refused.
func (anthropicDialect) text(b *body) (textDoc, error) {
	d := newTreeDoc()
	system, err := d.root(b, "system")
	if err != nil {
		return nil, err
	}
	if s, ok := system.(string); ok {
		d.add(s, func(v string) { d.roots["system"] = v })
	}
	for _, block := range objects(system) {
		addTextBlock(d, block)
	}
	msgs, err := d.root(b, "messages")
	if err != nil {
		return nil, err
	}
	if _, ok := msgs.([]any); !ok {
		return nil, errors.New("the 'messages' field must be an array")
	}
	for _, msg := range objects(msgs) {
		d.addField(msg, "content")
		for _, block := range objects(msg["content"]) {
			addTextBlock(d, block)
			if block["type"] != "tool_result" {
				continue
			}
			d.addField(block, "content")
			for _, inner := range objects(block["content"]) {
				addTextBlock(d, inner)
			}
		}
	}
	return d, nil
}

// addTextBlock collects the text of a text block.
func addTextBlock(d *treeDoc, block map[string]any) {
	if block["type"] == "text" {
		d.addField(block, "text")
	}
}

// addSystem puts the guardrail's prompt first, as a block of its own, so the
// client's blocks keep their cache markers.
func (anthropicDialect) addSystem(b *body, prompt string) error {
	first := map[string]any{"type": "text", "text": prompt}
	blocks := []any{first}
	if raw, ok := b.value("system"); ok {
		var s string
		var rest []json.RawMessage
		switch {
		case json.Unmarshal(raw, &s) == nil:
			blocks = append(blocks, map[string]any{"type": "text", "text": s})
		case json.Unmarshal(raw, &rest) == nil:
			for _, r := range rest {
				blocks = append(blocks, r)
			}
		default:
			return errors.New("the 'system' field must be a string or an array of text blocks; " +
				"a guardrail on this key adds a system prompt to every request")
		}
	}
	encoded, err := json.Marshal(blocks)
	if err != nil {
		return err
	}
	b.set("system", encoded)
	return nil
}

// clamp holds max_tokens to the ceiling. A thinking budget has to stay below
// max_tokens, or Anthropic refuses the request, so it is lowered with it -
// and thinking is turned off when the ceiling leaves less than the smallest
// budget Anthropic accepts.
func (anthropicDialect) clamp(b *body, limit int) {
	clampFields(b, limit, "max_tokens")
	raw, ok := b.value("thinking")
	if !ok {
		return
	}
	var thinking map[string]json.RawMessage
	if json.Unmarshal(raw, &thinking) != nil {
		return
	}
	var budget int
	if json.Unmarshal(thinking["budget_tokens"], &budget) != nil || budget < limit {
		return
	}
	const minBudget = 1024
	if limit <= minBudget {
		b.remove("thinking")
		return
	}
	thinking["budget_tokens"] = json.RawMessage(strconv.Itoa(limit - 1))
	if encoded, err := json.Marshal(thinking); err == nil {
		b.set("thinking", encoded)
	}
}

// needsNative is always empty: every Messages request can be translated.
func (anthropicDialect) needsNative(*body) string { return "" }

// auth sends the key as x-api-key, which is what the Messages API reads, with
// the client's API version and beta flags.
func (anthropicDialect) auth(client http.Header) func(http.Header, string) {
	version := strings.TrimSpace(client.Get("Anthropic-Version"))
	if version == "" {
		version = anthropicVersion
	}
	beta := strings.Join(client.Values("Anthropic-Beta"), ",")
	return func(h http.Header, credential string) {
		if credential != "" {
			h.Set("X-Api-Key", credential)
		}
		h.Set("Anthropic-Version", version)
		if beta != "" {
			h.Set("Anthropic-Beta", beta)
		}
	}
}

// anthropicUsage is the usage record of the Messages API. Unlike OpenAI's,
// its input count leaves out what was read from or written to the cache.
type anthropicUsage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
}

// merge keeps the larger of each count. The stream reports them in pieces:
// input at the start, output at the end, and later counts are running totals.
func (u *anthropicUsage) merge(o anthropicUsage) {
	u.InputTokens = max(u.InputTokens, o.InputTokens)
	u.OutputTokens = max(u.OutputTokens, o.OutputTokens)
	u.CacheCreationTokens = max(u.CacheCreationTokens, o.CacheCreationTokens)
	u.CacheReadTokens = max(u.CacheReadTokens, o.CacheReadTokens)
}

// tokens is the record in the gateway's terms, where cached tokens are part of
// the input. A cache write is charged as input: the gateway has no rate for
// it, and Anthropic's is a quarter more.
func (u anthropicUsage) tokens() *tokenUsage {
	t := &tokenUsage{
		InputTokens:  u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens,
		OutputTokens: u.OutputTokens,
	}
	t.InputDetails.CachedTokens = u.CacheReadTokens
	t.TotalTokens = t.InputTokens + t.OutputTokens
	return t
}

func (anthropicDialect) usage(raw []byte) *tokenUsage {
	var doc struct {
		Usage *anthropicUsage `json:"usage"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.Usage == nil {
		return nil
	}
	return doc.Usage.tokens()
}

func (anthropicDialect) rename(raw []byte, alias string) []byte {
	return renameIn(raw, "", alias)
}

func (anthropicDialect) stream(alias string) nativeStream {
	return &anthropicStream{alias: alias}
}

// anthropicStream reads a Messages stream on its way through. Only the event
// that opens the message and the one that closes it are decoded; the rest are
// counted by a byte search.
type anthropicStream struct {
	alias string
	usage anthropicUsage
	// started and ended say whether the events carrying the counts arrived:
	// the input comes when the message opens, the output when it closes.
	started, ended bool
	deltas         int
}

var (
	contentDeltaType = []byte(`"content_block_delta"`)
	messageStartType = []byte(`"message_start"`)
	messageDeltaType = []byte(`"message_delta"`)
)

func (s *anthropicStream) event(payload []byte) []byte {
	switch {
	case bytes.Contains(payload, contentDeltaType):
		s.deltas++
	case bytes.Contains(payload, messageStartType):
		var ev struct {
			Message struct {
				Usage *anthropicUsage `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(payload, &ev) == nil && ev.Message.Usage != nil {
			s.usage.merge(*ev.Message.Usage)
			s.started = true
		}
		return renameIn(payload, "message", s.alias)
	case bytes.Contains(payload, messageDeltaType):
		var ev struct {
			Usage *anthropicUsage `json:"usage"`
		}
		if json.Unmarshal(payload, &ev) == nil && ev.Usage != nil {
			s.usage.merge(*ev.Usage)
			s.ended = true
		}
	}
	return nil
}

// counts keeps the input count of a stream cut short, which is exact, and
// counts its output from the deltas.
func (s *anthropicStream) counts() (*tokenUsage, int, bool) {
	switch {
	case s.ended:
		return s.usage.tokens(), s.deltas, false
	case s.started:
		u := s.usage.tokens()
		u.OutputTokens = max(u.OutputTokens, s.deltas)
		return u, s.deltas, true
	default:
		return nil, s.deltas, false
	}
}

// ------------------------------------------------------------ count_tokens

// countTokens answers `POST /v1/messages/count_tokens`, which clients use to
// see how full the context is.
//
// It is answered here, from an estimate, and never forwarded: forwarding
// would send the prompt to Anthropic without its filters. The estimate errs
// high, so a client that compacts on it compacts early rather than late.
func (s *Server) countTokens(w http.ResponseWriter, r *http.Request) {
	sh := anthropicShape{}
	res, ok := s.authenticate(w, r, sh)
	if !ok {
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.opts.MaxBodyBytes))
	if err != nil {
		sh.writeError(w, http.StatusRequestEntityTooLarge, "", "",
			"the request body exceeds the gateway's limit")
		return
	}
	b, err := parseBody(raw)
	if err != nil {
		sh.writeError(w, http.StatusBadRequest, "", "", err.Error())
		return
	}
	alias, _ := b.str("model")
	if !s.mayCall(res, alias) {
		sh.writeError(w, http.StatusNotFound, "", "",
			s.advise("the model '"+alias+"' does not exist or this key may not use it"))
		return
	}
	doc, err := anthropicDialect{}.text(b)
	if err != nil {
		sh.writeError(w, http.StatusBadRequest, "", "", err.Error())
		return
	}
	// Tool declarations are part of the prompt too, and on a coding agent a
	// large one.
	tools, _ := b.value("tools")
	tokens := estimateTokens(doc.texts()) + len(tools)/bytesPerToken
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int{"input_tokens": tokens})
}

// mayCall reports whether a key may name alias as its model: a chat model it
// is allowed, or one of its organisation's routers.
func (s *Server) mayCall(res *policy.Resolved, alias string) bool {
	if alias == "" || !res.AllowsModel(alias) {
		return false
	}
	if m, found := s.src.Model(alias); found {
		return m.Enabled && m.Kind == policy.KindChat
	}
	_, routed := s.src.Router(res.Key.OrgID, alias)
	return routed
}
