package gateway

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
)

// surface is one API the gateway serves: the kind of model it reaches, the
// upstream path it forwards to, and the format it speaks to the client.
type surface struct {
	kind  policy.Kind
	path  string
	shape shape
	// dialect is set on an API a hosted provider also serves as its own, and
	// says how to forward it there untranslated. See native.go.
	dialect dialect
}

var (
	chatSurface       = surface{kind: policy.KindChat, path: "/chat/completions", shape: openAIShape{}}
	completionSurface = surface{kind: policy.KindCompletion, path: "/completions", shape: openAIShape{}}
	embeddingSurface  = surface{kind: policy.KindEmbedding, path: "/embeddings", shape: openAIShape{}}
	// messagesSurface is the Anthropic-shaped `/v1/messages`: the same models
	// and guardrails as chatSurface, in another format.
	messagesSurface = surface{kind: policy.KindChat, path: "/chat/completions",
		shape: anthropicShape{}, dialect: anthropicDialect{}}
	// responsesSurface is OpenAI's `/v1/responses`, the same again.
	responsesSurface = surface{kind: policy.KindChat, path: "/chat/completions",
		shape: responsesShape{}, dialect: responsesDialect{}}
)

// What a shape answers when a backend's success cannot be translated: one it
// cannot read, or one it cannot encode again.
const (
	msgUnreadableAnswer = "the inference plane returned a response the gateway could not read"
	msgUnencodable      = "the gateway could not encode the response"
)

// shape is the request and response format one API surface speaks.
//
// The inference plane and serve speak the OpenAI shape. Other shapes are
// translated at the edges, so one set of guardrails covers every client. The
// one exception is a request forwarded to the provider whose own API it is in.
type shape interface {
	// decodeBody turns a request in this shape into an OpenAI chat request,
	// and returns the shape to answer it with: the Responses API names each
	// call the way the request declared its tool. The error is shown to the
	// client, so it says what is wrong with the body.
	decodeBody(raw []byte) (*body, shape, error)
	// encode turns one buffered upstream response into this shape, and returns
	// the status to answer with. The status in tells an error body from a
	// completion. The status out may differ, since a success that cannot be
	// translated has to become a failure.
	encode(raw []byte, alias string, status int) ([]byte, int)
	// pipe forwards a streamed upstream response in this shape, flushing as it
	// goes, and reports what the stream carried.
	pipe(dst io.Writer, flush func(), src io.Reader, alias string, dropUsageEvent bool, limit int64) (streamStats, error)
	// writeError renders a refusal the gateway generated itself. typ and code
	// are the OpenAI spellings; a shape that names its errors differently
	// translates them.
	writeError(w http.ResponseWriter, status int, typ, code, msg string)
	// contentType is the media type this shape's buffered responses carry, or
	// empty to keep whatever the inference plane sent.
	contentType() string
}

// openAIShape is the identity: the surfaces that already speak what the
// inference plane speaks.
type openAIShape struct{ openAIErrors }

// openAIErrors renders a refusal in the chat shape's envelope. The Responses
// API shares it.
type openAIErrors struct{}

func (openAIErrors) writeError(w http.ResponseWriter, status int, typ, code, msg string) {
	httpx.WriteError(w, status, typ, code, msg)
}

func (sh openAIShape) decodeBody(raw []byte) (*body, shape, error) {
	b, err := parseBody(raw)
	return b, sh, err
}

// encode names the alias as the model: the client was promised the alias,
// never the backend's name for it.
func (openAIShape) encode(raw []byte, alias string, status int) ([]byte, int) {
	if status >= 300 {
		return openAIErrorBody(raw, status), status
	}
	return renameModel(raw, alias), status
}

// openAIErrorBody puts an upstream error in the OpenAI envelope. One already
// in it passes unchanged, type and code included. vLLM's bare message and
// FastAPI's detail are wrapped, since an OpenAI SDK reads only error.message.
//
// A request too long for the backend's context is the exception: vLLM and
// llama.cpp do not give OpenAI's code for it, which is what Codex and the
// OpenAI SDKs act on, so it gets the code and the gateway's own wording.
func openAIErrorBody(raw []byte, status int) []byte {
	var envelope struct {
		Error *struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	decoded := json.Unmarshal(raw, &envelope) == nil && envelope.Error != nil
	if status == http.StatusBadRequest && (!decoded || envelope.Error.Code != "context_length_exceeded") {
		if overflow := contextOverflow(raw); overflow != "" {
			return httpx.ErrorBody("invalid_request_error", "context_length_exceeded", overflow)
		}
	}
	if decoded && envelope.Error.Message != "" {
		return raw
	}
	typ := "invalid_request_error"
	if status >= 500 {
		typ = "server_error"
	}
	return httpx.ErrorBody(typ, "", upstreamErrorMessage(raw, status))
}

func (openAIShape) contentType() string { return "" }

func (openAIShape) pipe(dst io.Writer, flush func(), src io.Reader, alias string,
	dropUsageEvent bool, limit int64,
) (streamStats, error) {
	return pipeSSE(dst, flush, src, alias, dropUsageEvent, limit)
}
