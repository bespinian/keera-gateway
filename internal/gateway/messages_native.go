package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
)

// The Messages API forwarded to Anthropic as it is. See native.go.

// anthropicVersion is sent when the client names none. It is the only one
// Anthropic has published.
const anthropicVersion = "2023-06-01"

type anthropicDialect struct{}

func (anthropicDialect) provider() string { return "anthropic" }

func (anthropicDialect) path() string { return "/messages" }

func (anthropicDialect) opening(b *body) (json.RawMessage, bool) {
	raw, _ := b.value("messages")
	return openingIn(raw, func(elem []byte) ([]chatMessage, error) {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(elem, &m); err != nil {
			return nil, err
		}
		return convertMessage(m.Role, m.Content)
	})
}

// text is the system prompt, and the text and tool results of every turn.
// Thinking blocks are left alone, since they are signed.
func (anthropicDialect) text(b *body) (textDoc, error) {
	d := newTreeDoc()
	system, err := d.root(b, "system")
	if err != nil {
		return nil, err
	}
	if s, ok := system.(string); ok {
		d.add(s, func(v string) { d.roots["system"] = v })
	}
	for _, block := range d.objects(system) {
		addTextBlock(d, block)
	}
	msgs, err := d.root(b, "messages")
	if err != nil {
		return nil, err
	}
	if _, ok := msgs.([]any); !ok {
		return nil, errors.New("the 'messages' field must be an array")
	}
	for _, msg := range d.objects(msgs) {
		d.addField(msg, "content")
		for _, block := range d.objects(msg["content"]) {
			addTextBlock(d, block)
			if block["type"] != "tool_result" {
				continue
			}
			d.addField(block, "content")
			for _, inner := range d.objects(block["content"]) {
				addTextBlock(d, inner)
			}
		}
	}
	if d.err != nil {
		return nil, d.err
	}
	return d, nil
}

// addTextBlock collects the text of a text block, a search result or a
// document. See blockText.
func addTextBlock(d *treeDoc, block map[string]any) {
	switch block["type"] {
	case "text":
		d.addField(block, "text")
	case "search_result":
		d.addField(block, "title")
		for _, inner := range d.objects(block["content"]) {
			addTextBlock(d, inner)
		}
	case "document":
		d.addField(block, "title")
		d.addField(block, "context")
		for _, src := range d.objects([]any{block["source"]}) {
			switch src["type"] {
			case "text":
				d.addField(src, "data")
			case "content":
				d.addField(src, "content")
				for _, inner := range d.objects(src["content"]) {
					addTextBlock(d, inner)
				}
			}
		}
	}
}

// addSystem puts the system prompt first as its own block, so the client's
// blocks keep their cache markers.
func (anthropicDialect) addSystem(b *body, prompt string) error {
	return setSystem(b, prompt, true)
}

// setSystem adds the system prompt as its own text block, before or after the
// client's.
func setSystem(b *body, prompt string, first bool) error {
	var blocks []any
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
			return errors.New("the 'system' field must be a string or an array of text " +
				"blocks; " + promptAdded)
		}
	}
	block := map[string]any{"type": "text", "text": prompt}
	if first {
		blocks = append([]any{block}, blocks...)
	} else {
		blocks = append(blocks, block)
	}
	encoded, err := json.Marshal(blocks)
	if err != nil {
		return err
	}
	b.set("system", encoded)
	return nil
}

func (anthropicDialect) needsNative(*body) string { return "" }

func (anthropicDialect) onKey(*body) string { return "" }

// auth sends the key as x-api-key, with the client's API version and beta
// flags.
func (anthropicDialect) auth(client http.Header) func(http.Header, string) {
	version := strings.TrimSpace(client.Get("Anthropic-Version"))
	if version == "" {
		version = anthropicVersion
	}
	beta := strings.Join(client.Values("Anthropic-Beta"), ",")
	return func(h http.Header, credential string) {
		setCredential(h, "X-Api-Key", credential)
		h.Set("Anthropic-Version", version)
		if beta != "" {
			h.Set("Anthropic-Beta", beta)
		}
	}
}

// anthropicUsage is the Messages API's usage record. Unlike OpenAI's, its
// input count leaves out cache reads and writes.
type anthropicUsage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
	// CacheCreation splits the cache writes by how long they are kept.
	CacheCreation struct {
		HourTokens int `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

// merge keeps the larger of each count, since the stream reports them in
// pieces and as running totals.
func (u *anthropicUsage) merge(o anthropicUsage) {
	u.InputTokens = max(u.InputTokens, o.InputTokens)
	u.OutputTokens = max(u.OutputTokens, o.OutputTokens)
	u.CacheCreationTokens = max(u.CacheCreationTokens, o.CacheCreationTokens)
	u.CacheReadTokens = max(u.CacheReadTokens, o.CacheReadTokens)
	u.CacheCreation.HourTokens = max(u.CacheCreation.HourTokens, o.CacheCreation.HourTokens)
}

// tokens converts to the gateway's terms, where the cache counts are part of
// the input.
func (u anthropicUsage) tokens() *tokenUsage {
	t := &tokenUsage{
		InputTokens:  u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens,
		OutputTokens: u.OutputTokens,
	}
	t.InputDetails.CachedTokens = u.CacheReadTokens
	t.InputDetails.CacheWriteTokens = u.CacheCreationTokens
	t.InputDetails.CacheWriteHourTokens = u.CacheCreation.HourTokens
	return t
}

func (anthropicDialect) usage(raw []byte) *tokenUsage { return usageOf[anthropicUsage](raw) }

// usageOf reads the top-level usage record of a whole answer, in shape U.
func usageOf[U any, P interface {
	*U
	tokens() *tokenUsage
}](raw []byte) *tokenUsage {
	var doc struct {
		Usage P `json:"usage"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.Usage == nil {
		return nil
	}
	return doc.Usage.tokens()
}

func (anthropicDialect) stream(alias string) nativeStream {
	return &anthropicStream{alias: alias}
}

// anthropicStream reads a Messages stream on its way through. Only the
// opening and closing events are decoded; the rest are counted.
type anthropicStream struct {
	alias string
	usage anthropicUsage
	// started and ended say whether the input and output counts arrived.
	started, ended bool
	deltas         int
	// stopped says message_stop arrived.
	stopped     bool
	upstreamErr string
}

func (s *anthropicStream) event(payload []byte) []byte {
	switch typeOf(payload) {
	case "content_block_delta":
		s.deltas++
	case "message_stop":
		s.stopped = true
	case "error":
		s.upstreamErr = upstreamText(payload)
	case "message_start":
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
	case "message_delta":
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

func (s *anthropicStream) failure() error {
	return endedFailure(s.upstreamErr, s.stopped)
}

// counts keeps the exact input count of a stream cut short, and estimates its
// output from the deltas.
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

// countTokens answers `POST /v1/messages/count_tokens` with an estimate. It is
// never forwarded, which would send the prompt to Anthropic unfiltered. The
// estimate errs high, so a client compacts early rather than late.
func (s *Server) countTokens(w http.ResponseWriter, r *http.Request) {
	sh := anthropicShape{}
	res, ok := s.authenticate(w, r, sh)
	if !ok {
		return
	}
	raw, ok := readBody(w, r, s.opts.MaxBodyBytes, func(status int, _, msg string) {
		sh.writeError(w, status, "", "", msg)
	})
	if !ok {
		return
	}
	b, err := parseBody(raw)
	if err != nil {
		sh.writeError(w, http.StatusBadRequest, "", "", err.Error())
		return
	}
	// Refused as a request to the model would be.
	alias, _ := b.str("model")
	ref := aliasRefusal(alias)
	var n named
	if ref == nil {
		n, ref = s.lookupModel(res, alias, policy.KindChat)
	}
	if ref != nil {
		sh.writeError(w, ref.status, ref.typ, ref.code, s.told(*ref))
		return
	}
	doc, err := anthropicDialect{}.text(b)
	if err != nil {
		sh.writeError(w, http.StatusBadRequest, "", "", err.Error())
		return
	}
	// Tool declarations and added system prompts are part of the prompt too.
	tools, _ := b.value("tools")
	prompts := len(res.SystemPrompt) + mostPromptBytes(n.router.Start)
	tokens := estimateTokens(doc.texts()) + (len(tools)+prompts)/bytesPerToken
	httpx.WriteJSON(w, http.StatusOK, map[string]int{"input_tokens": tokens})
}

// mostPromptBytes is the most system prompt text any path through a router
// adds, since the path is only decided when the request is sent.
func mostPromptBytes(s *policy.Step) int {
	if s == nil {
		return 0
	}
	most := max(mostPromptBytes(s.Next), mostPromptBytes(s.Exceeded))
	for _, next := range s.Branches {
		most = max(most, mostPromptBytes(next))
	}
	if s.Kind == policy.StepSystemPrompt {
		most += len(s.Prompt)
	}
	return most
}
