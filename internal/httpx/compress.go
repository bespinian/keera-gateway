package httpx

import (
	"compress/gzip"
	"net/http"
	"strings"
	"sync"
)

// Compression for the control plane, as there is often no proxy in front to do
// it. The inference plane is left alone: buffering would delay a stream.

// compressMinBytes is the smallest response worth compressing.
const compressMinBytes = 1 << 10

// gzipPool reuses compressors: each carries about 300KB of state.
var gzipPool = sync.Pool{
	New: func() any {
		// Not BestCompression: per request, it costs more CPU than it saves.
		zw, _ := gzip.NewWriterLevel(nil, gzip.DefaultCompression)
		return zw
	},
}

// AcceptsGzip reports whether an Accept-Encoding header asks for gzip. A
// substring test is enough, as no other encoding is on offer.
func AcceptsGzip(header string) bool {
	return strings.Contains(header, "gzip")
}

// compressibleType reports whether a Content-Type is worth gzipping. Not
// text/event-stream: the buffer would hold back the live request log.
func compressibleType(ct string) bool {
	switch ct = MediaType(ct); ct {
	case "application/json", "image/svg+xml":
		return true
	case "text/event-stream":
		return false
	}
	return strings.HasPrefix(ct, "text/")
}

// Compress gzips the responses of next for clients that asked for it.
//
// It decides on the first write, once the content type and size are known, so
// a small response can still go out plain. A response that already has a
// Content-Encoding is left alone.
func Compress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set even when not compressing, so a cache does not serve the plain
		// answer to everyone.
		w.Header().Set("Vary", "Accept-Encoding")
		// After an upgrade nothing may be written to the connection.
		if r.Header.Get("Upgrade") != "" || !AcceptsGzip(r.Header.Get("Accept-Encoding")) {
			next.ServeHTTP(w, r)
			return
		}
		cw := &compressWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(cw, r)
		// Not deferred: after a panic, the middleware above can only send its
		// 500 if the status line is unsent.
		cw.close()
	})
}

// compressWriter decides, on the first write, whether to gzip what follows.
type compressWriter struct {
	http.ResponseWriter

	status int
	// held is the body written before the decision.
	held    []byte
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
// the threshold or nothing, like a 204 or 304.
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
// response reaches the client.
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

// Unwrap lets http.NewResponseController reach past this wrapper, such as to
// set the request log stream's write deadline.
func (c *compressWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }
