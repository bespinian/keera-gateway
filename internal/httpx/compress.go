package httpx

import (
	"compress/gzip"
	"net/http"
	"strings"
	"sync"
)

// Compression for the control plane. The panel's assets and reports shrink to
// about a third, and there is often no proxy in front to do it. The inference
// plane is left alone: buffering a streamed completion would delay it.

// compressMinBytes is the smallest response worth compressing. Below it the
// gzip framing is a large part of what is sent.
const compressMinBytes = 1 << 10

// gzipPool reuses compressors: each carries about 300KB of state.
var gzipPool = sync.Pool{
	New: func() any {
		// Not BestCompression, as the embedded assets get: per request, the
		// last few percent cost more CPU than they save.
		zw, _ := gzip.NewWriterLevel(nil, gzip.DefaultCompression)
		return zw
	},
}

// AcceptsGzip reports whether an Accept-Encoding header asks for gzip. A
// substring test is enough: there is no other encoding on offer, and a client
// that cannot read gzip does not name it.
func AcceptsGzip(header string) bool {
	return strings.Contains(header, "gzip")
}

// compressibleType reports whether a Content-Type is worth gzipping.
// text/event-stream is excluded: a compressor's buffer would hold back the live
// request log.
func compressibleType(ct string) bool {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	switch ct = strings.TrimSpace(ct); ct {
	case "application/json", "image/svg+xml":
		return true
	case "text/event-stream":
		return false
	}
	return strings.HasPrefix(ct, "text/")
}

// Compress gzips the responses of next for clients that asked for it.
//
// The choice waits for the first write, when the content type and body size
// are known. Until then the body is held, so a small response can still go out
// uncompressed. A response that already has a Content-Encoding, like the
// panel's precompressed assets, is left alone.
func Compress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set even when not compressing: the resource varies, so a cache must
		// not serve the plain answer to everyone.
		w.Header().Set("Vary", "Accept-Encoding")
		if !AcceptsGzip(r.Header.Get("Accept-Encoding")) {
			next.ServeHTTP(w, r)
			return
		}
		cw := &compressWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(cw, r)
		// Not deferred: after a panic the middleware above can only send its
		// 500 while the status line is still unsent.
		cw.close()
	})
}

// compressWriter decides, on the first write, whether to gzip what follows.
type compressWriter struct {
	http.ResponseWriter

	status int
	// held is the body written before the decision. The write that takes it
	// past compressMinBytes decides.
	held []byte
	// decided is set once the headers are sent.
	decided bool
	gz      *gzip.Writer
}

func (c *compressWriter) WriteHeader(code int) {
	if !c.decided {
		c.status = code
	}
}

func (c *compressWriter) Write(p []byte) (int, error) {
	switch {
	case c.gz != nil:
		return c.gz.Write(p)
	case c.decided:
		return c.ResponseWriter.Write(p)
	}
	c.held = append(c.held, p...)
	if len(c.held) < compressMinBytes {
		return len(p), nil
	}
	if err := c.decide(true); err != nil {
		return 0, err
	}
	return len(p), nil
}

// decide sends the headers and the held body. worthwhile is false for a
// response that ended under the threshold, which goes out as it is.
func (c *compressWriter) decide(worthwhile bool) error {
	c.decided = true
	header := c.Header()
	if worthwhile && header.Get("Content-Encoding") == "" &&
		compressibleType(header.Get("Content-Type")) {
		header.Set("Content-Encoding", "gzip")
		// The set length is the uncompressed one; send chunked instead.
		header.Del("Content-Length")
		c.gz = gzipPool.Get().(*gzip.Writer)
		c.gz.Reset(c.ResponseWriter)
	}
	c.ResponseWriter.WriteHeader(c.status)
	held := c.held
	c.held = nil
	if len(held) == 0 {
		return nil
	}
	if c.gz != nil {
		_, err := c.gz.Write(held)
		return err
	}
	_, err := c.ResponseWriter.Write(held)
	return err
}

// close finishes the response, deciding for a handler that wrote less than
// the threshold or nothing at all, like a 204 or 304.
func (c *compressWriter) close() {
	if !c.decided {
		_ = c.decide(false)
	}
	if c.gz == nil {
		return
	}
	zw := c.gz
	c.gz = nil
	_ = zw.Close()
	zw.Reset(nil) // do not pin the response writer in the pool
	gzipPool.Put(zw)
}

// FlushError forces the decision and pushes everything through, so a streamed
// response reaches the client. An event stream is not compressed, so after the
// first flush this is a pass-through.
func (c *compressWriter) FlushError() error {
	if !c.decided {
		if err := c.decide(true); err != nil {
			return err
		}
	}
	if c.gz != nil {
		if err := c.gz.Flush(); err != nil {
			return err
		}
	}
	return http.NewResponseController(c.ResponseWriter).Flush()
}

// Unwrap lets http.NewResponseController reach past this wrapper, for example
// to set the write deadline the streamed request log needs.
func (c *compressWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }
