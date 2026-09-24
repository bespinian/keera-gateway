package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// filterHarness wires a gateway whose key applies one filter, in front of a
// stand-in inference plane that answers as both models. The two are told apart
// by the backend model name, which is what the gateway actually sends.
func filterHarness(t *testing.T, filterReply func(segments []string) string,
	resolved *policy.Resolved,
) *harness {
	t.Helper()
	return filterHarnessWith(t,
		func(_ string, segments []string) string { return filterReply(segments) }, resolved)
}

// filterHarnessWith is the same, for the tests where the stand-in has to tell
// one filter from another. The instruction is the only thing that distinguishes
// them on the wire, two filters being able to run on the same alias.
func filterHarnessWith(t *testing.T, filterReply func(instruction string, segments []string) string,
	resolved *policy.Resolved,
) *harness {
	t.Helper()

	backend := func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var sent struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &sent)
		w.Header().Set("Content-Type", "application/json")

		if sent.Model != "guard-served" {
			_, _ = io.WriteString(w, `{"id":"1","choices":[{"message":{"content":"hi"}}],`+
				`"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
			return
		}
		var segments []string
		if len(sent.Messages) > 1 {
			_ = json.Unmarshal([]byte(sent.Messages[1].Content), &segments)
		}
		var instruction string
		if len(sent.Messages) > 0 {
			instruction = sent.Messages[0].Content
		}
		reply, _ := json.Marshal(filterReply(instruction, segments))
		_, _ = io.WriteString(w, `{"id":"2","choices":[{"message":{"content":`+string(reply)+
			`}}],"usage":{"prompt_tokens":100,"completion_tokens":200}}`)
	}

	if resolved == nil {
		resolved = policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"}, nil, nil, nil)
		resolved.Filters = []string{"redact"}
	}
	models := map[string]policy.Model{
		"keera-code": {
			Alias: "keera-code", Kind: policy.KindChat, BackendModel: "served-name",
			InputMicrosPerMTok: 1_000_000, OutputMicrosPerMTok: 4_000_000, Enabled: true,
		},
		"keera-guard": {
			Alias: "keera-guard", Kind: policy.KindChat, BackendModel: "guard-served",
			InputMicrosPerMTok: 2_000_000, OutputMicrosPerMTok: 2_000_000, Enabled: true,
		},
		"keera-embed": {
			Alias: "keera-embed", Kind: policy.KindEmbedding, BackendModel: "embed-served",
			Enabled: true,
		},
	}
	h := newHarness(t, backend, models, resolved)
	h.src.filters = map[string]policy.Filter{
		"org_1/redact": {
			OrgID: "org_1", Alias: "redact", Model: "keera-guard",
			Prompt: "Replace every credential with [CREDENTIAL].",
		},
	}
	return h
}

// redactor is a filter model that behaves: it rewrites what it was told to and
// hands everything else back untouched.
func redactor(segments []string) string {
	out := make([]string, len(segments))
	for i, s := range segments {
		out[i] = strings.ReplaceAll(s, "hunter2", "[CREDENTIAL]")
	}
	encoded, _ := json.Marshal(out)
	return string(encoded)
}

func TestFilterRewritesTheConversationBeforeItIsForwarded(t *testing.T) {
	h := filterHarness(t, redactor, nil)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"deploy with hunter2"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// The filter's own call reaches the backend first, so the second body is
	// the one the client's model was actually asked.
	<-h.upstreamBodies
	forwarded := string(<-h.upstreamBodies)
	if strings.Contains(forwarded, "hunter2") {
		t.Errorf("the secret reached the model the client asked for: %s", forwarded)
	}
	if !strings.Contains(forwarded, "[CREDENTIAL]") {
		t.Errorf("the filtered text was not forwarded: %s", forwarded)
	}
	if got := resp.Header.Get("X-Keera-Filters"); got != "redact" {
		t.Errorf("X-Keera-Filters = %q, want the filter that ran named", got)
	}
}

func TestFilterSpendIsChargedToTheSameBudget(t *testing.T) {
	h := filterHarness(t, redactor, nil)

	h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"hunter2"}]}`)

	// The filter model is priced at 2.00 per million tokens either way, so its
	// 100 input and 200 output tokens come to 600 micro-units. The model
	// the client asked for adds its own 30 on top.
	const filterMicros = 600
	ev := h.sink.last(t)
	if ev.CostMicros <= filterMicros {
		t.Errorf("cost = %d, want more than the %d the filter alone spent",
			ev.CostMicros, filterMicros)
	}
	// The tokens stay the client's model's own, so a per-alias report is not
	// quietly mixing two models' generation into one row.
	if ev.InputTokens != 10 || ev.OutputTokens != 5 {
		t.Errorf("tokens = %d/%d, want the answering model's own",
			ev.InputTokens, ev.OutputTokens)
	}
	if h.budgets.charged < filterMicros {
		t.Errorf("charged = %d, want the filter's spend included", h.budgets.charged)
	}
}

func TestAFilterThatCannotAnswerRefusesRatherThanForwarding(t *testing.T) {
	cases := []struct {
		name  string
		reply func([]string) string
	}{
		{"prose instead of an array", func([]string) string { return "I have redacted it." }},
		{"the wrong number of segments", func([]string) string { return `["one"]` }},
		{"nothing at all", func([]string) string { return "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := filterHarness(t, tc.reply, nil)
			resp := h.post(t, "/v1/chat/completions",
				`{"model":"keera-code","messages":[{"role":"user","content":"a"},`+
					`{"role":"assistant","content":"b"}]}`)
			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502; a filter that cannot run must not "+
					"let the request past", resp.StatusCode)
			}
			select {
			case body := <-h.upstreamBodies:
				if !strings.Contains(string(body), "guard-served") {
					t.Fatalf("the request was forwarded unfiltered: %s", body)
				}
			default:
				t.Fatal("nothing reached the backend at all")
			}
			select {
			case body := <-h.upstreamBodies:
				t.Fatalf("a second request was forwarded after the filter failed: %s", body)
			default:
			}
			// The generation still happened, so it is still charged.
			if ev := h.sink.last(t); ev.CostMicros == 0 {
				t.Error("the failed filter's spend was not recorded")
			}
		})
	}
}

func TestAGuardrailNamingAMissingFilterRefuses(t *testing.T) {
	h := filterHarness(t, redactor, nil)
	h.src.filters = nil

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"hunter2"}]}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "redact") {
		t.Errorf("the refusal does not name the filter: %s", body)
	}
	select {
	case body := <-h.upstreamBodies:
		t.Fatalf("the request was forwarded with no filter having run: %s", body)
	default:
	}
}

func TestFilterRunsBeforeTheSystemPromptIsAdded(t *testing.T) {
	resolved := policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"}, nil, nil, nil)
	resolved.Filters = []string{"redact"}
	resolved.SystemPrompt = "Answer in British English."

	// A filter that would mangle anything it is shown proves the guardrail's
	// own wording was never shown to it.
	h := filterHarness(t, func(segments []string) string {
		out := make([]string, len(segments))
		for i := range segments {
			out[i] = "REWRITTEN"
		}
		encoded, _ := json.Marshal(out)
		return string(encoded)
	}, resolved)

	h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"hello"}]}`)

	filterCall := string(<-h.upstreamBodies)
	if strings.Contains(filterCall, "British English") {
		t.Errorf("the guardrail's own system prompt was handed to the filter: %s", filterCall)
	}
	forwarded := string(<-h.upstreamBodies)
	if !strings.Contains(forwarded, "British English") {
		t.Errorf("the system prompt did not survive filtering: %s", forwarded)
	}
	if !strings.Contains(forwarded, "REWRITTEN") {
		t.Errorf("the filtered message was not forwarded: %s", forwarded)
	}
}

func TestFilterReachesEveryAddressableTextInARequest(t *testing.T) {
	var seen []string
	h := filterHarness(t, func(segments []string) string {
		seen = append([]string(nil), segments...)
		return redactor(segments)
	}, nil)

	h.post(t, "/v1/chat/completions", `{"model":"keera-code","messages":[
		{"role":"system","content":"plain string content"},
		{"role":"user","content":[
			{"type":"text","text":"a text part"},
			{"type":"image_url","image_url":{"url":"data:x"}}
		]},
		{"role":"tool","tool_call_id":"c1","content":"a tool result with hunter2"}
	]}`)

	want := []string{
		"plain string content", "a text part", "a tool result with hunter2",
	}
	if len(seen) != len(want) {
		t.Fatalf("the filter saw %d segments (%q), want %d", len(seen), seen, len(want))
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("segment %d = %q, want %q", i, seen[i], want[i])
		}
	}

	<-h.upstreamBodies
	forwarded := string(<-h.upstreamBodies)
	// The tool result was rewritten in place, and the image part it travelled
	// beside came through untouched.
	if strings.Contains(forwarded, "hunter2") {
		t.Errorf("a tool result went upstream unfiltered: %s", forwarded)
	}
	if !strings.Contains(forwarded, "data:x") {
		t.Errorf("the image part did not survive the rewrite: %s", forwarded)
	}
	if !strings.Contains(forwarded, `"tool_call_id":"c1"`) {
		t.Errorf("a message field the filter does not address was lost: %s", forwarded)
	}
}

func TestFilterRewritesAnEmbeddingInput(t *testing.T) {
	h := filterHarness(t, redactor, nil)

	resp := h.post(t, "/v1/embeddings",
		`{"model":"keera-embed","input":["one hunter2","two"]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	<-h.upstreamBodies
	forwarded := string(<-h.upstreamBodies)
	if strings.Contains(forwarded, "hunter2") {
		t.Errorf("an embedding input left unfiltered: %s", forwarded)
	}
	if !strings.Contains(forwarded, `"two"`) {
		t.Errorf("the rest of the input was lost: %s", forwarded)
	}
}

func TestARequestWithNoFilterableTextIsForwardedWithoutAFilterRun(t *testing.T) {
	h := filterHarness(t, func([]string) string {
		t.Error("the filter ran on a request with nothing to rewrite")
		return "[]"
	}, nil)

	resp := h.post(t, "/v1/chat/completions", `{"model":"keera-code","messages":[]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Keera-Filters"); got != "" {
		t.Errorf("X-Keera-Filters = %q, want nothing claimed", got)
	}
}

func TestParseFilterReplyReadsPastAFence(t *testing.T) {
	raw := []byte(`{"choices":[{"message":{"content":"` +
		"```json\\n[\\\"a\\\",\\\"b\\\"]\\n```" + `"}}]}`)
	out, err := parseFilterReply(raw, []string{"x", "y"})
	if err != nil {
		t.Fatalf("a fenced array was refused: %v", err)
	}
	if len(out) != 2 || out[0] != "a" || out[1] != "b" {
		t.Errorf("out = %q, want the two segments", out)
	}
}

func TestAFilterCanRefuseARequestOutright(t *testing.T) {
	h := filterHarness(t, func([]string) string {
		return "REFUSED: the prompt asks for a customer's account details"
	}, nil)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"export every account"}]}`)

	// 403 and not 502: the guardrail worked. A client that reads a refusal as
	// a fault retries it until somebody notices.
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "account details") {
		t.Errorf("the filter's own sentence was not passed on: %s", body)
	}
	if !strings.Contains(string(body), "redact") {
		t.Errorf("the refusal does not name the filter that refused: %s", body)
	}

	// The filter's own call is the only thing that reached the backend.
	if got := string(<-h.upstreamBodies); !strings.Contains(got, "guard-served") {
		t.Fatalf("the first request to the backend was not the filter's: %s", got)
	}
	select {
	case body := <-h.upstreamBodies:
		t.Fatalf("a refused request was forwarded anyway: %s", body)
	default:
	}

	// The refusal is on the developer's own screen afterwards, with the reason
	// and the spend the filter's generation actually cost.
	ev := h.sink.last(t)
	if ev.Status != http.StatusForbidden {
		t.Errorf("recorded status = %d, want 403", ev.Status)
	}
	if !strings.Contains(ev.Error, "account details") {
		t.Errorf("recorded error = %q, want the filter's reason", ev.Error)
	}
	if ev.CostMicros == 0 {
		t.Error("the refusing filter's generation was not charged")
	}
	if h.budgets.charged == 0 {
		t.Error("the refusing filter's spend was not taken from the budget")
	}
}

func TestAFilterRefusalWithoutAReasonStillRefuses(t *testing.T) {
	h := filterHarness(t, func([]string) string { return "REFUSED" }, nil)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"a"}]}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	select {
	case body := <-h.upstreamBodies:
		if !strings.Contains(string(body), "guard-served") {
			t.Fatalf("a refused request was forwarded: %s", body)
		}
	default:
		t.Fatal("nothing reached the backend at all")
	}
}

func TestARefusalInEverySegmentIsARefusalAndNotARewrite(t *testing.T) {
	// The mistake the protocol invites: a filter told to work segment by
	// segment, and then told it may refuse, refuses each segment. Forwarding
	// that would replace the whole conversation with the word REFUSED and call
	// it a rewrite.
	h := filterHarness(t, func(segments []string) string {
		out := make([]string, len(segments))
		for i := range out {
			out[i] = "REFUSED: a credential"
		}
		encoded, _ := json.Marshal(out)
		return string(encoded)
	}, nil)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"a"},`+
			`{"role":"assistant","content":"b"}]}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	<-h.upstreamBodies
	select {
	case body := <-h.upstreamBodies:
		t.Fatalf("a conversation of REFUSED was forwarded as a rewrite: %s", body)
	default:
	}
}

func TestAnEchoedRefusalIsNotARefusal(t *testing.T) {
	// A prompt about something else being refused, handed back untouched by a
	// filter with nothing to redact in it. The request is the developer's, not
	// a verdict on the developer.
	const prompt = "REFUSED: connect to db-01 - why does psql say that?"
	h := filterHarness(t, redactor, nil)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":`+
			mustJSON(prompt)+`}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; a request that talks about a refusal is not one",
			resp.StatusCode)
	}
	<-h.upstreamBodies
	if got := string(<-h.upstreamBodies); !strings.Contains(got, "db-01") {
		t.Errorf("the request was not forwarded intact: %s", got)
	}
}

func TestFilterRefusalReasonIsBoundedAndOneLine(t *testing.T) {
	// The sentence is a small model's prose about a prompt it has just read, so
	// a prompt can talk it into anything. It lands in an error body, a usage
	// row and a log line, and does so once per attempt.
	h := filterHarness(t, func([]string) string {
		return "REFUSED: line one\nline two\x07 " + strings.Repeat("x", 4000)
	}, nil)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"a"}]}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	ev := h.sink.last(t)
	if len(ev.Error) > 600 {
		t.Errorf("recorded error is %d bytes; the reason is not bounded", len(ev.Error))
	}
	if strings.ContainsAny(ev.Error, "\n\r\x07") {
		t.Errorf("recorded error carries control characters: %q", ev.Error)
	}
	if !strings.Contains(ev.Error, "line one line two") {
		t.Errorf("recorded error = %q, want the reason collapsed to one line", ev.Error)
	}
}

func TestRefusalReasonReadsAModelsFormattingHabits(t *testing.T) {
	refuses := []struct {
		name, in, want string
	}{
		{"the plain form", "REFUSED: a credential", "a credential"},
		{"no reason", "REFUSED", ""},
		{"emphasised", "**REFUSED**: a credential", "a credential"},
		{"fenced", "```\nREFUSED: a credential\n```", "a credential"},
		{"lowercase", "refused: a credential", "a credential"},
		{"a full stop", "REFUSED. a credential", "a credential"},
		{"no separator at all", "REFUSED a credential", "a credential"},
	}
	for _, tc := range refuses {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := refusalReason(tc.in)
			if !ok {
				t.Fatalf("refusalReason(%q) did not read a refusal", tc.in)
			}
			if got != tc.want {
				t.Errorf("reason = %q, want %q", got, tc.want)
			}
		})
	}

	// The word has to end where the word ends. A request dropped by a
	// coincidence of spelling is the cheap mistake on this path.
	rewrites := []string{
		"REFUSEDXYZ is not a verdict",
		"I have not REFUSED anything",
		"The connection was REFUSED: check the firewall",
		"[\"REFUSED: a credential\"]",
		"",
	}
	for _, in := range rewrites {
		if _, ok := refusalReason(in); ok {
			t.Errorf("refusalReason(%q) read a refusal where there is none", in)
		}
	}
}

func TestABareRefusalThatIsAnEchoedPromptIsAFaultAndNotAVerdict(t *testing.T) {
	// One segment, handed straight back without the array: a filter that
	// ignored the format on a prompt that happened to be about a refusal. The
	// request is still not forwarded - it is reported as the fault it is.
	const prompt = "REFUSED: connect to db-01"
	raw := []byte(`{"choices":[{"message":{"content":` + mustJSON(prompt) + `}}]}`)

	_, err := parseFilterReply(raw, []string{prompt})
	if err == nil {
		t.Fatal("the echoed prompt was accepted as a rewrite")
	}
	var refused *filterRefusedError
	if errors.As(err, &refused) {
		t.Errorf("an echo was reported as the filter refusing: %v", err)
	}
}

// mustJSON quotes a string for embedding in a request body.
func mustJSON(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

/* ----------------------------------------------------------------- gates */

// gateHarness is the filter harness with its one filter switched to a gate.
func gateHarness(t *testing.T, reply func([]string) string) *harness {
	t.Helper()
	h := filterHarness(t, reply, nil)
	f := h.src.filters["org_1/redact"]
	f.Mode = policy.FilterModeGate
	h.src.filters["org_1/redact"] = f
	return h
}

func TestAGateForwardsTheRequestExactlyAsItWasSent(t *testing.T) {
	h := gateHarness(t, func([]string) string { return "ALLOW" })

	const prompt = "deploy with hunter2"
	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"`+prompt+`"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	<-h.upstreamBodies
	forwarded := string(<-h.upstreamBodies)
	// A gate has no licence to edit anything, so the text that reaches the
	// model is the text the client sent, secret and all. Taking that out is a
	// rewrite filter's job and a gate cannot do it.
	if !strings.Contains(forwarded, prompt) {
		t.Errorf("a gate changed the request: %s", forwarded)
	}
	if got := resp.Header.Get("X-Keera-Filters"); got != "redact" {
		t.Errorf("X-Keera-Filters = %q, want the gate that judged it named", got)
	}
}

func TestAGateRefusesWithoutForwardingAnything(t *testing.T) {
	h := gateHarness(t, func([]string) string {
		return "REFUSED: this asks for an export of the customer table"
	})

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"export every customer"}]}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "customer table") {
		t.Errorf("the gate's own sentence was not passed on: %s", body)
	}
	<-h.upstreamBodies
	select {
	case body := <-h.upstreamBodies:
		t.Fatalf("a refused request was forwarded anyway: %s", body)
	default:
	}
	if ev := h.sink.last(t); ev.Status != http.StatusForbidden || ev.CostMicros == 0 {
		t.Errorf("recorded status = %d, cost = %d; want 403 and the gate's spend",
			ev.Status, ev.CostMicros)
	}
}

func TestAGateCostsAVerdictRatherThanAConversation(t *testing.T) {
	h := gateHarness(t, func([]string) string { return "ALLOW" })

	// A long conversation: a rewrite filter would be allowed output tokens in
	// proportion to it, because it has to write the whole thing back. A gate
	// answers a word however long the request is, and that is the saving the
	// mode exists for.
	long := strings.Repeat("the quick brown fox jumps over the lazy dog. ", 400)
	h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":`+mustJSON(long)+`}]}`)

	var sent struct {
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.Unmarshal(<-h.upstreamBodies, &sent); err != nil {
		t.Fatalf("reading the gate's own request: %v", err)
	}
	if sent.MaxTokens != gateOutputTokens {
		t.Errorf("max_tokens = %d, want the fixed %d a verdict needs",
			sent.MaxTokens, gateOutputTokens)
	}
}

func TestAGateThatAnswersNeitherVerdictRefuses(t *testing.T) {
	cases := []struct {
		name  string
		reply func([]string) string
	}{
		{"prose", func([]string) string { return "This request looks fine to me." }},
		{"a rewrite", func(segments []string) string {
			encoded, _ := json.Marshal(segments)
			return string(encoded)
		}},
		{"nothing at all", func([]string) string { return "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := gateHarness(t, tc.reply)
			resp := h.post(t, "/v1/chat/completions",
				`{"model":"keera-code","messages":[{"role":"user","content":"a"}]}`)
			// Fails closed: the only way past a gate is a word it recognises.
			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", resp.StatusCode)
			}
			<-h.upstreamBodies
			select {
			case body := <-h.upstreamBodies:
				t.Fatalf("the request went to the model unjudged: %s", body)
			default:
			}
		})
	}
}

func TestAGateReadsTheWordsASmallModelActuallyWrites(t *testing.T) {
	allowed := []string{"ALLOW", "ALLOWED", "allow", "**ALLOW**", "ALLOW: nothing sensitive"}
	for _, reply := range allowed {
		t.Run("allowing with "+reply, func(t *testing.T) {
			h := gateHarness(t, func([]string) string { return reply })
			resp := h.post(t, "/v1/chat/completions",
				`{"model":"keera-code","messages":[{"role":"user","content":"a"}]}`)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200; %q is a gate allowing a request",
					resp.StatusCode, reply)
			}
		})
	}

	// "REFUSE" is read as a refusal here and not on the rewrite path. A gate's
	// answer is nothing but a verdict, so there is no prose for the word to
	// collide with.
	for _, reply := range []string{"REFUSE: a credential", "REFUSED", "refused: a credential"} {
		t.Run("refusing with "+reply, func(t *testing.T) {
			h := gateHarness(t, func([]string) string { return reply })
			resp := h.post(t, "/v1/chat/completions",
				`{"model":"keera-code","messages":[{"role":"user","content":"a"}]}`)
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; %q is a gate refusing a request",
					resp.StatusCode, reply)
			}
		})
	}
}

func TestAGateAfterARewriteJudgesTheRedactedText(t *testing.T) {
	// The order the chain already runs in, and the right way round: what a gate
	// decides on is what would actually have been sent, not what was typed.
	resolved := policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"}, nil, nil, nil)
	resolved.Filters = []string{"redact", "escalate"}

	var judged []string
	h := filterHarnessWith(t, func(instruction string, segments []string) string {
		if strings.Contains(instruction, "may not go") {
			judged = segments
			return "ALLOW"
		}
		return redactor(segments)
	}, resolved)
	h.src.filters["org_1/escalate"] = policy.Filter{
		OrgID: "org_1", Alias: "escalate", Model: "keera-guard",
		Mode: policy.FilterModeGate, Prompt: "Refuse a request that may not go.",
	}

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"deploy with hunter2"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(judged) != 1 || strings.Contains(judged[0], "hunter2") {
		t.Errorf("the gate judged %q; it should see what the rewrite left", judged)
	}
	if got := resp.Header.Get("X-Keera-Filters"); got != "redact, escalate" {
		t.Errorf("X-Keera-Filters = %q, want both in the order they ran", got)
	}
}

func TestCheckingAGateSendsBothHalvesOfTheSampleSeparately(t *testing.T) {
	// A gate answers once per request, so the credential and the ordinary code
	// cannot go in one: a request carrying both is refused for the credential
	// and says nothing about whether ordinary work gets through.
	var asked [][]string
	h := gateHarness(t, func(segments []string) string {
		asked = append(asked, segments)
		for _, s := range segments {
			if strings.Contains(s, "AWS_SECRET_ACCESS_KEY") {
				return "REFUSED: the request carries a credential"
			}
		}
		return "ALLOW"
	})

	f := h.src.filters["org_1/redact"]
	m, _ := h.src.Model("keera-guard")
	p := h.server().CheckFilter(context.Background(), f, m)

	if !p.OK || p.Error != "" {
		t.Fatalf("the check failed: ok = %v, error = %q", p.OK, p.Error)
	}
	if p.Mode != policy.FilterModeGate {
		t.Errorf("mode = %q, want the gate it is", p.Mode)
	}
	if len(asked) != 2 {
		t.Fatalf("the gate was asked %d times, want the two halves separately", len(asked))
	}
	if len(p.Verdicts) != 2 {
		t.Fatalf("verdicts = %+v, want one per half", p.Verdicts)
	}
	if !p.Verdicts[0].Refused || !p.Verdicts[0].ExpectRefusal {
		t.Errorf("the half carrying a credential was not refused: %+v", p.Verdicts[0])
	}
	if p.Verdicts[1].Refused || p.Verdicts[1].ExpectRefusal {
		t.Errorf("ordinary source code was refused: %+v", p.Verdicts[1])
	}
	if len(p.Warnings) != 0 {
		t.Errorf("warnings = %q, want none from a gate that answered both correctly",
			p.Warnings)
	}
	// A rewrite filter's reading of the same sample has nothing to say here:
	// nothing was rewritten, so there are no segments to show.
	if len(p.Segments) != 0 {
		t.Errorf("segments = %+v, want none from a gate", p.Segments)
	}
}

func TestCheckingAGateThatRefusesEverythingWarnsAboutIt(t *testing.T) {
	// The expensive mistake for this mode, and the one nothing downstream
	// reports: every request the guardrail covers is stopped, and each sender
	// is told only that a guardrail refused them.
	h := gateHarness(t, func([]string) string { return "REFUSED: it mentions a system" })

	f := h.src.filters["org_1/redact"]
	m, _ := h.src.Model("keera-guard")
	p := h.server().CheckFilter(context.Background(), f, m)

	if !p.OK {
		t.Fatalf("the check failed rather than warning: %q", p.Error)
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "source code was refused") {
		t.Errorf("warnings = %q, want the eager gate named", p.Warnings)
	}
}

func TestCheckingAGateWhoseModelCannotAnswerIsAFailure(t *testing.T) {
	h := gateHarness(t, func([]string) string { return "I would allow this, probably." })

	f := h.src.filters["org_1/redact"]
	m, _ := h.src.Model("keera-guard")
	p := h.server().CheckFilter(context.Background(), f, m)

	if p.OK {
		t.Fatal("a gate whose answer cannot be read passed its check")
	}
	if !strings.Contains(p.Error, "ALLOW") {
		t.Errorf("error = %q, want it to say what the answer should have been", p.Error)
	}
}

/* ------------------------------------------------------------------ shadow */

// shadowHarness is the redaction filter, not enforcing.
func shadowHarness(t *testing.T, reply func([]string) string) *harness {
	t.Helper()
	h := filterHarness(t, reply, nil)
	f := h.src.filters["org_1/redact"]
	f.Shadow = true
	h.src.filters["org_1/redact"] = f
	return h
}

// filterRuns is the filter log the request wrote, which is where everything
// about a shadow filter has to show up: it changed nothing about the answer, so
// the log is the only place its behaviour exists at all.
func filterRuns(t *testing.T, h *harness) []store.FilterRun {
	t.Helper()
	return h.sink.last(t).FilterRuns
}

func TestAShadowFilterForwardsTheRequestExactlyAsItWasSent(t *testing.T) {
	h := shadowHarness(t, redactor)

	const prompt = "deploy with hunter2"
	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"`+prompt+`"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	<-h.upstreamBodies
	forwarded := string(<-h.upstreamBodies)
	// The whole promise of shadow: the filter read the request, would have
	// taken the credential out of it, and the request went as it was sent.
	if !strings.Contains(forwarded, prompt) {
		t.Errorf("a shadow filter changed the request: %s", forwarded)
	}
	// And it is not claimed to have acted. A header naming it would tell a
	// client its request had been through a guardrail that was not enforcing.
	if got := resp.Header.Get("X-Keera-Filters"); got != "" {
		t.Errorf("X-Keera-Filters = %q, want nothing: no filter acted", got)
	}

	runs := filterRuns(t, h)
	if len(runs) != 1 {
		t.Fatalf("recorded %d filter runs, want the one that ran", len(runs))
	}
	if !runs[0].Shadow || runs[0].Outcome != store.FilterRewrite {
		t.Errorf("run = %+v, want a shadow run recorded as the rewrite it would have made",
			runs[0])
	}
	if runs[0].Segments != 1 || runs[0].Changed != 1 {
		t.Errorf("segments = %d, changed = %d; want the one segment it would have edited",
			runs[0].Segments, runs[0].Changed)
	}
}

func TestAShadowFilterThatRefusesDoesNotStopTheRequest(t *testing.T) {
	h := shadowHarness(t, func([]string) string {
		return "REFUSED: this asks for an export of the customer table"
	})

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"export every customer"}]}`)
	// The refusal that did not happen. It is the number a guardrail is rolled
	// out on, and the request it was about was served.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: a shadow filter refuses nothing", resp.StatusCode)
	}
	<-h.upstreamBodies
	select {
	case <-h.upstreamBodies:
	default:
		t.Fatal("the request was not forwarded")
	}
	runs := filterRuns(t, h)
	if len(runs) != 1 || runs[0].Outcome != store.FilterRefuse || !runs[0].Shadow {
		t.Errorf("runs = %+v, want the refusal recorded as one that would have happened", runs)
	}
}

func TestAShadowFilterThatCannotRunDoesNotStopTheRequest(t *testing.T) {
	// Everywhere else this refuses the request: a control that can be turned
	// off by breaking it is not a control. A measurement is not a control, and
	// one that takes a department offline is not a measurement anybody would
	// agree to make.
	h := shadowHarness(t, func([]string) string { return "I have redacted it." })

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"hunter2"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: a shadow filter that broke stops nothing",
			resp.StatusCode)
	}
	runs := filterRuns(t, h)
	if len(runs) != 1 || runs[0].Outcome != store.FilterError {
		t.Errorf("runs = %+v, want the failure recorded: a shadow filter that cannot "+
			"run is measuring nothing", runs)
	}
}

func TestAShadowFilterWhoseModelIsGoneDoesNotStopTheRequest(t *testing.T) {
	h := shadowHarness(t, redactor)
	f := h.src.filters["org_1/redact"]
	f.Model = "keera-vanished"
	h.src.filters["org_1/redact"] = f

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"hunter2"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	runs := filterRuns(t, h)
	if len(runs) != 1 || runs[0].Outcome != store.FilterError {
		t.Errorf("runs = %+v, want a run recorded for a filter that could not run at all",
			runs)
	}
}

func TestAShadowFilterIsStillCharged(t *testing.T) {
	h := shadowHarness(t, redactor)

	h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"hunter2"}]}`)

	// The same 600 micro-units the enforcing filter costs. Shadow buys
	// knowledge, not a discount: the generation happened, and a filter that
	// could be made free by not enforcing it would be a filter nobody costed.
	const filterMicros = 600
	if h.budgets.charged < filterMicros {
		t.Errorf("charged = %d, want the shadow filter's spend included",
			h.budgets.charged)
	}
	runs := filterRuns(t, h)
	if len(runs) != 1 || runs[0].CostMicros != filterMicros {
		t.Errorf("runs = %+v, want the run charged %d", runs, filterMicros)
	}
}

func TestAShadowRewriteIsNotHandedToTheNextFilter(t *testing.T) {
	// A shadow rewrite is a rewrite that did not happen, so the gate after it
	// has to judge the text that will actually be sent - not the text it would
	// have been sent if the filter in front had been enforcing.
	resolved := policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"}, nil, nil, nil)
	resolved.Filters = []string{"redact", "escalate"}

	var judged []string
	h := filterHarnessWith(t, func(instruction string, segments []string) string {
		if strings.Contains(instruction, "may not go") {
			judged = segments
			return "ALLOW"
		}
		return redactor(segments)
	}, resolved)
	shadowed := h.src.filters["org_1/redact"]
	shadowed.Shadow = true
	h.src.filters["org_1/redact"] = shadowed
	h.src.filters["org_1/escalate"] = policy.Filter{
		OrgID: "org_1", Alias: "escalate", Model: "keera-guard",
		Mode: policy.FilterModeGate, Prompt: "Refuse a request that may not go.",
	}

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"deploy with hunter2"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(judged) != 1 || !strings.Contains(judged[0], "hunter2") {
		t.Errorf("the gate judged %q; it should see the text that will be sent", judged)
	}
	if got := resp.Header.Get("X-Keera-Filters"); got != "escalate" {
		t.Errorf("X-Keera-Filters = %q, want only the filter that acted", got)
	}
	runs := filterRuns(t, h)
	if len(runs) != 2 || runs[0].Shadow == runs[1].Shadow {
		t.Fatalf("runs = %+v, want both filters recorded and told apart", runs)
	}
}

/* ------------------------------------------------------------ the filter log */

func TestAFilterRunIsRecordedWithWhatItDid(t *testing.T) {
	h := filterHarness(t, redactor, nil)

	h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"deploy with hunter2"},`+
			`{"role":"user","content":"and tell me why"}]}`)

	runs := filterRuns(t, h)
	if len(runs) != 1 {
		t.Fatalf("recorded %d runs, want one", len(runs))
	}
	run := runs[0]
	switch {
	case run.Filter != "redact":
		t.Errorf("filter = %q, want the one that ran", run.Filter)
	case run.Mode != policy.FilterModeRewrite:
		// A filter stored before there were modes says nothing, and the log has
		// to say what it actually was rather than repeat the blank.
		t.Errorf("mode = %q, want it filled in as the rewrite it is", run.Mode)
	case run.Shadow:
		t.Error("an enforcing filter was recorded as a shadow run")
	case run.Outcome != store.FilterRewrite:
		t.Errorf("outcome = %q, want a rewrite", run.Outcome)
	case run.Segments != 2 || run.Changed != 1:
		// Counts, and nothing about the text: what makes "this instruction
		// rewrites most of what it sees" answerable without keeping any of it.
		t.Errorf("segments = %d, changed = %d; want 2 shown and the 1 it edited",
			run.Segments, run.Changed)
	case run.CostMicros == 0:
		t.Error("the run was recorded as free")
	}
}

func TestAFilterThatChangedNothingIsRecordedAsAPass(t *testing.T) {
	// The distinction the log exists for. This filter worked and did nothing,
	// and a filter doing nothing to a department's whole week is either one
	// that is not needed or an instruction that is not finding what it was
	// written to find - neither of which "it ran" would ever say.
	h := filterHarness(t, redactor, nil)

	h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"why does the pod restart"}]}`)

	runs := filterRuns(t, h)
	if len(runs) != 1 || runs[0].Outcome != store.FilterPass || runs[0].Changed != 0 {
		t.Errorf("runs = %+v, want one pass that changed nothing", runs)
	}
}

func TestARefusedRequestStillCarriesItsFilterRuns(t *testing.T) {
	// The refusal is the case the log is read for most often, so it must not be
	// the one outcome that goes unrecorded.
	h := gateHarness(t, func([]string) string { return "REFUSED: not this one" })

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"export every customer"}]}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	runs := filterRuns(t, h)
	if len(runs) != 1 || runs[0].Outcome != store.FilterRefuse ||
		runs[0].Mode != policy.FilterModeGate {
		t.Errorf("runs = %+v, want the gate's refusal recorded", runs)
	}
}

func TestAFilterThatBrokeIsRecordedOnTheRequestItRefused(t *testing.T) {
	h := filterHarness(t, func([]string) string { return "I have redacted it." }, nil)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"hunter2"}]}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	// An enforcing filter that cannot run refuses everything its guardrails
	// cover. The count that says so has to live somewhere other than in the
	// requests it refused, or the only symptom is a department complaining.
	runs := filterRuns(t, h)
	if len(runs) != 1 || runs[0].Outcome != store.FilterError {
		t.Errorf("runs = %+v, want the failure recorded against the filter", runs)
	}
}

func TestARequestWithNoFilterableTextRecordsNoFilterRun(t *testing.T) {
	// The filters were not run, and are not claimed to have run either - a row
	// saying a filter passed a request it never read would make every rate on
	// its screen wrong.
	h := filterHarness(t, redactor, nil)

	h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":[{"type":"image_url",`+
			`"image_url":{"url":"data:image/png;base64,iVBOR"}}]}]}`)

	if runs := filterRuns(t, h); len(runs) != 0 {
		t.Errorf("runs = %+v, want none: there was nothing for a filter to read", runs)
	}
}

/* ---------------------------------- gates read from the first token */

// gateHarnessServingLogprobs is a gate in front of a plane that serves a
// distribution, as an inference plane of your own does.
//
// reply is what the model writes out; allow is how much of the first token the
// ALLOW verdict holds, against REFUSED holding the rest. The two are set
// independently on purpose - a model whose prose and whose first token
// disagree is the case the reading has to be careful about.
func gateHarnessServingLogprobs(t *testing.T, reply string, allow float64) *harness {
	t.Helper()

	backend := func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var sent struct {
			Model    string `json:"model"`
			Logprobs bool   `json:"logprobs"`
		}
		_ = json.Unmarshal(raw, &sent)
		w.Header().Set("Content-Type", "application/json")

		if sent.Model != "guard-served" {
			_, _ = io.WriteString(w, `{"id":"1","choices":[{"message":{"content":"hi"}}],`+
				`"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
			return
		}
		content, _ := json.Marshal(reply)
		body := `{"id":"2","choices":[{"message":{"content":` + string(content) + `}`
		if sent.Logprobs {
			// The first token is whichever verdict holds most of the mass,
			// which is what a plane decoding greedily would have written.
			token := "ALLOW"
			if allow < 0.5 {
				token = "REFUSED"
			}
			body += fmt.Sprintf(`,"logprobs":{"content":[{"token":%q,"logprob":%v,`+
				`"top_logprobs":[{"token":%q,"logprob":%v},{"token":%q,"logprob":%v}]}]}`,
				token, math.Log(math.Max(allow, 1-allow)),
				"ALLOW", math.Log(allow), "REFUSED", math.Log(1-allow))
		}
		body += `}],"usage":{"prompt_tokens":100,"completion_tokens":2}}`
		_, _ = io.WriteString(w, body)
	}

	resolved := policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"}, nil, nil, nil)
	resolved.Filters = []string{"redact"}
	models := map[string]policy.Model{
		"keera-code": {
			Alias: "keera-code", Kind: policy.KindChat, BackendModel: "served-name",
			InputMicrosPerMTok: 1_000_000, OutputMicrosPerMTok: 4_000_000, Enabled: true,
		},
		"keera-guard": {
			Alias: "keera-guard", Kind: policy.KindChat, BackendModel: "guard-served",
			InputMicrosPerMTok: 2_000_000, OutputMicrosPerMTok: 2_000_000, Enabled: true,
		},
	}
	h := newHarness(t, backend, models, resolved)
	h.src.filters = map[string]policy.Filter{
		"org_1/redact": {
			OrgID: "org_1", Alias: "redact", Mode: policy.FilterModeGate,
			Model: "keera-guard", Prompt: "Refuse anything carrying a credential.",
		},
	}
	return h
}

// A gate asks for the distribution behind its verdict. A rewrite filter does
// not: its answer is a whole array of text, and the first token of it says
// nothing about anything.
func TestOnlyAGateAsksForTheDistribution(t *testing.T) {
	gate := gateHarness(t, func([]string) string { return "ALLOW" })
	if resp := gate.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"a"}]}`,
	); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	asked := string(<-gate.upstreamBodies)
	if !strings.Contains(asked, `"logprobs":true`) {
		t.Errorf("the gate did not ask for a distribution: %s", asked)
	}
	// The allowance is untouched: a refusal still carries a written sentence.
	if !strings.Contains(asked, `"max_tokens":192`) {
		t.Errorf("the gate's output allowance changed: %s", asked)
	}

	rewrite := filterHarness(t, redactor, nil)
	if resp := rewrite.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"a"}]}`,
	); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if asked := string(<-rewrite.upstreamBodies); strings.Contains(asked, `"logprobs"`) {
		t.Errorf("a rewrite filter asked for a distribution: %s", asked)
	}
}

// The failure this is for. A small model that writes a sentence instead of the
// word used to stop the request - a guardrail refusing ordinary work because
// of how the model it runs on formats an answer. The first token says which
// verdict it was, whatever it wrapped around it.
func TestAVerdictIsReadFromTheFirstTokenWhenTheWordsCannotBe(t *testing.T) {
	tests := []struct {
		name  string
		reply string
		allow float64
		want  int
	}{
		{
			name:  "prose that means yes",
			reply: "This request is ordinary work and may proceed.",
			allow: 0.97, want: http.StatusOK,
		},
		{
			name:  "prose that means no",
			reply: "This one carries a live credential and must not be sent.",
			allow: 0.02, want: http.StatusForbidden,
		},
		{
			name:  "a model that thought aloud before answering",
			reply: "<think>nothing sensitive here</think>",
			allow: 0.95, want: http.StatusOK,
		},
		{
			// It answered with its first token and stopped, which is a plane
			// that returned the distribution and no text worth reading.
			name: "nothing written at all", reply: "", allow: 0.96, want: http.StatusOK,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := gateHarnessServingLogprobs(t, tc.reply, tc.allow)
			resp := h.post(t, "/v1/chat/completions",
				`{"model":"keera-code","messages":[{"role":"user","content":"a"}]}`)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

// A gate that refuses owes the sender a sentence. Where it refused without
// writing the word, the prose it wrote instead is that sentence.
func TestARefusalReadFromTheFirstTokenStillCarriesASentence(t *testing.T) {
	h := gateHarnessServingLogprobs(t,
		"This one carries a live credential and must not be sent.", 0.02)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"a"}]}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "carries a live credential") {
		t.Errorf("the refusal carried no reason: %s", body)
	}

	// And where there was no prose either, it says so rather than nothing.
	h = gateHarnessServingLogprobs(t, "", 0.01)
	resp = h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"a"}]}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "without saying what it found") {
		t.Errorf("the refusal explained nothing at all: %s", body)
	}
}

// The words still decide where they are there. An administrator checked an
// instruction and put it into production, and what the words mean is not
// something a distribution gets to overrule.
func TestTheWrittenVerdictOutranksTheDistribution(t *testing.T) {
	// It wrote REFUSED, and its first token leans the other way.
	h := gateHarnessServingLogprobs(t, "REFUSED: a credential", 0.9)
	if resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"a"}]}`,
	); resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403; the written refusal was overruled", resp.StatusCode)
	}

	h = gateHarnessServingLogprobs(t, "ALLOW", 0.1)
	if resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"a"}]}`,
	); resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200; the written allowance was overruled", resp.StatusCode)
	}
}

// Nothing here loosens the one property a gate rests on. A plane that serves no
// distribution and a model that wrote no verdict is still a request that does
// not go.
func TestAGateWithNeitherReadingStillFailsClosed(t *testing.T) {
	h := gateHarness(t, func([]string) string { return "I would allow this, probably." })
	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"a"}]}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	<-h.upstreamBodies
	select {
	case body := <-h.upstreamBodies:
		t.Fatalf("the request went to the model unjudged: %s", body)
	default:
	}
}

// What a check reports, which is the whole reason the number is kept: a gate
// that answered both halves correctly and could barely tell them apart.
func TestAGateCheckReportsHowSureEachVerdictWas(t *testing.T) {
	h := gateHarnessServingLogprobs(t, "ALLOW", 0.93)

	p := h.srv.CheckFilter(t.Context(), h.src.filters["org_1/redact"],
		h.src.models["keera-guard"])
	if !p.OK {
		t.Fatalf("the check failed: %s", p.Error)
	}
	if len(p.Verdicts) == 0 {
		t.Fatal("the check produced no verdicts")
	}
	for i, v := range p.Verdicts {
		if v.Confidence < 0.92 || v.Confidence > 0.94 {
			t.Errorf("verdict %d was %.3f sure, want the stand-in's 0.93", i+1, v.Confidence)
		}
	}

	// A plane serving no distribution says nothing rather than claiming the
	// gate had no opinion.
	plain := gateHarness(t, func([]string) string { return "ALLOW" })
	p = plain.srv.CheckFilter(t.Context(), plain.src.filters["org_1/redact"],
		plain.src.models["keera-guard"])
	if !p.OK {
		t.Fatalf("the check failed: %s", p.Error)
	}
	for i, v := range p.Verdicts {
		if v.Confidence != 0 {
			t.Errorf("verdict %d claimed %.2f confidence from a plane that reports none",
				i+1, v.Confidence)
		}
	}
}
