package gateway

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTextReachesTheBackendAsTheClientWroteIt(t *testing.T) {
	// The prompt is copied, not re-escaped, so the client's escapes stay.
	const text = `"a\"b\\c <d> & é 🙂 \n\t "`
	in := `{"model":"m","system":[{"type":"text","text":` + text + `}],"messages":[` +
		`{"role":"user","content":` + text + `},` +
		`{"role":"assistant","content":[{"type":"text","text":` + text + `}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":` + text + `}]}]}`
	raw, err := decodeRaw(anthropicShape{}, in)
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
	for _, m := range messagesOf(t, decodeAs(t, anthropicShape{}, in)) {
		if m["content"] != want {
			t.Errorf("%s content = %q, want %q", m["role"], m["content"], want)
		}
	}
}

func TestTextTheBackendMightRefuseIsCleanedUp(t *testing.T) {
	// Invalid UTF-8 and a lone surrogate would break a tokenizer, so they
	// become U+FFFD, as decoding would make them.
	in := "{\"model\":\"m\",\"messages\":[{\"role\":\"user\",\"content\":\"a\xffb\"}," +
		`{"role":"assistant","content":"c\ud800d"},{"role":"user","content":"e🙂f"}]}`
	raw, err := decodeRaw(anthropicShape{}, in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !utf8.Valid(raw) {
		t.Errorf("the translated request is not UTF-8: %q", raw)
	}
	var got []string
	for _, m := range messagesOf(t, decodeAs(t, anthropicShape{}, in)) {
		got = append(got, m["content"].(string))
	}
	want := []string{"a�b", "c�d", "e🙂f"}
	if !slices.Equal(got, want) {
		t.Errorf("contents = %q, want %q", got, want)
	}
}

func TestTextBlocksAreJoinedWithoutLosingTheirEscapes(t *testing.T) {
	// An escape at either edge of a block must survive joining.
	in := `{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"t","content":[{"type":"text","text":"a\""},` +
		`{"type":"text","text":""},{"type":"text","text":"\\b"}]}]}]}`
	msgs := messagesOf(t, decodeAs(t, anthropicShape{}, in))
	if len(msgs) != 1 || msgs[0]["content"] != "a\"\n\n\\b" {
		t.Errorf("messages = %v, want one tool message reading %q", msgs, "a\"\n\n\\b")
	}
}

func TestContentThatIsNotTextIsStillRefused(t *testing.T) {
	// Text is not decoded, so its type is checked on its own.
	for _, content := range []string{`5`, `[{"type":"text","text":5}]`, `{"text":"x"}`} {
		in := `{"model":"m","messages":[{"role":"user","content":` + content + `}]}`
		_, _, err := anthropicShape{}.decodeBody([]byte(in))
		if err == nil || !strings.Contains(err.Error(), "must be a string or an array") {
			t.Errorf("content %s: error = %v, want a refusal", content, err)
		}
	}
}
