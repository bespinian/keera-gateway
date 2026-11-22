package gateway

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/textproto"
	"slices"
	"strings"
)

// A transcription is a multipart form, not JSON: the audio and a few text
// fields. The gateway reads the text fields into a body like any other
// request, so the model name, limits and filters work as they do for chat,
// and keeps the audio aside to send on unchanged. See https://docs.keera.ch/use/api/.

// audioField is the one part that may be a file. Any other file part is
// refused: a backend may read it as text, and no filter would have seen it.
const audioField = "file"

// audioPart is the audio of a transcription, as the client sent it.
type audioPart struct {
	filename    string
	contentType string
	data        []byte
}

// parseForm reads a transcription form into a body of its text fields and
// the audio. A field sent more than once becomes an array, so a filter reads
// every copy.
func parseForm(contentType string, raw []byte) (*body, *audioPart, error) {
	media, params, err := mime.ParseMediaType(contentType)
	if err != nil || media != "multipart/form-data" || params["boundary"] == "" {
		return nil, nil, errors.New("a transcription is sent as multipart/form-data, " +
			"with the audio in the 'file' field")
	}
	values := map[string][]string{}
	var order []string
	var audio *audioPart
	r := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
	for {
		part, err := r.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("the form could not be read: %w", err)
		}
		name := part.FormName()
		switch {
		case name == "":
			return nil, nil, errors.New("every part of the form needs a name")
		case name == audioField && audio != nil:
			return nil, nil, errors.New("send one audio file in the 'file' field")
		case name != audioField && part.FileName() != "":
			return nil, nil, fmt.Errorf("the field '%s' is a file; only 'file' may be one", name)
		}
		if name == audioField {
			// The audio is most of the form, so room for all of it is made once.
			data, err := readAll(part, len(raw))
			if err != nil {
				return nil, nil, fmt.Errorf("the form could not be read: %w", err)
			}
			audio = &audioPart{
				filename:    part.FileName(),
				contentType: part.Header.Get("Content-Type"),
				data:        data,
			}
			continue
		}
		data, err := io.ReadAll(part)
		if err != nil {
			return nil, nil, fmt.Errorf("the form could not be read: %w", err)
		}
		if _, seen := values[name]; !seen {
			order = append(order, name)
		}
		values[name] = append(values[name], string(data))
	}
	if audio == nil || len(audio.data) == 0 {
		return nil, nil, errors.New("the 'file' field with the audio is required")
	}
	b := &body{fields: make(map[string]json.RawMessage, len(order))}
	for _, name := range order {
		var raw []byte
		switch v := values[name]; {
		case name == "stream":
			// A streamed transcription's events carry no usage record the
			// gateway reads, so it could not be billed. false is kept as a
			// boolean, as in a JSON body.
			if !slices.Equal(v, []string{"false"}) {
				return nil, nil, errors.New("the gateway does not stream transcriptions; leave out 'stream'")
			}
			raw = []byte("false")
		case len(v) > 1:
			raw, _ = json.Marshal(v)
		default:
			raw, _ = json.Marshal(v[0])
		}
		b.set(name, raw)
	}
	if err := checkKeys(b.fields); err != nil {
		return nil, nil, err
	}
	return b, audio, nil
}

// formPayload writes the body's fields and the audio back as a form, for one
// backend. It returns the payload and its content type. Writing to memory
// cannot fail, so the writer's errors are not checked.
func formPayload(b *body, audio *audioPart) ([]byte, string) {
	var buf bytes.Buffer
	buf.Grow(len(audio.data) + b.size() + 1024)
	w := multipart.NewWriter(&buf)
	for _, name := range b.order {
		for _, v := range formValues(b.fields[name]) {
			_ = w.WriteField(name, v)
		}
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`,
		audioField, formFilename(audio.filename)))
	h.Set("Content-Type", cmp.Or(audio.contentType, "application/octet-stream"))
	part, _ := w.CreatePart(h)
	_, _ = part.Write(audio.data)
	_ = w.Close()
	return buf.Bytes(), w.FormDataContentType()
}

// formValues is a field as form values: each element of an array, or the
// value itself. A string is sent unquoted, anything else as its JSON text.
func formValues(raw json.RawMessage) []string {
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil {
		list = []json.RawMessage{raw}
	}
	out := make([]string, 0, len(list))
	for _, v := range list {
		var s string
		if json.Unmarshal(v, &s) == nil {
			out = append(out, s)
		} else {
			out = append(out, string(v))
		}
	}
	return out
}

// formFilename keeps the client's file name, which backends read the audio
// format from, without what would break the header.
func formFilename(name string) string {
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' {
			return -1
		}
		return r
	}, name)
	if name == "" {
		return "audio"
	}
	return name
}

// transcriptionUsage reads what a transcription used. Whisper and vLLM
// report the audio's length, the GPT-4o transcribers tokens, Mistral both,
// and verbose_json carries the length too. A text, srt or vtt answer says
// nothing.
func transcriptionUsage(raw []byte) *tokenUsage {
	var answer struct {
		Duration *float64 `json:"duration"`
		Usage    *struct {
			Type    string   `json:"type"`
			Seconds *float64 `json:"seconds"`
			// Mistral's name for the length.
			AudioSeconds *float64 `json:"prompt_audio_seconds"`
			Input        int      `json:"input_tokens"`
			Output       int      `json:"output_tokens"`
			Prompt       int      `json:"prompt_tokens"`
			Completion   int      `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &answer) != nil {
		return nil
	}
	var u tokenUsage
	seconds := answer.Duration
	if usage := answer.Usage; usage != nil {
		u.InputTokens = max(usage.Input, usage.Prompt)
		u.OutputTokens = max(usage.Output, usage.Completion)
		seconds = cmp.Or(usage.Seconds, usage.AudioSeconds, seconds)
	}
	if seconds != nil && *seconds > 0 && !math.IsInf(*seconds, 0) {
		// Rounded up, as providers bill a started second.
		u.AudioMillis = int64(math.Ceil(min(*seconds, maxAudioSeconds) * 1000))
	}
	if u == (tokenUsage{}) {
		return nil
	}
	return &u
}

// maxAudioSeconds caps a reported length, so a nonsense one cannot overflow
// a cost. It is a week.
const maxAudioSeconds = 7 * 24 * 3600

// leastAudioBytesPerSecond is lower than any speech codec a transcriber
// takes: Opus at 6 kbit/s. The credit hold reads a file as this slow, so it
// errs high.
const leastAudioBytesPerSecond = 750

// mostAudioMillis is the longest audio a file of this size can hold.
func mostAudioMillis(bytes int) int64 {
	return int64(bytes) * 1000 / leastAudioBytesPerSecond
}

// audioTokensAtMost is the most tokens a model priced by token can count
// for this much audio. The GPT-4o transcribers count about ten a second;
// five times that leaves room for a model that counts more.
func audioTokensAtMost(millis int64) int {
	const perSecond = 50
	return int(millis * perSecond / 1000)
}

// transcriptBytes is how much text a transcription answer holds, for a model
// billed on the gateway's own count.
func transcriptBytes(raw []byte) int {
	var answer struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &answer) == nil {
		return len(answer.Text)
	}
	// text, srt and vtt are the transcript itself.
	return len(raw)
}

// reportingFormats are the answers that say how much audio was heard. A
// model on the deployment's key must be asked for one, or it could not be
// billed.
var reportingFormats = []string{"json", "verbose_json"}

// unreportedFormat says why a transcription on the deployment's key is
// refused, or "" when its answer will say what it used.
func unreportedFormat(b *body) string {
	format, ok := b.str("response_format")
	if !ok || slices.Contains(reportingFormats, format) {
		return ""
	}
	return "on this deployment's key, a transcription is billed by what its answer " +
		"reports, and a '" + format + "' answer reports nothing; ask for json or " +
		"verbose_json"
}

// transcriptMediaTypes are the text answers a transcription may give. They
// are passed on as they are: none renders as a page.
var transcriptMediaTypes = []string{"text/plain", "text/vtt", "application/x-subrip"}

// transcriptionShape is the OpenAI shape, except that a text answer is not
// JSON and keeps the model name it never had.
type transcriptionShape struct{ openAIShape }

func (sh transcriptionShape) encode(raw []byte, alias string, status int) ([]byte, int) {
	if status < 300 && !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return raw, status
	}
	return sh.openAIShape.encode(raw, alias, status)
}
