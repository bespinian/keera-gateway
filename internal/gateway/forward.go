package gateway

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bespinian/keera-gateway/internal/httpx"
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
	// err is set when the last destination could not be reached or did not
	// start answering in time, which is the one case with no response to serve.
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
	// sent is when the request went to model, the destination in resp.
	sent time.Time
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
func (s *Server) destinations(orgID string, model policy.Model, d routeDecision) []policy.Model {
	if len(d.chain) < 2 {
		return []policy.Model{model}
	}
	out := make([]policy.Model, 0, len(d.chain))
	for _, alias := range d.chain {
		if m, ok := s.serveable(orgID, alias); ok {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		// The organisation's models changed under the decision. One attempt at what was
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
	// clientID is the client's X-Client-Request-Id. Only the client's own
	// request carries it: one a filter or router sends is another request.
	clientID string
}

// forward sends the request to the first destination that can answer it.
//
// A destination fails over only when it could not be reached, did not start
// answering in time, or answered 5xx. A 4xx is not retried: a request too long
// or malformed for one destination is so for the next.
//
// Failover stops at the first byte of the answer, since half of one model's
// stream cannot be finished by another. That is why the timeout that matters
// is the one on the response headers.
//
// build encodes the request for each destination, since 'model' carries the
// backend's name and a destination may speak the client's own API.
func (s *Server) forward(ctx context.Context, chain []policy.Model,
	build func(policy.Model) outbound, tr *trace,
) forwarded {
	var f forwarded
	for i, m := range chain {
		out := build(m)
		f.model, f.payload, f.at = m, out.payload, i

		began := time.Now()
		f.sent = began
		// Two counts over different spans. The metric is this process's own
		// concurrency and ends at the response line. The load tracker's is the
		// destination's and lasts until the last token, which is why its
		// release is returned for answer to defer. See loads.begin.
		s.metrics.InflightAdd(1)
		release := s.load.begin(m.Key())
		resp, err := s.send(ctx, m, out)
		s.metrics.InflightAdd(-1)
		f.resp, f.err = resp, err
		// One step per destination, including the ones that said no. On a
		// fallback chain they are why the request was slow, and the row only
		// names the model that answered.
		tr.since(store.SpanUpstream, m.Alias, began, attemptNote(resp, err))

		// A client that hung up is not the destination's fault.
		gone := errors.Is(ctx.Err(), context.Canceled)
		if (err != nil && !gone) || (resp != nil && resp.StatusCode >= 500) {
			// Recorded even on the last attempt, so a measured router ranks it
			// last for a while. Stamped now, not at the start, so a long dial
			// timeout does not shorten the penalty. See loadPenalty.
			s.load.fail(m.Key(), time.Now())
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
			s.metrics.UpstreamError(m.Alias, m.OrgID)
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

// send sends the request to one of a model's backends, moving to the next one
// only if a backend cannot be connected to. It never resends a request a
// backend may have received: generation is not free, a 500 from vLLM is an
// answer, and a backend that did not answer in time may still be working on it.
//
// Each failure is counted here once, whoever sent the request: a client, a
// filter, a router or a model check.
func (s *Server) send(ctx context.Context, model policy.Model, out outbound) (*http.Response, error) {
	counter, _ := s.rr.LoadOrStore(model.Key(), new(atomic.Uint64))
	offset := counter.(*atomic.Uint64).Add(1) - 1
	// Filters, routers and model checks come through here too, so they get the
	// same fixes as a client's request.
	payload := fitProvider(model, out.path, out.payload)

	var lastErr error
	for i := range model.Backends {
		base := model.Backends[(int(offset)+i)%len(model.Backends)]
		// A fresh reader per attempt lets this loop move to the next backend
		// after a connection failure. A bare *bytes.Reader also gives the
		// request a GetBody, without which Go cannot resend it on a stale
		// keep-alive connection or after an HTTP/2 GOAWAY.
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			strings.TrimRight(base, "/")+out.path, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.ContentLength = int64(len(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream, application/json")
		// A model with no credential is sent unauthenticated, and the
		// endpoint's own 401 is the clearest thing to show a developer.
		if out.auth != nil {
			out.auth(req.Header, model.APIKey)
		} else {
			setCredential(req.Header, "", model.APIKey)
		}
		// OpenAI records the client's id, so its support can find a request
		// whose answer never arrived. Other providers have no such header.
		if out.clientID != "" && model.Provider == "openai" {
			req.Header.Set(httpx.ClientRequestIDHeader, out.clientID)
		}
		resp, err := s.modelClient(model).Do(req)
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		s.metrics.UpstreamError(model.Alias, model.OrgID)
		if !undelivered(err) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

// setCredential sends credential in the header named name, or as a bearer
// token when name is empty. An empty credential sends nothing.
func setCredential(h http.Header, name, credential string) {
	switch {
	case credential == "":
	case name == "":
		h.Set("Authorization", "Bearer "+credential)
	default:
		h.Set(name, credential)
	}
}

// undelivered reports whether a failed request never reached the backend,
// because no connection to it was made.
func undelivered(err error) bool {
	op, ok := errors.AsType[*net.OpError](err)
	return ok && (op.Op == "dial" || op.Op == "proxyconnect")
}

// copyResponseHeaders forwards what the client needs and adds what an SSE
// stream needs to survive an ingress.
//
// The retry headers are passed from every upstream. Without them, a client
// told 429 or 529 can only guess when to try again.
func copyResponseHeaders(w http.ResponseWriter, resp *http.Response, alias string, streaming bool) {
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", forwardableContentType(ct))
	}
	for _, name := range []string{"Retry-After", "X-Should-Retry"} {
		if v := resp.Header.Values(name); len(v) > 0 {
			w.Header()[name] = v
		}
	}
	w.Header().Set("X-Keera-Model", alias)
	if streaming {
		streamHeaders(w.Header())
		w.Header().Set("Connection", "keep-alive")
	}
}

// streamHeaders keeps an event stream from being held back on its way to the
// client.
func streamHeaders(h http.Header) {
	h.Set("Cache-Control", "no-cache")
	// ingress-nginx buffers responses unless told not to, and the editor then
	// shows nothing until the end.
	h.Set("X-Accel-Buffering", "no")
}

// forwardableContentType limits an upstream media type to the two this
// gateway speaks, keeping the backend's parameters.
//
// The panel, the control API and the inference API share one origin. A backend
// answering text/html would hand a browser a page to render next to the
// panel's session cookie, so anything that is not JSON or an event stream is
// sent as JSON.
func forwardableContentType(ct string) string {
	switch httpx.MediaType(ct) {
	case "application/json", "text/event-stream":
		return ct
	default:
		return "application/json"
	}
}
