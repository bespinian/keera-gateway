package gateway

import (
	"bytes"
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// formField is one part of a test form. A file has a filename.
type formField struct {
	name, filename, value string
}

func form(t *testing.T, fields ...formField) (string, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, f := range fields {
		if f.filename == "" {
			_ = w.WriteField(f.name, f.value)
			continue
		}
		part, err := w.CreateFormFile(f.name, f.filename)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(part, f.value)
	}
	_ = w.Close()
	return buf.String(), w.FormDataContentType()
}

func (h *harness) postForm(t *testing.T, body, contentType string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.url("/v1/audio/transcriptions"),
		strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// readForm reads a form the gateway sent on: each field's values, and each
// file's name and content.
func readForm(t *testing.T, contentType string, raw []byte) (map[string][]string, map[string][2]string) {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("content type %q: %v", contentType, err)
	}
	values, files := map[string][]string{}, map[string][2]string{}
	r := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
	for {
		part, err := r.NextPart()
		if err == io.EOF {
			return values, files
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(part)
		if part.FileName() != "" {
			files[part.FormName()] = [2]string{part.FileName(), string(data)}
		} else {
			values[part.FormName()] = append(values[part.FormName()], string(data))
		}
	}
}

// The audio goes on unchanged, with the backend's model name and every other
// field, and the minutes the backend reports are charged.
func TestATranscriptionSendsTheAudioOnAndIsChargedByTheMinute(t *testing.T) {
	type sent struct {
		path, contentType string
		raw               []byte
	}
	got := make(chan sent, 1)
	backend := func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got <- sent{r.URL.Path, r.Header.Get("Content-Type"), raw}
		jsonBackend(`{"text":"Grüezi","usage":{"type":"duration","seconds":61.2}}`)(w, r)
	}
	h := newHarness(t, backend, filterModels(), nil)
	audio := "RIFF\x00\x01\x02binary audio\xff"
	body, ct := form(t,
		formField{name: "model", value: "keera-whisper"},
		formField{name: "language", value: "de"},
		formField{name: "timestamp_granularities[]", value: "word"},
		formField{name: "timestamp_granularities[]", value: "segment"},
		formField{name: "file", filename: "meeting.wav", value: audio},
	)
	resp := h.postForm(t, body, ct)
	answer, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(answer), "Grüezi") {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, answer)
	}
	s := <-got
	if s.path != "/v1/audio/transcriptions" {
		t.Errorf("path = %s", s.path)
	}
	values, files := readForm(t, s.contentType, s.raw)
	if v := values["model"]; len(v) != 1 || v[0] != "whisper-served" {
		t.Errorf("model = %v, want the backend's name", v)
	}
	if v := values["language"]; len(v) != 1 || v[0] != "de" {
		t.Errorf("language = %v", v)
	}
	if v := values["timestamp_granularities[]"]; len(v) != 2 || v[0] != "word" || v[1] != "segment" {
		t.Errorf("a repeated field = %v, want both values in order", v)
	}
	if f := files["file"]; f[0] != "meeting.wav" || f[1] != audio {
		t.Errorf("file = %q, want the audio unchanged", f)
	}
	ev := h.sink.last(t)
	// 61.2 s at 0.006 a minute.
	if ev.AudioMillis != 61_200 || ev.CostMicros != 6_120 {
		t.Errorf("audio = %d ms, cost = %d, want 61200 ms and 6120", ev.AudioMillis, ev.CostMicros)
	}
}

// A text, srt or vtt transcript is the answer itself, not JSON.
func TestATextTranscriptIsPassedOnAsText(t *testing.T) {
	backend := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, `He said "model": "x".`)
	}
	h := newHarness(t, backend, filterModels(), nil)
	body, ct := form(t,
		formField{name: "model", value: "keera-whisper"},
		formField{name: "response_format", value: "text"},
		formField{name: "file", filename: "a.mp3", value: "audio"},
	)
	resp := h.postForm(t, body, ct)
	answer, _ := io.ReadAll(resp.Body)
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("content type = %q, want text/plain", got)
	}
	if string(answer) != `He said "model": "x".` {
		t.Errorf("answer = %q, want the transcript as it came", answer)
	}
}

// What a filter could not read, or the gateway could not bill, is refused
// before anything is sent.
func TestATranscriptionTheGatewayCannotReadIsRefused(t *testing.T) {
	audio := formField{name: "file", filename: "a.wav", value: "audio"}
	model := formField{name: "model", value: "keera-whisper"}
	for _, tc := range []struct {
		name   string
		fields []formField
		want   string
	}{
		{"no audio", []formField{model}, "'file'"},
		{"text in a file", []formField{
			model, audio,
			{name: "prompt", filename: "p.txt", value: "hunter2"},
		}, "only 'file'"},
		{"two files", []formField{model, audio, audio}, "one audio file"},
		{"a stream", []formField{model, audio, {name: "stream", value: "true"}}, "stream"},
		{
			"a misspelled model",
			[]formField{{name: "Model", value: "keera-whisper"}, audio},
			"must be spelled",
		},
	} {
		h := newHarness(t, jsonBackend(`{"text":""}`), filterModels(), nil)
		body, ct := form(t, tc.fields...)
		resp := h.postForm(t, body, ct)
		answer, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(answer), tc.want) {
			t.Errorf("%s: status = %d, body = %s; want 400 saying %s",
				tc.name, resp.StatusCode, answer, tc.want)
		}
	}
	h := newHarness(t, jsonBackend(`{"text":""}`), filterModels(), nil)
	resp := h.post(t, "/v1/audio/transcriptions", `{"model":"keera-whisper"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a JSON body: status = %d, want 400", resp.StatusCode)
	}
}

// A chat model is not served here, and a transcription model only here.
func TestATranscriptionModelIsServedOnlyAtItsEndpoint(t *testing.T) {
	h := newHarness(t, jsonBackend(hiAnswer), filterModels(), nil)
	body, ct := form(t,
		formField{name: "model", value: "keera-code"},
		formField{name: "file", filename: "a.wav", value: "audio"},
	)
	if resp := h.postForm(t, body, ct); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a chat model: status = %d, want 404", resp.StatusCode)
	}
	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-whisper","messages":[{"role":"user","content":"hi"}]}`)
	answer, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(answer), "/v1/audio/transcriptions") {
		t.Errorf("status = %d, body = %s; want a 404 naming /v1/audio/transcriptions",
			resp.StatusCode, answer)
	}
}

// Filters read the prompt and Mistral's context_bias. The audio is not
// text, as in a chat.
func TestFilterRewritesATranscriptionPrompt(t *testing.T) {
	h := filterHarness(t, redactor, nil)
	body, ct := form(t,
		formField{name: "model", value: "keera-whisper"},
		formField{name: "prompt", value: "the password is hunter2"},
		formField{name: "context_bias", value: "Keera"},
		formField{name: "context_bias", value: "hunter2"},
		formField{name: "file", filename: "a.wav", value: "hunter2 audio"},
	)
	if resp := h.postForm(t, body, ct); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	<-h.upstreamBodies
	forwarded := string(<-h.upstreamBodies)
	if strings.Contains(forwarded, "password is hunter2") {
		t.Errorf("the prompt left unfiltered: %s", forwarded)
	}
	if strings.Contains(forwarded, "\r\n\r\nhunter2\r\n") {
		t.Errorf("a context_bias word left unfiltered: %s", forwarded)
	}
	if !strings.Contains(forwarded, "\r\n\r\nKeera\r\n") {
		t.Errorf("the other context_bias word was lost: %s", forwarded)
	}
	if !strings.Contains(forwarded, "hunter2 audio") {
		t.Errorf("the audio was changed: %s", forwarded)
	}
}

// On the deployment's key, an answer must say what it used, or it could not
// be billed.
func TestATranscriptionOnTheDeploymentsKeyMustReportItsUse(t *testing.T) {
	h := newHarness(t, jsonBackend(`{"text":"hi","usage":{"type":"duration","seconds":3}}`),
		filterModels(), nil)
	onPlatformKey(h, "keera-whisper")
	body, ct := form(t,
		formField{name: "model", value: "keera-whisper"},
		formField{name: "response_format", value: "srt"},
		formField{name: "file", filename: "a.wav", value: "audio"},
	)
	resp := h.postForm(t, body, ct)
	answer, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(answer), "verbose_json") {
		t.Errorf("srt: status = %d, body = %s; want 400", resp.StatusCode, answer)
	}

	body, ct = form(t,
		formField{name: "model", value: "keera-whisper"},
		formField{name: "file", filename: "a.wav", value: "audio"},
	)
	if resp := h.postForm(t, body, ct); resp.StatusCode != http.StatusOK {
		t.Fatalf("json: status = %d, want 200", resp.StatusCode)
	}
	ev := h.sink.last(t)
	// 3 s at 0.006 a minute.
	if len(ev.Bills) != 1 || ev.Bills[0].AudioMillis != 3_000 || ev.Bills[0].Micros != 300 {
		t.Errorf("bills = %+v, want 3 s billed 300", ev.Bills)
	}
}

func TestTranscriptionUsageReadsEachBackendsRecord(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		want         *tokenUsage
	}{
		{
			"whisper", `{"text":"a","usage":{"type":"duration","seconds":9}}`,
			&tokenUsage{AudioMillis: 9_000},
		},
		{
			"a part second is billed as a whole one", `{"text":"a","usage":{"type":"duration","seconds":0.0004}}`,
			&tokenUsage{AudioMillis: 1},
		},
		{
			"gpt-4o", `{"text":"a","usage":{"type":"tokens","input_tokens":40,"output_tokens":7}}`,
			&tokenUsage{InputTokens: 40, OutputTokens: 7},
		},
		{
			"mistral", `{"text":"a","usage":{"prompt_audio_seconds":12,"prompt_tokens":5,"completion_tokens":3}}`,
			&tokenUsage{InputTokens: 5, OutputTokens: 3, AudioMillis: 12_000},
		},
		{
			"verbose_json", `{"text":"a","duration":2.5,"segments":[]}`,
			&tokenUsage{AudioMillis: 2_500},
		},
		{"ollama", `{"text":"a"}`, nil},
		{"text", `hello`, nil},
		{"a nonsense length", `{"usage":{"seconds":-5}}`, nil},
	} {
		got := transcriptionUsage([]byte(tc.answer))
		switch {
		case got == nil && tc.want == nil:
		case got == nil || tc.want == nil || *got != *tc.want:
			t.Errorf("%s: usage = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestCheckModelTranscribesASecondOfSilence(t *testing.T) {
	var values map[string][]string
	var files map[string][2]string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		values, files = readForm(t, r.Header.Get("Content-Type"), raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":""}`)
	}))
	defer upstream.Close()
	m := chatAlias(upstream.URL)
	m.Kind = policy.KindTranscription
	m.BackendModel = "whisper"
	p := probeServer(t).CheckModel(context.Background(), m)
	if !p.OK {
		t.Fatalf("probe = %+v, want ok", p)
	}
	if v := values["model"]; len(v) != 1 || v[0] != "whisper" {
		t.Errorf("model = %v", v)
	}
	if f := files["file"]; !strings.HasPrefix(f[1], "RIFF") || len(f[1]) != 44+32_000 {
		t.Errorf("audio = %d bytes, want a second of 16 kHz WAV", len(f[1]))
	}
}
