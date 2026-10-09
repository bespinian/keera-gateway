package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// onPlatformKey puts one of the harness's models on the deployment's key.
func onPlatformKey(h *harness, alias string) {
	m := h.src.models[alias]
	m.Billing = &policy.Billing{Provider: "anthropic", Currency: "USD", DiscountBP: 2000,
		CreditRate: 500_000}
	m.PlatformKey = true
	h.src.models[alias] = m
}

func TestARequestToAModelOnTheDeploymentsKeyIsBilled(t *testing.T) {
	h := filterHarness(t, redactor, nil)
	onPlatformKey(h, "keera-code")

	h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"hunter2"}]}`)

	ev := h.sink.last(t)
	if len(ev.Bills) != 1 {
		t.Fatalf("bills = %+v, want the request's only: the filter's model is the organisation's own", ev.Bills)
	}
	// 10 in at 1.00 and 5 out at 4.00, less 20% for the provider.
	b := ev.Bills[0]
	if b.Alias != "keera-code" || b.InputTokens != 10 || b.OutputTokens != 5 ||
		b.Micros != 30 || b.ProviderMicros != 24 || b.Currency != "USD" {
		t.Errorf("bill = %+v", b)
	}
}

// A filter's model on the deployment's key is paid for too, even when the
// request's own model is not.
func TestAFiltersCallOnTheDeploymentsKeyIsBilled(t *testing.T) {
	h := filterHarness(t, redactor, nil)
	onPlatformKey(h, "keera-guard")

	h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"hunter2"}]}`)

	ev := h.sink.last(t)
	if len(ev.Bills) != 1 || ev.Bills[0].Alias != "keera-guard" {
		t.Fatalf("bills = %+v, want the filter's call", ev.Bills)
	}
	// 100 in and 200 out at 2.00.
	if b := ev.Bills[0]; b.Micros != 600 || b.InputTokens != 100 || b.OutputTokens != 200 {
		t.Errorf("bill = %+v", b)
	}
}

func TestAModelOnTheOrganisationsOwnKeyIsNotBilled(t *testing.T) {
	h := filterHarness(t, redactor, nil)
	h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"hunter2"}]}`)
	if ev := h.sink.last(t); len(ev.Bills) != 0 {
		t.Errorf("bills = %+v, want none", ev.Bills)
	}
}

// What a call on the deployment's key takes from the credit is taken from the
// local view at once, not only when the usage writer gets to it.
func TestACallOnTheDeploymentsKeyTakesTheCredit(t *testing.T) {
	h := filterHarness(t, redactor, nil)
	onPlatformKey(h, "keera-code")

	h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"hunter2"}]}`)

	// USD 30 micros at CHF 0.50 a dollar.
	if b := h.sink.last(t).Bills[0]; b.CreditMicros != 15 {
		t.Errorf("credit = %d, want 15", b.CreditMicros)
	}
	h.budgets.mu.Lock()
	defer h.budgets.mu.Unlock()
	if h.budgets.credited != 15 {
		t.Errorf("credited = %d, want 15", h.budgets.credited)
	}
}

func TestAnOrganisationWithoutCreditCannotUseTheDeploymentsKey(t *testing.T) {
	for _, onKey := range []string{"keera-code", "keera-guard"} {
		t.Run(onKey, func(t *testing.T) {
			h := filterHarness(t, redactor, nil)
			onPlatformKey(h, onKey)
			h.budgets.creditErr = &policy.ErrNoCredit{BalanceMicros: -1_200_000}

			resp := h.post(t, "/v1/chat/completions",
				`{"model":"keera-code","messages":[{"role":"user","content":"hunter2"}]}`)
			if resp.StatusCode != http.StatusPaymentRequired {
				t.Fatalf("status = %d, want 402", resp.StatusCode)
			}
			var envelope struct {
				Error struct{ Message, Code string } `json:"error"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&envelope)
			if envelope.Error.Code != "credit_exhausted" ||
				!strings.Contains(envelope.Error.Message, "CHF -1.20") {
				t.Errorf("error = %+v", envelope.Error)
			}
		})
	}
}

// Without credit, the organisation's own models still work.
func TestCreditDoesNotStopTheOrganisationsOwnModels(t *testing.T) {
	h := filterHarness(t, redactor, nil)
	h.budgets.creditErr = &policy.ErrNoCredit{}
	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"hunter2"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// waitFor polls until done holds, and fails the test if it never does.
func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A request holds the most it may cost while it runs, so requests started
// together cannot all spend the same credit, and gives it back at the end.
func TestARequestOnTheDeploymentsKeyHoldsCreditWhileItRuns(t *testing.T) {
	h := filterHarness(t, redactor, nil)
	onPlatformKey(h, "keera-code")

	body := `{"model":"keera-code","max_tokens":1000,` +
		`"messages":[{"role":"user","content":"hunter2"}]}`
	h.post(t, "/v1/chat/completions", body)

	// The whole body as prompt and the ceiling as answer. The filter's model
	// is the organisation's own, so it adds nothing.
	want, _ := h.src.models["keera-code"].Bill(policy.Tokens{
		Input: len(body) / bytesPerToken, Output: 1000,
	})
	waitFor(t, "the hold to be given back", func() bool {
		h.budgets.mu.Lock()
		defer h.budgets.mu.Unlock()
		return h.budgets.held == 0
	})
	h.budgets.mu.Lock()
	defer h.budgets.mu.Unlock()
	if h.budgets.mostHeld != want.CreditMicros || want.CreditMicros == 0 {
		t.Errorf("held = %d, want %d", h.budgets.mostHeld, want.CreditMicros)
	}
}

// A router's decision is billed on top of the answer, so it is held on top of
// it too.
func TestARoutedRequestHoldsItsDecisionOnTopOfItsAnswer(t *testing.T) {
	h := routerHarness(t, picks("keera-large"), autoRouter(), nil)
	onPlatformKey(h, "keera-picker")
	onPlatformKey(h, "keera-large")

	body := `{"model":"auto","max_tokens":1000,"messages":[{"role":"user","content":"hi"}]}`
	h.post(t, "/v1/chat/completions", body)

	in := len(body) / bytesPerToken
	answer, _ := h.src.models["keera-large"].Bill(policy.Tokens{Input: in, Output: 1000})
	decision, _ := h.src.models["keera-picker"].Bill(policy.Tokens{Input: in, Output: routerOutputTokens})
	want := answer.CreditMicros + 2*decision.CreditMicros
	waitFor(t, "the hold to be given back", func() bool {
		h.budgets.mu.Lock()
		defer h.budgets.mu.Unlock()
		return h.budgets.held == 0
	})
	h.budgets.mu.Lock()
	defer h.budgets.mu.Unlock()
	if h.budgets.mostHeld != want || decision.CreditMicros == 0 {
		t.Errorf("held = %d, want %d", h.budgets.mostHeld, want)
	}
}

func TestOutputCeiling(t *testing.T) {
	for _, tc := range []struct {
		body      string
		guardrail int
		want      int
	}{
		{`{}`, 0, unstatedOutputTokens},
		{`{"max_tokens":500}`, 0, 500},
		{`{"max_output_tokens":700,"max_tokens":500}`, 0, 700},
		{`{"max_completion_tokens":"900"}`, 0, 900},
		{`{"max_tokens":500}`, 200, 200},
		{`{}`, 200, 200},
		{`{"max_tokens":1e300}`, 0, mostOutputTokens},
	} {
		b, err := parseBody([]byte(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		if got := outputCeiling(b, tc.guardrail); got != tc.want {
			t.Errorf("%s with ceiling %d: %d, want %d", tc.body, tc.guardrail, got, tc.want)
		}
	}
}

// A client that hangs up does not end a request on the deployment's key: the
// answer is read to its usage record, which says what the provider charged.
// An estimate would miss what was never streamed, such as reasoning.
func TestAnAnswerOnTheDeploymentsKeyIsReadToTheEndAfterAHangUp(t *testing.T) {
	for _, stream := range []bool{true, false} {
		name := map[bool]string{true: "stream", false: "buffered"}[stream]
		t.Run(name, func(t *testing.T) {
			left := make(chan struct{})
			backend := func(w http.ResponseWriter, _ *http.Request) {
				if !stream {
					<-left
					_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"x"}}],`+
						`"usage":{"prompt_tokens":7,"completion_tokens":900}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				rc := http.NewResponseController(w)
				_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"x"}}]}`+"\n\n")
				_ = rc.Flush()
				<-left
				_, _ = io.WriteString(w, `data: {"choices":[],"usage":{"prompt_tokens":7,`+
					`"completion_tokens":900}}`+"\n\ndata: [DONE]\n\n")
			}
			h := newHarness(t, backend, nil, nil)
			onPlatformKey(h, "keera-code")

			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
					h.url("/v1/chat/completions"), strings.NewReader(
						`{"model":"keera-code","messages":[],"stream":`+
							map[bool]string{true: "true", false: "false"}[stream]+`}`))
				req.Header.Set("Authorization", "Bearer "+testKey)
				if resp, err := http.DefaultClient.Do(req); err == nil {
					_ = resp.Body.Close()
				}
			}()
			<-h.upstreamBodies
			cancel()
			// Long enough for the gateway to see the client go.
			time.Sleep(50 * time.Millisecond)
			close(left)

			waitFor(t, "the request to be recorded", func() bool { return h.sink.count() > 0 })
			ev := h.sink.last(t)
			if ev.OutputTokens != 900 || ev.Estimated {
				t.Errorf("output = %d, estimated = %v; want the provider's 900", ev.OutputTokens,
					ev.Estimated)
			}
			if len(ev.Bills) != 1 || ev.Bills[0].OutputTokens != 900 {
				t.Errorf("bills = %+v, want the provider's 900 output tokens", ev.Bills)
			}
		})
	}
}

// The provider charges for its own tools apart from tokens, which is not
// billed, so they never go out on the deployment's key.
func TestHostedToolsAreTakenOutOnTheDeploymentsKey(t *testing.T) {
	h, seen := providerHarness(t, "anthropic", "claude-opus-5", jsonBackend(anthropicAnswer), nil)
	onPlatformKey(h, "keera-frontier")

	resp := h.post(t, "/v1/messages", `{"model":"keera-frontier","max_tokens":64,`+
		`"tools":[{"name":"read_file","input_schema":{"type":"object"}},`+
		`{"type":"web_search_20250305","name":"web_search"}],`+
		`"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	got := nextSeen(t, seen).body
	if strings.Contains(got, "web_search") || !strings.Contains(got, "read_file") {
		t.Errorf("forwarded = %s", got)
	}
	if h := resp.Header.Get("X-Keera-Removed-Tools"); h != "web_search" {
		t.Errorf("X-Keera-Removed-Tools = %q", h)
	}
}

// Every organisation shares the deployment's OpenAI account, so nothing is
// stored there, and nothing is generated where the gateway cannot bill it.
func TestAResponsesRequestOnTheDeploymentsKeyStoresNothing(t *testing.T) {
	h, seen := providerHarness(t, "openai", "gpt-5.5",
		jsonBackend(`{"model":"gpt-5.5","usage":{"input_tokens":1,"output_tokens":1}}`), nil)
	onPlatformKey(h, "keera-frontier")

	h.post(t, "/v1/responses", `{"model":"keera-frontier","input":"hi","store":true}`)
	if got := nextSeen(t, seen).body; !strings.Contains(got, `"store":false`) {
		t.Errorf("forwarded = %s, want store off", got)
	}

	for _, field := range []string{`"background":true`, `"previous_response_id":"resp_1"`,
		`"conversation":"conv_1"`} {
		resp := h.post(t, "/v1/responses", `{"model":"keera-frontier","input":"hi",`+field+`}`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", field, resp.StatusCode)
		}
	}
	select {
	case r := <-seen:
		t.Errorf("a refused request reached OpenAI: %s", r.body)
	default:
	}
}

// A chat completion is not stored at the deployment's OpenAI account either.
func TestAChatCompletionOnTheDeploymentsKeyIsNotStored(t *testing.T) {
	h, seen := providerHarness(t, "openai", "gpt-5.5",
		jsonBackend(`{"model":"gpt-5.5","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`), nil)
	onPlatformKey(h, "keera-frontier")

	resp := h.post(t, "/v1/chat/completions", `{"model":"keera-frontier","store":true,`+
		`"messages":[{"role":"user","content":"hi"}]}`)
	if got := nextSeen(t, seen).body; strings.Contains(got, `"store"`) {
		t.Errorf("forwarded = %s, want no store", got)
	}
	if h := resp.Header.Get("X-Keera-Removed-Fields"); h != "store" {
		t.Errorf("X-Keera-Removed-Fields = %q", h)
	}
}

// A check calls the model like a request does, so on the deployment's key it
// needs credit and is billed, without a usage row.
func TestAModelCheckOnTheDeploymentsKeyIsBilled(t *testing.T) {
	h := filterHarness(t, redactor, nil)
	onPlatformKey(h, "keera-code")

	h.server().CheckModel(context.Background(), h.src.models["keera-code"])
	ev := h.sink.last(t)
	if !ev.BillsOnly || len(ev.Bills) != 1 || ev.Bills[0].OutputTokens != 128 {
		t.Errorf("event = %+v, want one bill at the check's ceiling and no usage row", ev)
	}

	h.budgets.creditErr = &policy.ErrNoCredit{}
	<-h.upstreamBodies
	p := h.server().CheckModel(context.Background(), h.src.models["keera-code"])
	if !p.NoCredit || p.Reachable {
		t.Errorf("probe = %+v, want it refused for credit", p)
	}
	select {
	case body := <-h.upstreamBodies:
		t.Errorf("a check without credit reached the model: %s", body)
	default:
	}
}

func TestAFilterCheckOnTheDeploymentsKeyIsBilled(t *testing.T) {
	h := filterHarness(t, redactor, nil)
	onPlatformKey(h, "keera-guard")

	h.server().CheckFilter(context.Background(), h.src.filters["org_1/redact"])
	ev := h.sink.last(t)
	if !ev.BillsOnly || len(ev.Bills) != 1 || ev.Bills[0].Alias != "keera-guard" {
		t.Errorf("event = %+v, want the filter model's call billed", ev)
	}

	h.budgets.creditErr = &policy.ErrNoCredit{}
	p := h.server().CheckFilter(context.Background(), h.src.filters["org_1/redact"])
	if p.OK || !strings.Contains(p.Error, "credit") {
		t.Errorf("probe = %+v, want it refused for credit", p)
	}
}

// Postgres refuses a NUL byte in text, and a usage row it refuses takes the
// whole batch with it, other organisations' bills included.
func TestAModelNameWithAControlCharacterIsRefusedAndNotStored(t *testing.T) {
	h := newHarness(t, jsonBackend(`{}`), nil, nil)
	for _, name := range []string{`\u0000`, `keera-code\u0000`, `a\nb`} {
		resp := h.post(t, "/v1/chat/completions", `{"model":"`+name+`","messages":[]}`)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", name, resp.StatusCode)
		}
		if ev := h.sink.last(t); ev.Alias != "" {
			t.Errorf("%s: alias %q was put on the usage row", name, ev.Alias)
		}
	}
}

// A backend that reads "true" as true would stream an answer the gateway
// reads as a whole one, and the request would go unbilled.
func TestAStreamFieldThatIsNotABooleanIsRefused(t *testing.T) {
	h := newHarness(t, jsonBackend(`{"choices":[],"usage":{"prompt_tokens":1}}`), nil, nil)
	for value, want := range map[string]int{
		`"true"`: http.StatusBadRequest, `1`: http.StatusBadRequest,
		`null`: http.StatusOK, `false`: http.StatusOK,
	} {
		resp := h.post(t, "/v1/chat/completions",
			`{"model":"keera-code","messages":[],"stream":`+value+`}`)
		if resp.StatusCode != want {
			t.Errorf("stream %s: status = %d, want %d", value, resp.StatusCode, want)
		}
	}
}

// The event that carries the usage record can quote what the client sent. A
// client must not be able to make it look like another event.
func TestAStreamQuotingAnEventTypeIsBilledFromItsUsageRecord(t *testing.T) {
	stream := func(events ...string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, e := range events {
				_, _ = io.WriteString(w, "data: "+e+"\n\n")
			}
		}
	}
	t.Run("responses", func(t *testing.T) {
		h, _ := providerHarness(t, "openai", "gpt-5.5", stream(
			`{"type":"response.output_text.delta","delta":"hi"}`,
			`{"type":"response.completed","response":{"model":"gpt-5.5",`+
				`"metadata":{"a":"x.delta"},"usage":{"input_tokens":30,"output_tokens":2000}}}`,
		), nil)
		resp := h.post(t, "/v1/responses", `{"model":"keera-frontier","stream":true,`+
			`"metadata":{"a":"x.delta"},"input":"hi"}`)
		_, _ = io.ReadAll(resp.Body)
		if ev := h.sink.last(t); ev.OutputTokens != 2000 || ev.Estimated {
			t.Errorf("output = %d, estimated %v; want the record's 2000", ev.OutputTokens, ev.Estimated)
		}
	})
	t.Run("messages", func(t *testing.T) {
		h, _ := providerHarness(t, "anthropic", "claude-opus-5", stream(
			`{"type":"message_start","message":{"model":"m","usage":{"input_tokens":30,"output_tokens":1}}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"stop_sequence",`+
				`"stop_sequence":"content_block_delta"},"usage":{"output_tokens":2000}}`,
		), nil)
		resp := h.post(t, "/v1/messages", `{"model":"keera-frontier","max_tokens":4000,`+
			`"stream":true,"stop_sequences":["content_block_delta"],`+
			`"messages":[{"role":"user","content":"hi"}]}`)
		_, _ = io.ReadAll(resp.Body)
		if ev := h.sink.last(t); ev.OutputTokens != 2000 || ev.Estimated {
			t.Errorf("output = %d, estimated %v; want the record's 2000", ev.OutputTokens, ev.Estimated)
		}
	})
}

// Fast mode, data residency and a dearer service tier cost more than the list
// price per token, so they never go out on the deployment's key.
func TestPremiumFieldsAreTakenOutOnTheDeploymentsKey(t *testing.T) {
	h, seen := providerHarness(t, "anthropic", "claude-opus-5", jsonBackend(anthropicAnswer), nil)
	onPlatformKey(h, "keera-frontier")

	body := `{"model":"keera-frontier","max_tokens":64,"speed":"fast","inference_geo":"us",` +
		`"service_tier":"priority","messages":[{"role":"user","content":"hi"}]}`
	resp := h.post(t, "/v1/messages", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got := nextSeen(t, seen).body
	for _, field := range []string{"speed", "inference_geo", "service_tier"} {
		if strings.Contains(got, `"`+field+`"`) {
			t.Errorf("%s went out: %s", field, got)
		}
	}
	if h := resp.Header.Get("X-Keera-Removed-Fields"); h != "inference_geo, service_tier, speed" {
		t.Errorf("X-Keera-Removed-Fields = %q", h)
	}

	// A tier at the list price stays, and a model on the organisation's own
	// key is sent as asked.
	h.post(t, "/v1/messages", `{"model":"keera-frontier","max_tokens":64,`+
		`"service_tier":"auto","messages":[{"role":"user","content":"hi"}]}`)
	if got := nextSeen(t, seen).body; !strings.Contains(got, `"service_tier":"auto"`) {
		t.Errorf("forwarded = %s, want the auto tier kept", got)
	}
	h2, seen2 := providerHarness(t, "anthropic", "claude-opus-5", jsonBackend(anthropicAnswer), nil)
	h2.post(t, "/v1/messages", body)
	if got := nextSeen(t, seen2).body; !strings.Contains(got, `"speed":"fast"`) {
		t.Errorf("forwarded = %s, want the organisation's own model sent as asked", got)
	}
}

// Anthropic charges a cache write kept for an hour at twice the input price.
func TestOneHourCacheWritesAreBilledAtTheirOwnRate(t *testing.T) {
	h, _ := providerHarness(t, "anthropic", "claude-opus-5", jsonBackend(
		`{"type":"message","model":"claude-opus-5","content":[],"usage":{"input_tokens":100,`+
			`"cache_creation_input_tokens":1000,"cache_read_input_tokens":0,"output_tokens":0,`+
			`"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":1000}}}`), nil)
	onPlatformKey(h, "keera-frontier")

	h.post(t, "/v1/messages", `{"model":"keera-frontier","max_tokens":64,`+
		`"messages":[{"role":"user","content":"hi"}]}`)
	// 100 in at 1 micro, and 1000 written for an hour at 2.
	if b := h.sink.last(t).Bills; len(b) != 1 || b[0].Micros != 2100 {
		t.Errorf("bills = %+v, want 2100 micros", b)
	}
}

// An answer without a usage record would otherwise cost nothing, though the
// provider charged for it. It is billed the most it may have used.
func TestAnAnswerOnTheDeploymentsKeyWithoutUsageIsBilledItsCeiling(t *testing.T) {
	body := `{"model":"keera-code","max_tokens":1000,"messages":[{"role":"user","content":"hi"}]}`
	for name, opts := range map[string]Options{
		"no usage record": {},
		"too large":       {MaxResponseBytes: 10},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarnessWith(t, jsonBackend(`{"choices":[{"message":{"content":"hi"}}]}`),
				nil, nil, opts)
			onPlatformKey(h, "keera-code")
			h.post(t, "/v1/chat/completions", body)

			ev := h.sink.last(t)
			if len(ev.Bills) != 1 || ev.Bills[0].OutputTokens != 1000 ||
				ev.Bills[0].InputTokens != len(body)/bytesPerToken || !ev.Estimated {
				t.Errorf("bills = %+v, estimated %v; want the body and the ceiling",
					ev.Bills, ev.Estimated)
			}
		})
	}

	// On the organisation's own key, nothing is made up.
	h := newHarness(t, jsonBackend(`{"choices":[{"message":{"content":"hi"}}]}`), nil, nil)
	h.post(t, "/v1/chat/completions", body)
	if ev := h.sink.last(t); ev.OutputTokens != 0 || ev.Estimated {
		t.Errorf("row = %d out, estimated %v; want nothing", ev.OutputTokens, ev.Estimated)
	}
}

// A filter's call on the deployment's key is billed even when its answer
// came without a usage record.
func TestAFiltersCallWithoutUsageIsBilledItsCeiling(t *testing.T) {
	h := filterHarnessAnswering(t, func(string, []string) string { return "" }, nil,
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}],`+
				`"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
		})
	inner := h.src.models["keera-guard"]
	guard := httptest.NewServer(jsonBackend(`{"choices":[{"message":{"content":"[]"}}]}`))
	t.Cleanup(guard.Close)
	inner.Backends = []string{guard.URL + "/v1"}
	h.src.models["keera-guard"] = inner
	onPlatformKey(h, "keera-guard")

	h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"hunter2"}]}`)
	ev := h.sink.last(t)
	if len(ev.Bills) != 1 || ev.Bills[0].Alias != "keera-guard" || ev.Bills[0].OutputTokens == 0 {
		t.Errorf("bills = %+v, want the filter's call billed at its ceiling", ev.Bills)
	}
}
