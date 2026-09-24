package gateway

import (
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

// shape is the request and response format one API surface speaks.
//
// The inference plane and serve speak the OpenAI shape. Other shapes are
// translated at the edges, so one set of guardrails covers every client. The
// one exception is a request forwarded to the provider whose own API it is in.
type shape interface {
	// decode turns a request in this shape into an OpenAI chat request. The
	// error is shown to the client, so it says what is wrong with the body.
	decode(raw []byte) ([]byte, error)
	// encode turns one buffered upstream response into this shape, and returns
	// the status to answer with. The status in tells an error body from a
	// completion. The status out may differ, since a success that cannot be
	// translated has to become a failure.
	encode(raw []byte, alias string, status int) ([]byte, int)
	// pipe forwards a streamed upstream response in this shape, flushing as it
	// goes, and reports what the stream carried.
	pipe(dst io.Writer, flush func(), src io.Reader, alias string, dropUsageEvent bool) (streamStats, error)
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
type openAIShape struct{}

func (openAIShape) decode(raw []byte) ([]byte, error) { return raw, nil }

func (openAIShape) encode(raw []byte, _ string, status int) ([]byte, int) {
	return raw, status
}

func (openAIShape) contentType() string { return "" }

func (openAIShape) pipe(dst io.Writer, flush func(), src io.Reader, _ string,
	dropUsageEvent bool,
) (streamStats, error) {
	return pipeSSE(dst, flush, src, dropUsageEvent)
}

func (openAIShape) writeError(w http.ResponseWriter, status int, typ, code, msg string) {
	httpx.WriteError(w, status, typ, code, msg)
}
