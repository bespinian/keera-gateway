package gateway

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// integer reads a field as a JSON integer. Only tests need it: the gateway
// reads ceilings with body.ceiling, which accepts every spelling.
func (b *body) integer(key string) (int, bool) {
	raw, ok := b.value(key)
	if !ok {
		return 0, false
	}
	var v int
	if json.Unmarshal(raw, &v) != nil {
		return 0, false
	}
	return v, true
}

func TestParseBodyPreservesEverythingItDoesNotTouch(t *testing.T) {
	// The gateway rewrites "model" and leaves the rest alone. Anything it
	// dropped or reordered here would be a field silently taken away from a
	// client - including provider extensions the gateway has never heard of.
	const in = `{"model":"keera-code","messages":[{"role":"user","content":"hi"}],` +
		`"temperature":0,"tools":[{"type":"function"}],"chat_template_kwargs":{"enable_thinking":false}}`

	b, err := parseBody([]byte(in))
	if err != nil {
		t.Fatalf("parseBody: %v", err)
	}
	b.setString("model", "Qwen/Qwen2.5-Coder-32B-Instruct")

	got := string(b.encode())
	want := `{"model":"Qwen/Qwen2.5-Coder-32B-Instruct","messages":[{"role":"user","content":"hi"}],` +
		`"temperature":0,"tools":[{"type":"function"}],"chat_template_kwargs":{"enable_thinking":false}}`
	if got != want {
		t.Errorf("encode()\n got: %s\nwant: %s", got, want)
	}
}

func TestParseBodyRejectsWhatIsNotARequest(t *testing.T) {
	for _, in := range []string{``, `   `, `[1,2,3]`, `"a string"`, `{"model":`} {
		if _, err := parseBody([]byte(in)); err == nil {
			t.Errorf("parseBody(%q) accepted a body that is not a JSON object", in)
		}
	}
}

func TestBodyAccessors(t *testing.T) {
	b, err := parseBody([]byte(`{"model":"m","stream":true,"max_tokens":128,"n":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := b.str("model"); !ok || v != "m" {
		t.Errorf("str(model) = %q, %v", v, ok)
	}
	if v, ok := b.boolean("stream"); !ok || !v {
		t.Errorf("boolean(stream) = %v, %v", v, ok)
	}
	if v, ok := b.integer("max_tokens"); !ok || v != 128 {
		t.Errorf("integer(max_tokens) = %d, %v", v, ok)
	}
	if _, ok := b.str("absent"); ok {
		t.Error("str of a missing field must report absent")
	}
	if _, ok := b.integer("n"); ok {
		t.Error("a null must not read as an integer")
	}
	if _, ok := b.integer("model"); ok {
		t.Error("a string must not read as an integer")
	}
}

func TestEnsureUsageInStream(t *testing.T) {
	tests := []struct {
		name         string
		in           string
		wantInjected bool
	}{
		{
			name:         "a client that sent no stream_options gets the flag added",
			in:           `{"model":"m","stream":true}`,
			wantInjected: true,
		},
		{
			name:         "a client that already asked keeps its own chunk",
			in:           `{"model":"m","stream":true,"stream_options":{"include_usage":true}}`,
			wantInjected: false,
		},
		{
			name:         "a client that asked not to is overridden, and the chunk is ours",
			in:           `{"model":"m","stream":true,"stream_options":{"include_usage":false}}`,
			wantInjected: true,
		},
		{
			name:         "other stream_options survive",
			in:           `{"model":"m","stream":true,"stream_options":{"continuous_usage_stats":true}}`,
			wantInjected: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := parseBody([]byte(tc.in))
			if err != nil {
				t.Fatal(err)
			}
			if got := b.ensureUsageInStream(); got != tc.wantInjected {
				t.Fatalf("ensureUsageInStream() = %v, want %v", got, tc.wantInjected)
			}
			var out struct {
				StreamOptions map[string]any `json:"stream_options"`
			}
			if err := json.Unmarshal(b.encode(), &out); err != nil {
				t.Fatalf("re-encoded body is not valid JSON: %v", err)
			}
			if out.StreamOptions["include_usage"] != true {
				t.Errorf("include_usage = %v, want true - without it the upstream reports no tokens",
					out.StreamOptions["include_usage"])
			}
		})
	}
}

func TestEnsureUsageInStreamKeepsUnrelatedOptions(t *testing.T) {
	b, _ := parseBody([]byte(`{"stream_options":{"continuous_usage_stats":true}}`))
	b.ensureUsageInStream()
	if !strings.Contains(string(b.encode()), "continuous_usage_stats") {
		t.Error("setting include_usage must not discard the client's other stream options")
	}
}

func TestClampOutputTokens(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		limit int
		want  int
		field string
	}{
		{"an oversized request is held to the ceiling", `{"max_tokens":100000}`, 4096, 4096, "max_tokens"},
		{"a request under the ceiling is untouched", `{"max_tokens":512}`, 4096, 512, "max_tokens"},
		{"a request with no ceiling of its own gets one", `{"model":"m"}`, 4096, 4096, "max_tokens"},
		{"the newer field name is clamped too", `{"max_completion_tokens":99999}`, 2048, 2048, "max_completion_tokens"},
		// encoding/json reads a number into a Go int only from an integer
		// literal, so every spelling below used to read as "no ceiling
		// stated" and be forwarded to the upstream unbounded.
		{"a ceiling written with an exponent", `{"max_completion_tokens":1e9}`, 2048, 2048, "max_completion_tokens"},
		{"a ceiling written as a float", `{"max_tokens":100000.0}`, 4096, 4096, "max_tokens"},
		{"a ceiling written as a string", `{"max_tokens":"999999"}`, 4096, 4096, "max_tokens"},
		{"a ceiling that is not a number at all", `{"max_tokens":"lots"}`, 4096, 4096, "max_tokens"},
		{"a ceiling of true", `{"max_completion_tokens":true}`, 2048, 2048, "max_completion_tokens"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := parseBody([]byte(tc.in))
			if err != nil {
				t.Fatal(err)
			}
			clampOutputTokens(b, tc.limit)
			got, ok := b.integer(tc.field)
			if !ok {
				t.Fatalf("%s is missing from %s", tc.field, b.encode())
			}
			if got != tc.want {
				t.Errorf("%s = %d, want %d", tc.field, got, tc.want)
			}
		})
	}
}

func TestClampLeavesNoUnboundedCeilingInTheBody(t *testing.T) {
	// The bug this guards: max_completion_tokens spelled with an exponent was
	// unreadable, so the request counted as having named no ceiling at all.
	// The guardrail's went on as max_tokens and the client's own number was
	// forwarded beside it, leaving the upstream to decide which of the two it
	// honoured - and so whether the administrator's limit held.
	b, err := parseBody([]byte(`{"max_tokens":100,"max_completion_tokens":1e9}`))
	if err != nil {
		t.Fatal(err)
	}
	clampOutputTokens(b, 4096)
	for _, field := range []string{"max_tokens", "max_completion_tokens"} {
		v, ok := b.ceiling(field)
		if !ok {
			t.Fatalf("%s cannot be read back from %s", field, b.encode())
		}
		if v > 4096 {
			t.Errorf("%s = %v, past the 4096 ceiling: %s", field, v, b.encode())
		}
	}
}

func TestClampKeepsAnInLimitCeilingHowTheClientWroteIt(t *testing.T) {
	// Reading a ceiling tolerantly must not mean rewriting one that is already
	// within the limit: the request is the client's, and 512.0 asks for
	// nothing 512 does not.
	b, _ := parseBody([]byte(`{"max_tokens":512.0}`))
	clampOutputTokens(b, 4096)
	if !strings.Contains(string(b.encode()), "512.0") {
		t.Errorf("an in-limit ceiling was rewritten: %s", b.encode())
	}
}

func TestClampDoesNotAddASecondCeilingFieldForAnySpelling(t *testing.T) {
	// The same conflict TestClampDoesNotAddASecondCeilingField guards, for the
	// spellings that used to read as no ceiling at all and so earned the
	// request a max_tokens it had not asked for.
	for _, in := range []string{
		`{"max_completion_tokens":1e9}`, `{"max_completion_tokens":100.0}`,
		`{"max_completion_tokens":"999999"}`, `{"max_completion_tokens":"lots"}`,
	} {
		b, err := parseBody([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		clampOutputTokens(b, 4096)
		if _, ok := b.value("max_tokens"); ok {
			t.Errorf("clamping %s added max_tokens alongside max_completion_tokens: %s",
				in, b.encode())
		}
	}
}

func TestClampDoesNotAddASecondCeilingField(t *testing.T) {
	// A request that already uses max_completion_tokens must not also come out
	// with max_tokens, which some servers treat as a conflict.
	b, _ := parseBody([]byte(`{"max_completion_tokens":100}`))
	clampOutputTokens(b, 4096)
	if _, ok := b.integer("max_tokens"); ok {
		t.Errorf("clamping added max_tokens alongside max_completion_tokens: %s", b.encode())
	}
}

func TestPrependSystem(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{
			name: "a conversation keeps its order behind the new head",
			in:   `{"messages":[{"role":"user","content":"hi"}]}`,
			want: `{"messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"hi"}]}`,
			ok:   true,
		},
		{
			name: "an empty array gains no stray comma",
			in:   `{"messages":[]}`,
			want: `{"messages":[{"role":"system","content":"Be brief."}]}`,
			ok:   true,
		},
		{
			name: "whitespace inside the array is not a message",
			in:   `{"messages":[  ]}`,
			want: `{"messages":[{"role":"system","content":"Be brief."}]}`,
			ok:   true,
		},
		{
			name: "the client's own system message is kept and follows",
			in: `{"messages":[{"role":"system","content":"You are a poet."},` +
				`{"role":"user","content":"hi"}]}`,
			want: `{"messages":[{"role":"system","content":"Be brief."},` +
				`{"role":"system","content":"You are a poet."},{"role":"user","content":"hi"}]}`,
			ok: true,
		},
		{name: "no messages field at all", in: `{"model":"keera-code"}`, want: `{"model":"keera-code"}`},
		{name: "an explicit null", in: `{"messages":null}`, want: `{"messages":null}`},
		{name: "a messages field that is not an array", in: `{"messages":"hi"}`, want: `{"messages":"hi"}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := parseBody([]byte(tc.in))
			if err != nil {
				t.Fatalf("parseBody: %v", err)
			}
			if got := b.prependSystem("Be brief."); got != tc.ok {
				t.Errorf("prependSystem() = %v, want %v", got, tc.ok)
			}
			if got := string(b.encode()); got != tc.want {
				t.Errorf("encode()\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

func TestPrependSystemDoesNotDecodeTheConversation(t *testing.T) {
	// The array is spliced, not parsed, so whatever a client put in a message
	// arrives upstream byte for byte - including fields this gateway has never
	// heard of and any exact number formatting the client chose.
	const in = `{"messages":[{"role":"user","content":"hi","cache_control":{"type":"ephemeral"},` +
		`"weight":1.50}],"model":"keera-code"}`
	b, err := parseBody([]byte(in))
	if err != nil {
		t.Fatalf("parseBody: %v", err)
	}
	b.prependSystem("Be brief.")

	got := string(b.encode())
	if !strings.Contains(got, `"cache_control":{"type":"ephemeral"},"weight":1.50`) {
		t.Errorf("the conversation was rewritten on the way through: %s", got)
	}
	if !strings.HasPrefix(got, `{"messages":[{"role":"system","content":"Be brief."},`) {
		t.Errorf("the system message is not first: %s", got)
	}
	var check map[string]json.RawMessage
	if err := json.Unmarshal([]byte(got), &check); err != nil {
		t.Fatalf("the spliced body is not valid JSON: %v", err)
	}
}

func TestParseBodyFindsFieldBoundariesInAwkwardJSON(t *testing.T) {
	// The top level is walked rather than decoded, so the punctuation that
	// ends a field is found by stepping over strings and counting brackets.
	// Every case here is a body where doing that naively goes wrong: a brace
	// or a comma inside a string, an escaped quote, a trailing backslash
	// before one, an empty container, a number that is not an integer.
	for _, in := range []string{
		`{"model":"m","messages":[{"role":"user","content":"}]},\"still text\""}]}`,
		`{"model":"m","stop":["}","]",","],"n":1}`,
		`{"model":"m","content":"a backslash \\ then a quote \" then a brace {"}`,
		`{"model":"m","tools":[],"tool_choice":{},"logit_bias":{"1":-0.5e2}}`,
		`{ "model" : "m" , "messages" : [ ] , "stream" : true }`,
		`{"model":"m","nested":{"a":{"b":[{"c":"}"}]}}}`,
		`{"model":"m","temperature":1e-3,"seed":-0,"stream":false,"user":null}`,
	} {
		b, err := parseBody([]byte(in))
		if err != nil {
			t.Errorf("parseBody(%s): %v", in, err)
			continue
		}
		// Re-encoding without touching anything has to give back a document
		// that says exactly what the client's did. It is not byte-for-byte -
		// encode does not reproduce the whitespace or the key escapes a client
		// chose - so the two are compared as JSON.
		var want, got any
		if err := json.Unmarshal([]byte(in), &want); err != nil {
			t.Fatalf("the test case is not valid JSON: %v", err)
		}
		out := b.encode()
		if err := json.Unmarshal(out, &got); err != nil {
			t.Errorf("encode(%s) is not valid JSON: %v\n%s", in, err, out)
			continue
		}
		if !reflect.DeepEqual(want, got) {
			t.Errorf("parse and encode changed the request\n  in: %s\n out: %s", in, out)
		}
	}
}

func TestParseBodyKeepsTheLastOfARepeatedField(t *testing.T) {
	// Which value a repeated key takes is what every JSON reader downstream
	// will decide too, and a body that arrived with one field is forwarded
	// with one rather than with the duplicate copied through.
	b, err := parseBody([]byte(`{"model":"first","stream":true,"model":"second"}`))
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := b.str("model"); v != "second" {
		t.Errorf("str(model) = %q, want the last value", v)
	}
	if got, want := string(b.encode()), `{"model":"second","stream":true}`; got != want {
		t.Errorf("encode() = %s, want %s", got, want)
	}
}

func TestParseBodyRefusesABodyItCannotRead(t *testing.T) {
	// A body the gateway cannot parse is refused here, with a message naming
	// where it went wrong, rather than forwarded for the inference plane to
	// reject in words about a request the client did not send.
	for _, in := range []string{
		`{"model":"m",}`,
		`{"model":"m"`,
		`{"model":}`,
		`{"model" "m"}`,
		`{model:"m"}`,
		`{"model":"m"}{"model":"m"}`,
		`{"model":"unterminated}`,
		`{"n":01}`,
		"{\"model\":\"m\",\"messages\":[{\"role\":\"user\",\"content\":\"a\tliteral tab\"}]}",
	} {
		if _, err := parseBody([]byte(in)); err == nil {
			t.Errorf("parseBody(%s) accepted a body that is not valid JSON", in)
		}
	}
}

// FuzzParseBody holds the walk to what unmarshalling into a map does. The walk
// is the gateway's own JSON reader, on the one path every request takes, so the
// question it has to keep answering is not whether it is plausible but whether
// it still agrees with the standard library about every body it accepts.
func FuzzParseBody(f *testing.F) {
	f.Add(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	f.Add(`{"a":"}\"","b":[1,{"c":null}],"d":{},"e":-1.5e+3}`)
	f.Add(`{ "a" : 1 , "a" : 2 }`)
	f.Add(`{}`)
	f.Add(`[]`)
	f.Add(`{"é":"😀"}`)

	f.Fuzz(func(t *testing.T, in string) {
		b, err := parseBody([]byte(in))

		var reference map[string]json.RawMessage
		refErr := json.Unmarshal([]byte(in), &reference)
		if refErr != nil || reference == nil {
			if err == nil {
				t.Fatalf("accepted %q, which is not a JSON object", in)
			}
			return
		}
		if err != nil {
			t.Fatalf("rejected %q, which unmarshals into a map: %v", in, err)
		}

		checkFields(t, in, b, reference)
		checkOrder(t, in, b)
		// And what comes out is a document that still parses.
		var check map[string]json.RawMessage
		if err := json.Unmarshal(b.encode(), &check); err != nil {
			t.Fatalf("%q: encode() is not valid JSON: %v\n%s", in, err, b.encode())
		}
	})
}

// checkFields holds every field the walk read to what unmarshalling read.
func checkFields(t *testing.T, in string, b *body, reference map[string]json.RawMessage) {
	t.Helper()
	if len(b.fields) != len(reference) {
		t.Fatalf("%q: read %d fields, want %d", in, len(b.fields), len(reference))
	}
	for k, want := range reference {
		got, ok := b.fields[k]
		if !ok {
			t.Fatalf("%q: field %q was not read", in, k)
		}
		// The walk keeps whitespace that unmarshalling drops, so the two are
		// compared as values.
		var wantVal, gotVal any
		if err := json.Unmarshal(want, &wantVal); err != nil {
			t.Fatalf("%q: reference value for %q is not valid JSON: %v", in, k, err)
		}
		if err := json.Unmarshal(got, &gotVal); err != nil {
			t.Fatalf("%q: field %q was read as invalid JSON %q", in, k, got)
		}
		if !reflect.DeepEqual(wantVal, gotVal) {
			t.Fatalf("%q: field %q read as %s, want %s", in, k, got, want)
		}
	}
}

// checkOrder holds the order to every field, once each.
func checkOrder(t *testing.T, in string, b *body) {
	t.Helper()
	if len(b.order) != len(b.fields) {
		t.Fatalf("%q: %d fields in the order, %d in the body", in, len(b.order), len(b.fields))
	}
	seen := make(map[string]bool, len(b.order))
	for _, k := range b.order {
		if seen[k] {
			t.Fatalf("%q: field %q is in the order twice", in, k)
		}
		seen[k] = true
		if _, ok := b.fields[k]; !ok {
			t.Fatalf("%q: the order names %q, which is not a field", in, k)
		}
	}
}
