package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// forwarded is what came back from the inference plane, and what it took to
// get there.
type forwarded struct {
	// resp is the answer that will be served, and model is what produced it.
	// When every destination failed, they are the last one tried: its status
	// is the truest reason available, so no gateway status replaces it.
	resp  *http.Response
	model policy.Model
	// payload is the body as that destination was sent it. Each attempt is
	// encoded for its own destination, because the 'model' field carries the
	// backend's name for the model, not the alias.
	payload []byte
	// err is set when the last destination could not be reached at all, which
	// is the one case with no response to serve.
	err error
	// at is how far down the chain the answer came from. Zero is the
	// destination the router meant to use.
	at int
	// failures is what each destination that did not answer said, in order,
	// for the request's own row.
	failures []string
	// wasted is how long those attempts took: time the client waited for
	// nothing, and on a fallback router what the router cost.
	wasted time.Duration
	// release ends the in-flight count of the destination in model, so the
	// count covers the whole answer. Failed attempts release their own before
	// moving on. It is nil only when no destination was tried.
	release func()
}

// done releases whatever the request still holds. It is safe on a zero value.
func (f forwarded) done() {
	if f.release != nil {
		f.release()
	}
}

// note is what to keep on the usage row about the destinations that did not
// answer, or empty when the first one did.
func (f forwarded) note() string {
	if len(f.failures) == 0 {
		return ""
	}
	return "tried before this one: " + strings.Join(f.failures, "; ")
}

// appendNote joins two sentences about one request, either of which may be
// absent.
func appendNote(text, note string) string {
	switch {
	case note == "":
		return text
	case text == "":
		return note
	default:
		return text + " (" + note + ")"
	}
}

// destinations turns a routing decision into the models to try, in order.
//
// It is one model unless a trying router produced a chain. A model the client
// or a deciding router chose has been decided on, and if it fails, that is
// its failure, not a reason to ask another one.
func (s *Server) destinations(model policy.Model, d routeDecision) []policy.Model {
	if len(d.chain) < 2 {
		return []policy.Model{model}
	}
	out := make([]policy.Model, 0, len(d.chain))
	for _, alias := range d.chain {
		if m, ok := s.serveable(alias); ok {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		// The catalogue changed under the decision. One attempt at what was
		// chosen is better than none.
		return []policy.Model{model}
	}
	return out
}

// outbound is one request as one destination is sent it.
type outbound struct {
	path    string
	payload []byte
	// auth presents the credential, and sets whatever else the destination's
	// API needs besides the body. Nil sends the credential as a bearer token,
	// which is what every OpenAI-shaped endpoint takes.
	auth func(h http.Header, credential string)
}

// forward sends the request to the first destination that can answer it.
//
// A destination fails over only when it could not be reached, did not start
// answering in time, or answered 5xx. A 4xx is not retried: a request that is
// too long or malformed will be so at the next destination too.
//
// Failover stops at the first byte of the answer, since half of one model's
// stream cannot be completed by another. That is why the timeout that matters
// is the one on the response headers.
//
// build encodes the request for one destination, because the 'model' field
// carries the backend's name for the model, not the alias, and because a
// destination may speak the client's own API rather than the OpenAI one.
func (s *Server) forward(ctx context.Context, chain []policy.Model,
	build func(policy.Model) outbound, tr *trace,
) forwarded {
	var f forwarded
	for i, m := range chain {
		out := build(m)
		f.model, f.payload, f.at = m, out.payload, i

		began := time.Now()
		// Two counts over different spans. The metric is this process's own
		// concurrency and ends at the response line. The load tracker's is the
		// destination's and lasts until the last token, which is why its
		// release is returned for answer to defer. See loads.begin.
		s.metrics.InflightAdd(1)
		release := s.load.begin(m.Alias)
		resp, err := s.send(ctx, m, out)
		s.metrics.InflightAdd(-1)
		f.resp, f.err = resp, err
		// One step per destination, including the ones that said no. On a
		// fallback chain they are why the request was slow, and the row only
		// names the model that answered.
		tr.since(store.SpanUpstream, m.Alias, began, attemptNote(resp, err))

		if err != nil || (resp != nil && resp.StatusCode >= 500) {
			// Recorded even on the last attempt, so a measured router ranks it
			// last for a while. Stamped now, not at the start, so a long dial
			// timeout does not shorten the penalty. See loadPenalty.
			s.load.fail(m.Alias, time.Now())
		}
		switch {
		case i == len(chain)-1 || ctx.Err() != nil:
			// Nowhere left to go, or nobody left to answer.
			f.release = release
			return f
		case err != nil:
			release()
			f.failures = append(f.failures, m.Alias+" could not be reached: "+err.Error())
		case resp.StatusCode >= 500:
			release()
			// Counted here because the request may succeed elsewhere, and then
			// nothing else would show this model is down.
			s.metrics.UpstreamError(m.Alias)
			f.failures = append(f.failures,
				m.Alias+" answered "+strconv.Itoa(resp.StatusCode))
			_ = resp.Body.Close()
		default:
			f.release = release
			return f
		}
		f.wasted += time.Since(began)
	}
	return f
}

// dispatch sends an OpenAI-shaped request to one of a model's backends.
func (s *Server) dispatch(ctx context.Context, model policy.Model, path string, payload []byte) (*http.Response, error) {
	return s.send(ctx, model, outbound{path: path, payload: payload})
}

// send sends the request to one of a model's backends, moving to the next one
// only if a backend cannot be reached at all. It never retries a request a
// backend answered: generation is not free, and a 500 from vLLM is an answer.
func (s *Server) send(ctx context.Context, model policy.Model, out outbound) (*http.Response, error) {
	counter, _ := s.rr.LoadOrStore(model.Alias, new(atomic.Uint64))
	offset := counter.(*atomic.Uint64).Add(1) - 1
	// Filters, routers and model checks come through here too, so they get the
	// same fixes as a client's request.
	payload := fitProvider(model, out.path, out.payload)

	var lastErr error
	for i := range model.Backends {
		base := model.Backends[(int(offset)+i)%len(model.Backends)]
		// A bytes.Reader body can be replayed, which lets this loop move to
		// the next backend after a connection failure.
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			strings.TrimRight(base, "/")+out.path, io.NopCloser(bytes.NewReader(payload)))
		if err != nil {
			return nil, err
		}
		req.ContentLength = int64(len(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream, application/json")
		// A model with no credential is sent unauthenticated, and the
		// endpoint's own 401 is the clearest thing to show a developer.
		credential := model.Credential(s.opts.APIKeys)
		switch {
		case out.auth != nil:
			out.auth(req.Header, credential)
		case credential != "":
			req.Header.Set("Authorization", "Bearer "+credential)
		}
		resp, err := s.client.Do(req)
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lastErr = err
		s.metrics.UpstreamError(model.Alias)
	}
	return nil, lastErr
}

// copyResponseHeaders forwards what the client needs and adds what an SSE
// stream needs to survive an ingress.
func (s *Server) copyResponseHeaders(w http.ResponseWriter, resp *http.Response, alias string, streaming bool) {
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", forwardableContentType(ct))
	}
	w.Header().Set("X-Keera-Model", alias)
	if streaming {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		// ingress-nginx buffers responses unless told not to, and the editor
		// then shows nothing until the end.
		w.Header().Set("X-Accel-Buffering", "no")
	}
}

// forwardableContentType limits an upstream media type to the two this
// gateway speaks, keeping the backend's parameters.
//
// The panel, the control API and the inference API share one origin. A backend
// answering text/html would hand a browser a page to render next to the
// panel's session cookie, so anything that is not JSON or an event stream is
// sent as JSON.
func forwardableContentType(ct string) string {
	media := ct
	if i := strings.IndexByte(media, ';'); i >= 0 {
		media = media[:i]
	}
	switch strings.ToLower(strings.TrimSpace(media)) {
	case "application/json", "text/event-stream":
		return ct
	default:
		return "application/json"
	}
}
