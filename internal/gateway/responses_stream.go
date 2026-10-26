package gateway

import (
	"encoding/json"
	"io"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/id"
)

// The streaming half of the Responses surface. The response opens, output
// items open and close by index, and the response closes with a status and
// token counts. Clients read finished items from their closing events, so
// those carry the whole item.

// pipe forwards a chat completion stream as a Responses stream. The API has
// no keep-alive event, so it writes only when the upstream does.
func (sh responsesShape) pipe(dst io.Writer, flush func(), src io.Reader, alias string,
	_ bool, limit int64,
) (streamStats, error) {
	return pipeEvents(dst, flush, src, limit, 0, func(send func([]byte) error) eventStream {
		return &responsesStream{
			alias: alias, tools: sh.tools, respID: id.New("resp"),
			createdAt: time.Now().Unix(), open: -1, send: send,
		}
	})
}

// responsesStream is the state of translating a chat stream to a Responses
// stream.
type responsesStream struct {
	alias     string
	tools     toolNames
	respID    string
	createdAt int64
	send      func([]byte) error
	seq       int

	started bool
	// done is every finished output item, for the closing event.
	done []any
	// blocks is the output item under way.
	blocks
	itemID string
	// call is the call item under way, as the client declared its tool.
	call  map[string]any
	text  strings.Builder
	input inputStream

	chatChunks
}

func (s *responsesStream) chunk(payload []byte) error {
	return s.fold(payload, s.textDelta, s.toolDelta)
}

func (s *responsesStream) textDelta(text string) error {
	if err := s.start(); err != nil {
		return err
	}
	if err := s.enter(false, 0, s.closeItem, s.openMessage); err != nil {
		return err
	}
	s.text.WriteString(text)
	return s.emit("response.output_text.delta", map[string]any{
		"item_id": s.itemID, "output_index": s.open, "content_index": 0, "delta": text,
		"logprobs": []any{},
	})
}

func (s *responsesStream) toolDelta(index int, tc oaiToolCallDelta) error {
	if err := s.start(); err != nil {
		return err
	}
	if err := s.enter(true, index, s.closeItem, func() error {
		return s.openCall(index, tc.ID, tc.Function.Name)
	}); err != nil {
		return err
	}
	if tc.Function.Arguments == "" {
		return nil
	}
	s.text.WriteString(tc.Function.Arguments)
	if s.call["type"] == "custom_tool_call" {
		return s.inputDelta(s.open, s.input.read(tc.Function.Arguments))
	}
	return s.emit("response.function_call_arguments.delta", map[string]any{
		"item_id": s.itemID, "output_index": s.open, "delta": tc.Function.Arguments,
	})
}

// response is the response object carried by the opening and closing events.
func (s *responsesStream) response(status string, incomplete any) responsesOut {
	output := s.done
	if output == nil {
		output = []any{}
	}
	return responsesOut{
		ID: s.respID, Object: "response", CreatedAt: s.createdAt, Status: status,
		IncompleteDetails: incomplete, Model: s.alias, Output: output,
		Usage: newResponsesUsage(s.usage),
	}
}

func (s *responsesStream) start() error {
	if s.started {
		return nil
	}
	s.started = true
	if err := s.emit("response.created", map[string]any{
		"response": s.response("in_progress", nil),
	}); err != nil {
		return err
	}
	return s.emit("response.in_progress", map[string]any{
		"response": s.response("in_progress", nil),
	})
}

func (s *responsesStream) openMessage() error {
	s.open, s.isTool, s.itemID = len(s.done), false, id.New("msg")
	s.text.Reset()
	if err := s.emit("response.output_item.added", map[string]any{
		"output_index": s.open,
		"item": map[string]any{
			"type": "message", "id": s.itemID, "status": "in_progress",
			"role": "assistant", "content": []any{},
		},
	}); err != nil {
		return err
	}
	return s.emit("response.content_part.added", map[string]any{
		"item_id": s.itemID, "output_index": s.open, "content_index": 0, "part": outputText(""),
	})
}

func (s *responsesStream) openCall(upstreamIndex int, upstreamID, name string) error {
	s.open, s.isTool, s.tool = len(s.done), true, upstreamIndex
	s.call = s.tools.call("in_progress", callID(upstreamID), name, "")
	s.itemID = s.call["id"].(string)
	s.text.Reset()
	s.input = inputStream{}
	return s.emit("response.output_item.added", map[string]any{"output_index": s.open, "item": s.call})
}

// closeItem closes the open item, repeating it whole in the closing events.
func (s *responsesStream) closeItem() error {
	if s.open < 0 {
		return nil
	}
	index, text := s.open, s.text.String()
	s.open = -1
	if s.isTool {
		return s.closeCall(index, text)
	}
	item := messageItem(s.itemID, "completed", text)
	s.done = append(s.done, item)
	if err := s.emit("response.output_text.done", map[string]any{
		"item_id": s.itemID, "output_index": index, "content_index": 0, "text": text,
		"logprobs": []any{},
	}); err != nil {
		return err
	}
	if err := s.emit("response.content_part.done", map[string]any{
		"item_id": s.itemID, "output_index": index, "content_index": 0, "part": outputText(text),
	}); err != nil {
		return err
	}
	return s.emit("response.output_item.done", map[string]any{"output_index": index, "item": item})
}

// closeCall closes the call under way, with its whole arguments, or the text
// read out of them for a custom tool.
func (s *responsesStream) closeCall(index int, args string) error {
	item := maps.Clone(s.call)
	item["status"] = "completed"
	s.done = append(s.done, item)
	var err error
	if item["type"] == "custom_tool_call" {
		input := customInput(args)
		item["input"] = input
		// What was not streamed yet. Arguments not read as they came are sent
		// whole here; the done event is final.
		if rest, ok := strings.CutPrefix(input, s.input.sent.String()); ok {
			err = s.inputDelta(index, rest)
		}
		if err == nil {
			err = s.emit("response.custom_tool_call_input.done", map[string]any{
				"item_id": s.itemID, "output_index": index, "input": input,
			})
		}
	} else {
		item["arguments"] = args
		err = s.emit("response.function_call_arguments.done", map[string]any{
			"item_id": s.itemID, "output_index": index, "arguments": args,
		})
	}
	if err != nil {
		return err
	}
	return s.emit("response.output_item.done", map[string]any{"output_index": index, "item": item})
}

func (s *responsesStream) inputDelta(index int, text string) error {
	if text == "" {
		return nil
	}
	return s.emit("response.custom_tool_call_input.delta", map[string]any{
		"item_id": s.itemID, "output_index": index, "delta": text,
	})
}

// inputStream reads a custom tool's text out of {"input": "..."} as the
// arguments arrive. Otherwise a long patch sends nothing until whole, and a
// client may give up on the quiet stream. Other shapes are left to
// customInput once whole.
type inputStream struct {
	raw []byte // arrived, not read yet
	// inside is set once the opening quote is read, and done once the string
	// ended or the arguments have another shape.
	inside, done bool
	sent         strings.Builder
}

// read takes the next piece of the arguments and returns the text it
// completes. An escape cut in half waits for the rest.
func (in *inputStream) read(args string) string {
	if in.done {
		return ""
	}
	in.raw = append(in.raw, args...)
	if !in.inside {
		n, ok := inputStart(in.raw)
		if n == 0 {
			in.done = !ok
			return ""
		}
		in.raw, in.inside = in.raw[n:], true
	}
	n, end := stringPrefix(in.raw)
	var text string
	if json.Unmarshal(append(append([]byte{'"'}, in.raw[:n]...), '"'), &text) != nil {
		in.done = true
		return ""
	}
	in.raw, in.done = in.raw[n:], end
	in.sent.WriteString(text)
	return text
}

// inputStart is how many bytes of raw open the input string, `{"input":"` with
// any spacing. It is 0 with ok set while raw could still become that, and 0
// without it once it cannot.
func inputStart(raw []byte) (n int, ok bool) {
	for _, tok := range []string{"{", `"input"`, ":", `"`} {
		for n < len(raw) && strings.ContainsRune(" \t\r\n", rune(raw[n])) {
			n++
		}
		rest := string(raw[n:])
		if len(rest) < len(tok) {
			return 0, strings.HasPrefix(tok, rest)
		}
		if !strings.HasPrefix(rest, tok) {
			return 0, false
		}
		n += len(tok)
	}
	return n, true
}

// stringPrefix is how much of raw, the inside of a JSON string, can be decoded
// now: up to the closing quote, which sets end, or to an incomplete escape. A
// high surrogate waits for its pair.
func stringPrefix(raw []byte) (n int, end bool) {
	for n < len(raw) {
		switch raw[n] {
		case '"':
			return n, true
		case '\\':
			if n+1 >= len(raw) {
				return n, false
			}
			if raw[n+1] != 'u' {
				n += 2
				continue
			}
			if n+6 > len(raw) {
				return n, false
			}
			if c, _ := strconv.ParseUint(string(raw[n+2:n+6]), 16, 16); c >= 0xd800 && c < 0xdc00 {
				if n+12 > len(raw) {
					return n, false
				}
				if raw[n+6] == '\\' && raw[n+7] == 'u' {
					n += 6
				}
			}
			n += 6
		default:
			n++
		}
	}
	return n, false
}

// finish closes the response with the last chunk's usage, or fails it if the
// upstream failed, since a closed response reads as a finished turn. The item
// under way is not whole, so it is left out.
func (s *responsesStream) finish(cut error) error {
	if err := s.start(); err != nil {
		return err
	}
	if cut != nil {
		failed := s.response("failed", nil)
		// server_error: the request was fine, so a retry may work.
		failed.Error = map[string]string{"code": "server_error", "message": cutMessage(cut)}
		return s.emit("response.failed", map[string]any{"response": failed})
	}
	if err := s.closeItem(); err != nil {
		return err
	}
	status, incomplete := responseStatus(s.finishReason)
	name := "response.completed"
	if status == "incomplete" {
		name = "response.incomplete"
	}
	return s.emit(name, map[string]any{"response": s.response(status, incomplete)})
}

func (s *responsesStream) emit(name string, payload map[string]any) error {
	payload["type"] = name
	payload["sequence_number"] = s.seq
	s.seq++
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.send(frame(name, data))
}
