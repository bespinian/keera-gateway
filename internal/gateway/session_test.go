package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// keyFor derives a session key the way serve does, from a body written out as
// a client would send it. headers are name/value pairs to set on the request.
func keyFor(t *testing.T, keyID, body string, kind policy.Kind, headers ...string) string {
	t.Helper()
	b, err := parseBody([]byte(body))
	if err != nil {
		t.Fatalf("parseBody: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	return sessionKey(r, keyID, b, kind)
}

// stated is keyFor with the gateway's own header set, which is what most of
// these tests mean by "the client named it".
func stated(t *testing.T, keyID, body, id string) string {
	t.Helper()
	return keyFor(t, keyID, body, policy.KindChat, SessionHeader, id)
}

// The property the whole report rests on: every call an agent makes working
// through one task hashes to the same key, and calls belonging to anything else
// do not.
func TestOneTasksCallsShareASessionKey(t *testing.T) {
	// Turn one of a task, and the same task once the agent has read a file,
	// called a tool and been given the result. What is identical between them
	// is the opening prompt and nothing else.
	first := `{"model":"keera-code","messages":[
		{"role":"system","content":"You are a coding agent. The date is 2026-09-09."},
		{"role":"user","content":"rename Widget to Gadget across the repo"}]}`
	later := `{"model":"keera-code","messages":[
		{"role":"system","content":"You are a coding agent. The date is 2026-09-09."},
		{"role":"user","content":"rename Widget to Gadget across the repo"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function",
			"function":{"name":"read","arguments":"{\"path\":\"widget.go\"}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"package widget"},
		{"role":"user","content":"looks right, keep going"}]}`

	opening := keyFor(t, "key_1", first, policy.KindChat)
	continued := keyFor(t, "key_1", later, policy.KindChat)
	if opening == "" {
		t.Fatal("a chat request with a user message got no session key")
	}
	if opening != continued {
		t.Errorf("the fortieth call of a task hashes to %q and the first to %q; a "+
			"session that changes key mid-task is not a session", continued, opening)
	}

	// A different task, and the same task from somebody else's key, are both
	// different sessions. The second matters: a session is read next to whose
	// team and whose person it belonged to, so one key's session must never be
	// joinable by another.
	other := strings.Replace(first, "rename Widget to Gadget", "add a health endpoint", 1)
	if keyFor(t, "key_1", other, policy.KindChat) == opening {
		t.Error("two different opening prompts hash to one session")
	}
	if keyFor(t, "key_2", first, policy.KindChat) == opening {
		t.Error("the same prompt from two keys hashes to one session")
	}

	// A changed system prompt does not start a new session. This is why the
	// opening user message is hashed rather than the whole prefix: agents put
	// the date, the working directory and the git branch in there, and a key
	// derived from them would cut a task in half whenever any of it moved.
	moved := strings.Replace(first, "2026-09-09", "2026-09-10", 1)
	if keyFor(t, "key_1", moved, policy.KindChat) != opening {
		t.Error("a system prompt that mentions today's date starts a new session " +
			"every midnight")
	}

	if store.StatedSession(opening) {
		t.Errorf("%q reads as a key the client stated, but it was derived", opening)
	}
}

// A client that knows which conversation it is on is not guessed about.
func TestAClientCanNameItsOwnSession(t *testing.T) {
	body := `{"model":"keera-code","messages":[{"role":"user","content":"go"}]}`
	named := stated(t, "key_1", body, "task-42")
	if !store.StatedSession(named) {
		t.Errorf("%q does not read as a key the client stated", named)
	}
	// The header wins over the conversation, so two tasks that open with the
	// same word stay apart.
	if same := stated(t, "key_1", body, "task-43"); same == named {
		t.Error("two stated sessions with the same opening prompt hash to one")
	}
	// And the same stated id from another key is still another session.
	if other := stated(t, "key_2", body, "task-42"); other == named {
		t.Error("one key can name another key's session")
	}
	// What is stored is a hash of the id, not the id: a client is free to put
	// its own conversation identifier in the header without that identifier
	// being kept.
	if strings.Contains(named, "task-42") {
		t.Errorf("the session key %q carries the client's id verbatim", named)
	}
}

// There is no standard header for this, so the ones that exist are read as one.
// Which of them a client sends is a fact about how that client was written, and
// a task cut in half by a spelling would be worse than no header at all.
func TestEveryConventionForNamingASessionIsReadAsOne(t *testing.T) {
	body := `{"model":"keera-code","messages":[{"role":"user","content":"go"}]}`
	want := stated(t, "key_1", body, "task-42")

	for _, name := range []string{"X-Keera-Session", "X-Session-Id", "Helicone-Session-Id"} {
		got := keyFor(t, "key_1", body, policy.KindChat, name, "task-42")
		if got != want {
			t.Errorf("%s gives session %q, and X-Keera-Session gives %q; the same "+
				"conversation must not depend on which header a client happens to "+
				"spell it in", name, got, want)
		}
		if !store.StatedSession(got) {
			t.Errorf("%s did not read as the client naming its own session", name)
		}
	}

	// A request carrying two of them is a client and a proxy both having an
	// opinion. This gateway's own header is the one to believe.
	both := keyFor(t, "key_1", body, policy.KindChat,
		"X-Keera-Session", "task-42", "X-Session-Id", "something-else")
	if both != want {
		t.Errorf("with both headers set the session is %q, want the one this "+
			"gateway's own header names", both)
	}

	// A header that is present but empty is not a client naming anything, and
	// must fall through to the conversation rather than putting every request
	// that carries it into one session.
	empty := keyFor(t, "key_1", body, policy.KindChat, "X-Session-Id", "   ")
	if store.StatedSession(empty) {
		t.Errorf("an empty session header was read as a stated session (%q)", empty)
	}
	if empty != keyFor(t, "key_1", body, policy.KindChat) {
		t.Error("an empty session header changed the derived session")
	}
}

// Only a conversation has a session. The other two surfaces have nothing stable
// to group by, and labelling them anyway would fill a report of tasks with
// tasks one request long.
func TestOnlyTheChatSurfacesGetASession(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind policy.Kind
		body string
	}{
		{"a completion", policy.KindCompletion,
			`{"model":"keera-code","prompt":"func main() {"}`},
		{"an embedding", policy.KindEmbedding,
			`{"model":"keera-embed","input":"package main"}`},
		{"a chat with no user turn yet", policy.KindChat,
			`{"model":"keera-code","messages":[{"role":"system","content":"be helpful"}]}`},
		{"a chat with no messages at all", policy.KindChat,
			`{"model":"keera-code","messages":[]}`},
		{"a chat whose messages are not an array", policy.KindChat,
			`{"model":"keera-code","messages":"hello"}`},
	} {
		if got := keyFor(t, "key_1", tc.body, tc.kind); got != "" {
			t.Errorf("%s got the session key %q, want none", tc.name, got)
		}
	}
}

// The walk has to survive every shape a client actually sends, because it runs
// on every chat request and a body the inference plane would have accepted must
// not fail here.
func TestTheOpeningPromptIsFoundInWhateverShapeItArrives(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"a bare string", `{"messages":[{"role":"user","content":"hello"}]}`, `"hello"`},
		{"content blocks",
			`{"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`,
			`[{"type":"text","text":"hello"}]`},
		{"after a system message",
			`{"messages":[{"role":"system","content":"be helpful"},{"role":"user","content":"hi"}]}`,
			`"hi"`},
		{"with the fields in the other order",
			`{"messages":[{"content":"hi","role":"user"}]}`, `"hi"`},
		{"past an element that is not an object",
			`{"messages":[null,7,"x",{"role":"user","content":"hi"}]}`, `"hi"`},
		{"past a message with no role",
			`{"messages":[{"content":"orphan"},{"role":"user","content":"hi"}]}`, `"hi"`},
		{"with a brace inside a string",
			`{"messages":[{"role":"user","content":"if (x) { y }"}]}`, `"if (x) { y }"`},
		{"with an escaped quote inside a string",
			`{"messages":[{"role":"user","content":"say \"role\": \"user\""}]}`,
			`"say \"role\": \"user\""`},
		{"only an assistant turn", `{"messages":[{"role":"assistant","content":"hi"}]}`, ""},
	} {
		b, err := parseBody([]byte(tc.body))
		if err != nil {
			t.Fatalf("%s: parseBody: %v", tc.name, err)
		}
		content, ok := b.firstUserMessage()
		switch {
		case tc.want == "" && ok:
			t.Errorf("%s: found %q, want nothing", tc.name, content)
		case tc.want != "" && !ok:
			t.Errorf("%s: found nothing, want %q", tc.name, tc.want)
		case tc.want != "" && string(content) != tc.want:
			t.Errorf("%s: found %q, want %q", tc.name, content, tc.want)
		}
	}
}

// An enormous opening prompt is hashed to a bound rather than in full, so the
// cost of labelling a request does not follow a field the client controls.
func TestAnEnormousOpeningPromptIsHashedToABound(t *testing.T) {
	body := `{"model":"keera-code","messages":[{"role":"user","content":"` +
		strings.Repeat("a", maxSessionInput+1000) + `"}]}`
	if got := keyFor(t, "key_1", body, policy.KindChat); got == "" {
		t.Fatal("a very long opening prompt got no session key")
	}
}

// The session goes on the usage row - including the row of a request that never
// reached a model, because a task that stopped when a guardrail refused it is
// exactly the task somebody comes looking for.
func TestTheUsageRowCarriesTheSessionEvenWhenTheRequestIsRefused(t *testing.T) {
	h := newHarness(t, jsonBackend(`{"id":"1","choices":[{"message":{"content":"hi"}}],`+
		`"usage":{"prompt_tokens":10,"completion_tokens":2}}`), nil, nil)

	const opening = `{"role":"user","content":"rename Widget to Gadget"}`
	served := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[`+opening+`]}`)
	if served.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", served.StatusCode)
	}
	first := h.sink.last(t)
	if first.SessionKey == "" {
		t.Fatal("the usage row of a served chat request carries no session")
	}

	// The same task, now asking for a model this key may not use. It is
	// refused before it is forwarded, and it has to stay part of the task.
	refused := h.post(t, "/v1/chat/completions",
		`{"model":"nothing-serves-this","messages":[`+opening+`,`+
			`{"role":"assistant","content":"done"},{"role":"user","content":"again"}]}`)
	if refused.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", refused.StatusCode)
	}
	second := h.sink.last(t)
	if second.SessionKey != first.SessionKey {
		t.Errorf("the refused call is in session %q and the served one in %q; a task "+
			"that ends on a refusal must not lose the refusal",
			second.SessionKey, first.SessionKey)
	}
}

// The Anthropic-shaped surface and the OpenAI-shaped one reach the same models
// through the same guardrails, so one conversation carried on either of them is
// one task rather than two.
func TestTheTwoChatSurfacesAgreeOnTheSession(t *testing.T) {
	h := newHarness(t, jsonBackend(`{"id":"1","choices":[{"message":{"content":"hi"}}],`+
		`"usage":{"prompt_tokens":10,"completion_tokens":2}}`), nil, nil)

	if resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"port this to Go"}]}`,
	); resp.StatusCode != http.StatusOK {
		t.Fatalf("the OpenAI-shaped call answered %d", resp.StatusCode)
	}
	openAI := h.sink.last(t).SessionKey

	if resp := h.post(t, "/v1/messages",
		`{"model":"keera-code","max_tokens":64,"system":"be helpful",`+
			`"messages":[{"role":"user","content":"port this to Go"}]}`,
	); resp.StatusCode != http.StatusOK {
		t.Fatalf("the Anthropic-shaped call answered %d", resp.StatusCode)
	}
	anthropic := h.sink.last(t).SessionKey

	if openAI == "" || anthropic != openAI {
		t.Errorf("the same conversation is session %q on one surface and %q on the "+
			"other", openAI, anthropic)
	}
}
