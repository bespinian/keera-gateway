package gateway

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTextReachesTheBackendAsTheClientWroteIt(t *testing.T) {
	// The prompt is copied, not decoded and escaped again, so what the client
	// escaped stays escaped the same way.
	const text = `"a\"b\\c <d> & é 🙂 \n\t "`
	in := `{"model":"m","system":[{"type":"text","text":` + text + `}],"messages":[` +
		`{"role":"user","content":` + text + `},` +
		`{"role":"assistant","content":[{"type":"text","text":` + text + `}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":` + text + `}]}]}`
	raw, err := anthropicShape{}.decode([]byte(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if n := bytes.Count(raw, []byte(text)); n != 4 {
		t.Errorf("the text appears as sent %d times, want 4:\n%s", n, raw)
	}

	var want string
	if err := json.Unmarshal([]byte(text), &want); err != nil {
		t.Fatal(err)
	}
	for _, m := range messagesOf(t, decodeMessages(t, in)) {
		if m["content"] != want {
			t.Errorf("%s content = %q, want %q", m["role"], m["content"], want)
		}
	}
}

func TestTextTheBackendMightRefuseIsCleanedUp(t *testing.T) {
	// Bytes that are not UTF-8, and a lone half of a surrogate pair, are valid
	// enough for the gateway but not for a tokenizer. They become U+FFFD, as
	// they did when every text was decoded.
	in := "{\"model\":\"m\",\"messages\":[{\"role\":\"user\",\"content\":\"a\xffb\"}," +
		`{"role":"assistant","content":"c\ud800d"},{"role":"user","content":"e🙂f"}]}`
	raw, err := anthropicShape{}.decode([]byte(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !utf8.Valid(raw) {
		t.Errorf("the translated request is not UTF-8: %q", raw)
	}
	var got []string
	for _, m := range messagesOf(t, decodeMessages(t, in)) {
		got = append(got, m["content"].(string))
	}
	want := []string{"a�b", "c�d", "e🙂f"}
	if !equalStrings(got, want) {
		t.Errorf("contents = %q, want %q", got, want)
	}
}

func TestTextBlocksAreJoinedWithoutLosingTheirEscapes(t *testing.T) {
	// Joining copies the inside of each literal, so an escape at either edge
	// of a block must survive it.
	in := `{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"t","content":[{"type":"text","text":"a\""},` +
		`{"type":"text","text":""},{"type":"text","text":"\\b"}]}]}]}`
	msgs := messagesOf(t, decodeMessages(t, in))
	if len(msgs) != 1 || msgs[0]["content"] != "a\"\n\n\\b" {
		t.Errorf("messages = %v, want one tool message reading %q", msgs, "a\"\n\n\\b")
	}
}

func TestContentThatIsNotTextIsStillRefused(t *testing.T) {
	// Text is no longer decoded, so its type is checked on its own.
	for _, content := range []string{`5`, `[{"type":"text","text":5}]`, `{"text":"x"}`} {
		in := `{"model":"m","messages":[{"role":"user","content":` + content + `}]}`
		_, err := anthropicShape{}.decode([]byte(in))
		if err == nil || !strings.Contains(err.Error(), "must be a string or an array") {
			t.Errorf("content %s: error = %v, want a refusal", content, err)
		}
	}
}
