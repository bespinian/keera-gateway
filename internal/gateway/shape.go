package gateway

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
)

// surface is one API the gateway serves: the model kind, the upstream path,
// and the format it speaks to the client.
type surface struct {
	kind  policy.Kind
	path  string
	shape shape
	// dialect is set on an API a hosted provider serves as its own, for
	// forwarding it there untranslated. See native.go.
	dialect dialect
}

var (
	chatSurface       = surface{kind: policy.KindChat, path: "/chat/completions", shape: openAIShape{}}
	completionSurface = surface{kind: policy.KindCompletion, path: "/completions", shape: openAIShape{}}
	embeddingSurface  = surface{kind: policy.KindEmbedding, path: "/embeddings", shape: openAIShape{}}
	// messagesSurface is the Anthropic-shaped `/v1/messages`: chatSurface in
	// another format.
	messagesSurface = surface{
		kind: policy.KindChat, path: "/chat/completions",
		shape: anthropicShape{}, dialect: anthropicDialect{},
	}
	// responsesSurface is OpenAI's `/v1/responses`, the same again.
	responsesSurface = surface{
		kind: policy.KindChat, path: "/chat/completions",
		shape: responsesShape{}, dialect: responsesDialect{},
	}
)

// What a shape answers when it cannot translate a backend's success.
const (
	msgUnreadableAnswer = "the inference plane returned a response the gateway could not read"
	msgUnencodable      = "the gateway could not encode the response"
)

// shape is the request and response format one API surface speaks. The
// inference plane and serve speak the OpenAI shape; others are translated at
// the edges, so one set of guardrails covers every client. The exception is a
// request forwarded to the provider whose own API it is in.
type shape interface {
	// decodeBody turns a request in this shape into an OpenAI chat request,
	// and returns the shape to answer with: the Responses API names each call
	// as the request declared its tool. The error is shown to the client.
	decodeBody(raw []byte) (*body, shape, error)
	// encode turns one buffered upstream response into this shape and
	// returns the status to answer with. It may differ from the status in,
	// since a success that cannot be translated becomes a failure.
	encode(raw []byte, alias string, status int) ([]byte, int)
	// pipe forwards a streamed upstream response in this shape, flushing as
	// it goes.
	pipe(dst io.Writer, flush func(), src io.Reader, alias string, dropUsageEvent bool, limit int64) (streamStats, error)
	// writeError renders the gateway's own refusal. typ and code are OpenAI
	// spellings, which a shape may translate.
	writeError(w http.ResponseWriter, status int, typ, code, msg string)
	// contentType is the media type of buffered responses, or "" to keep the
	// plane's.
	contentType() string
}

// openAIShape is the identity, for surfaces that already speak the plane's
// shape.
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

// encode names the alias as the model, never the backend's name.
func (openAIShape) encode(raw []byte, alias string, status int) ([]byte, int) {
	if status >= 300 {
		return openAIErrorBody(raw, status), status
	}
	return renameModel(raw, alias), status
}

// openAIErrorBody puts an upstream error in the OpenAI envelope, since an
// OpenAI SDK reads only error.message. One already in it passes unchanged.
// A context overflow gets OpenAI's code and the gateway's wording, since
// vLLM and llama.cpp do not send the code Codex and the SDKs act on.
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
	return httpx.ErrorBody(openAIErrorType(status), "", upstreamErrorMessage(raw, status))
}

// openAIErrorType types an error by status as the gateway types its own
// refusals, so a client reads both the same way.
func openAIErrorType(status int) string {
	switch {
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 500:
		return "server_error"
	}
	return "invalid_request_error"
}

func (openAIShape) contentType() string { return "" }

func (openAIShape) pipe(dst io.Writer, flush func(), src io.Reader, alias string,
	dropUsageEvent bool, limit int64,
) (streamStats, error) {
	return pipeSSE(dst, flush, src, alias, dropUsageEvent, limit)
}
