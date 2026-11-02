package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// oneTokenFee is 1.00 per million tokens, so a bill in micros is its tokens.
const oneTokenFee = 1_000_000

// withGatewayFee gives one of the harness's models the gateway fee, as the
// registry does on a deployment that charges one.
func withGatewayFee(h *harness, alias string) {
	m := h.src.models[alias]
	m.Billing = &policy.Billing{Provider: m.Provider, Credit: true, FeeMicrosPerMTok: oneTokenFee}
	m.PriceAtFee(oneTokenFee)
	h.src.models[alias] = m
}

// lyingAnswer is a self-hosted backend's answer that says it used nothing.
var lyingAnswer = `{"id":"1","choices":[{"message":{"content":"` + strings.Repeat("x", 400) +
	`"}}],"usage":{"prompt_tokens":0,"completion_tokens":0}}`

// A self-hosted backend can report what it likes, so the gateway bills what it
// passed on: the request it sent and the text that came back.
func TestASelfHostedModelIsBilledWhatTheGatewayCounted(t *testing.T) {
	h := newHarness(t, jsonBackend(lyingAnswer), nil, nil)
	withGatewayFee(h, "keera-code")

	h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"hello"}]}`)

	sent := <-h.upstreamBodies
	ev := h.sink.last(t)
	if len(ev.Bills) != 1 {
		t.Fatalf("bills = %+v, want one", ev.Bills)
	}
	b := ev.Bills[0]
	in := len(sent) / bytesPerTokenTypical
	if !b.GatewayFee || b.InputTokens != in || b.OutputTokens != 100 ||
		b.Micros != int64(in+100) || b.CreditMicros != b.Micros {
		t.Errorf("bill = %+v, want %d in and 100 out at the fee", b, in)
	}
	// The budgets are charged the same, and the row keeps what the backend said.
	if ev.CostMicros != b.Micros || ev.InputTokens != 0 || ev.OutputTokens != 0 {
		t.Errorf("row cost %d with %d in, %d out; want the bill's cost and the reported counts",
			ev.CostMicros, ev.InputTokens, ev.OutputTokens)
	}
}

func TestASelfHostedStreamIsBilledOnItsText(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			chunk := `{"id":"1","choices":[{"index":0,"delta":{"content":"abcdefgh"}}]}`
			h := newHarness(t, sseBackend(chunk, chunk, chunk, stopChunk,
				`{"id":"1","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`),
				nil, nil)
			withGatewayFee(h, "keera-code")

			resp := h.post(t, path, `{"model":"keera-code","max_tokens":100,"stream":true,`+
				`"messages":[{"role":"user","content":"hello"}]}`)
			_, _ = io.Copy(io.Discard, resp.Body)

			ev := h.sink.last(t)
			if len(ev.Bills) != 1 {
				t.Fatalf("bills = %+v, want one", ev.Bills)
			}
			// 24 bytes of text.
			if b := ev.Bills[0]; b.OutputTokens != 24/bytesPerTokenTypical || b.InputTokens <= 1 {
				t.Errorf("bill = %+v, want 6 out and the request's size in", b)
			}
		})
	}
}

// A model at a hosted provider is billed on the provider's own count.
func TestAModelOnTheOrganisationsOwnKeyIsBilledWhatItReported(t *testing.T) {
	h := newHarness(t, jsonBackend(hiAnswer), nil, nil)
	m := h.src.models["keera-code"]
	m.Provider = "mistral"
	h.src.models["keera-code"] = m
	withGatewayFee(h, "keera-code")

	h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"hello"}]}`)

	ev := h.sink.last(t)
	if len(ev.Bills) != 1 {
		t.Fatalf("bills = %+v, want one", ev.Bills)
	}
	if b := ev.Bills[0]; !b.GatewayFee || b.InputTokens != 10 || b.OutputTokens != 5 ||
		b.Micros != 15 || b.Provider != "mistral" {
		t.Errorf("bill = %+v, want the reported 10 in and 5 out at the fee", b)
	}
}

func TestTheGatewayFeeNeedsCredit(t *testing.T) {
	h := newHarness(t, jsonBackend(hiAnswer), nil, nil)
	withGatewayFee(h, "keera-code")
	h.budgets.creditErr = &policy.ErrNoCredit{}

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Errorf("status = %d, want 402", resp.StatusCode)
	}
}

// A filter's call to a self-hosted model is counted the same way.
func TestAFiltersCallToASelfHostedModelPaysTheFee(t *testing.T) {
	h := filterHarness(t, redactor, nil)
	withGatewayFee(h, "keera-guard")

	h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"hunter2"}]}`)

	ev := h.sink.last(t)
	if len(ev.Bills) != 1 || ev.Bills[0].Alias != "keera-guard" || !ev.Bills[0].GatewayFee {
		t.Fatalf("bills = %+v, want the filter's call", ev.Bills)
	}
	// Counted, not the 100 in and 200 out the stand-in reports.
	if b := ev.Bills[0]; b.InputTokens == 100 || b.OutputTokens == 200 || b.OutputTokens == 0 {
		t.Errorf("bill = %+v, want the gateway's count", b)
	}
}

func TestWrittenBytesCountsEverythingTheModelWrote(t *testing.T) {
	tests := []struct {
		name, payload string
		want          int
	}{
		{"content", `{"choices":[{"message":{"content":"hello"}}]}`, 5},
		{
			"reasoning and a tool call",
			`{"choices":[{"message":{"reasoning_content":"hmm","tool_calls":[` +
				`{"function":{"name":"ls","arguments":"{}"}}]}}]}`,
			3 + 2 + 2,
		},
		{"a stream chunk", `{"choices":[{"delta":{"reasoning":"ab","content":"cd"}}]}`, 4},
		{"a text completion", `{"choices":[{"text":"hello"}]}`, 5},
		{"an embedding", `{"data":[{"embedding":[0.1,0.2,0.3]}]}`, 0},
		{"not JSON", `nonsense`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := writtenBytes([]byte(tt.payload)); got != tt.want {
				t.Errorf("writtenBytes = %d, want %d", got, tt.want)
			}
		})
	}
}

// A stream arrives in pieces that cut lines anywhere.
func TestTextCounterReadsLinesCutAnywhere(t *testing.T) {
	stream := sseStream(
		`{"choices":[{"delta":{"content":"abcd"}}]}`,
		`{"choices":[{"delta":{"content":"efgh"}}]}`,
	) + `data: {"choices":[{"delta":{"content":"ijkl"}}]}`
	c := &textCounter{limit: DefaultMaxResponseBytes}
	for i := 0; i < len(stream); i += 7 {
		_, _ = c.Write([]byte(stream[i:min(i+7, len(stream))]))
	}
	// The last line has no newline and still counts.
	if got := c.tokens(); got != 12/bytesPerTokenTypical {
		t.Errorf("tokens = %d, want 3", got)
	}
}
