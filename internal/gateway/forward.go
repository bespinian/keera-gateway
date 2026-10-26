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

// forwarded is what came back from the inference plane, and what it took.
type forwarded struct {
	// resp is the answer to serve, and model produced it. If every destination
	// failed, they are the last one tried, whose status is the truest reason.
	resp  *http.Response
	model policy.Model
	// payload is the body as model was sent it, since 'model' carries the
	// backend's name.
	payload []byte
	// err is set when the last destination gave no response to serve.
	err error
	// at is the answering destination's place in the chain; 0 is the first.
	at int
	// failures is what each failed destination said, for the usage row.
	failures []string
	// wasted is how long the failed attempts took.
	wasted time.Duration
	// sent is when the request went to model.
	sent time.Time
	// release ends model's in-flight count, so the count covers the whole
	// answer. It is nil only when no destination was tried.
	release func()
}

// done releases what the request still holds. It is safe on a zero value.
func (f forwarded) done() {
	if f.release != nil {
		f.release()
	}
}

// note is what the usage row says about failed destinations, or "".
func (f forwarded) note() string {
	if len(f.failures) == 0 {
		return ""
	}
	return "tried before this one: " + strings.Join(f.failures, "; ")
}

// appendNote joins two notes, either of which may be empty.
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

// destinations turns a routing decision into the models to try, in order. It
// is one model unless a trying step produced a chain: a model chosen by the
// client or an instruction step does not fail over.
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
		// The models changed under the decision; try what was chosen.
		return []policy.Model{model}
	}
	return out
}

// outbound is one request as one destination is sent it.
type outbound struct {
	path    string
	payload []byte
	// auth sets the credential and any other headers the API needs. Nil sends
	// a bearer token, as OpenAI-shaped endpoints take.
	auth func(h http.Header, credential string)
	// clientID is the client's X-Client-Request-Id, set only on the client's
	// own request, not on a filter's or router's.
	clientID string
}

// forward sends the request to the first destination that can answer it.
//
// It fails over only when a destination could not be reached, did not start
// answering in time, or answered 5xx. A 4xx is not retried, since the next
// destination would say the same.
//
// Failover stops at the first byte of the answer, since another model cannot
// finish a half-sent stream.
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
		// The metric ends at the response line. The load count lasts until the
		// last token, so its release is returned for the caller to defer.
		s.metrics.InflightAdd(1)
		release := s.load.begin(m.Key())
		resp, err := s.send(ctx, m, out)
		s.metrics.InflightAdd(-1)
		f.resp, f.err = resp, err
		// One step per destination, including failed ones, which explain a
		// slow request.
		tr.since(store.SpanUpstream, m.Alias, began, attemptNote(resp, err))

		// A client that hung up is not the destination's fault.
		gone := errors.Is(ctx.Err(), context.Canceled)
		if (err != nil && !gone) || (resp != nil && resp.StatusCode >= 500) {
			// Stamped now, not at the start, so a long dial timeout does not
			// shorten the penalty. See loadPenalty.
			s.load.fail(m.Key(), time.Now())
		}
		switch {
		case i == len(chain)-1 || ctx.Err() != nil:
			f.release = release
			return f
		case err != nil:
			release()
			f.failures = append(f.failures, m.Alias+" could not be reached: "+err.Error())
		case resp.StatusCode >= 500:
			release()
			// Counted here, since a later success would hide this model is down.
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

// send sends the request to one of a model's backends, moving to the next only
// if it cannot connect. It never resends a request a backend may have
// received: generation is not free, and a slow backend may still be working.
func (s *Server) send(ctx context.Context, model policy.Model, out outbound) (*http.Response, error) {
	counter, _ := s.rr.LoadOrStore(model.Key(), new(atomic.Uint64))
	offset := counter.(*atomic.Uint64).Add(1) - 1
	payload := fitProvider(model, out.path, out.payload)

	var lastErr error
	for i := range model.Backends {
		base := model.Backends[(int(offset)+i)%len(model.Backends)]
		// A bare *bytes.Reader gives the request a GetBody, which Go needs to
		// resend it on a stale keep-alive connection or after an HTTP/2 GOAWAY.
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			strings.TrimRight(base, "/")+out.path, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.ContentLength = int64(len(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream, application/json")
		if out.auth != nil {
			out.auth(req.Header, model.APIKey)
		} else {
			setCredential(req.Header, "", model.APIKey)
		}
		// OpenAI support can find a request by the client's id.
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

// setCredential sends credential in the header name, or as a bearer token
// when name is empty.
func setCredential(h http.Header, name, credential string) {
	switch {
	case credential == "":
	case name == "":
		h.Set("Authorization", "Bearer "+credential)
	default:
		h.Set(name, credential)
	}
}

// undelivered reports whether a failed request never reached the backend.
func undelivered(err error) bool {
	op, ok := errors.AsType[*net.OpError](err)
	return ok && (op.Op == "dial" || op.Op == "proxyconnect")
}

// copyResponseHeaders forwards what the client needs, including the retry
// headers for a 429 or 529, and what an SSE stream needs to pass an ingress.
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

// streamHeaders keeps proxies from holding back an event stream.
func streamHeaders(h http.Header) {
	h.Set("Cache-Control", "no-cache")
	// ingress-nginx buffers responses unless told not to.
	h.Set("X-Accel-Buffering", "no")
}

// forwardableContentType sends anything but JSON or an event stream as JSON.
// The panel shares the origin, so a backend's text/html page would render
// next to the panel's session cookie.
func forwardableContentType(ct string) string {
	switch httpx.MediaType(ct) {
	case "application/json", "text/event-stream":
		return ct
	default:
		return "application/json"
	}
}
