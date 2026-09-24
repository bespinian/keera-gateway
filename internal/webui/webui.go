// Package webui serves the Keera Gateway control panel.
//
// The panel is plain ES modules and CSS with no build step, embedded in the
// binary, so `go build` is the whole build and an air-gapped install needs no
// npm.
package webui

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/httpx"
)

//go:embed assets
var assets embed.FS

// file is one embedded asset. The assets never change, so they are gzipped
// once at startup rather than per request.
type file struct {
	body        []byte
	contentType string
	etag        string
	// gz is the gzipped body, or nil when compressing did not make it smaller.
	gz []byte
	// gzEtag must differ from etag, or a cache could hand the gzipped body to
	// a client that cannot read it.
	gzEtag string
}

var (
	files map[string]file
	// shell is the document every route that is not a file falls back to.
	shell file
)

// shellPath is where the embedded document lives. The map is keyed by the URL
// the browser asks for, so keys carry the assets/ prefix.
const shellPath = "/assets/index.html"

func init() {
	files = make(map[string]file)
	err := fs.WalkDir(assets, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(assets, p)
		if err != nil {
			return err
		}
		files["/"+p] = newFile(p, body)
		return nil
	})
	if err != nil {
		panic("webui: " + err.Error())
	}
	var ok bool
	if shell, ok = files[shellPath]; !ok {
		panic("webui: " + shellPath + " is not embedded")
	}
}

func newFile(p string, body []byte) file {
	// With no build step there are no content-hashed filenames. An ETag gives
	// the same result: a changed file is refetched, an unchanged one is a 304.
	sum := sha256.Sum256(body)
	tag := `"` + base64.RawURLEncoding.EncodeToString(sum[:12]) + `"`
	f := file{body: body, contentType: contentTypeFor(p), etag: tag}
	if gz := squeeze(body); gz != nil {
		f.gz, f.gzEtag = gz, tag[:len(tag)-1]+`-gzip"`
	}
	return f
}

// squeeze gzips one asset at the highest setting, which is cheap once at
// startup. It returns nil when the result is not smaller, as for a tiny SVG.
func squeeze(body []byte) []byte {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil
	}
	if _, err := zw.Write(body); err != nil {
		return nil
	}
	if err := zw.Close(); err != nil {
		return nil
	}
	if buf.Len() >= len(body) {
		return nil
	}
	return buf.Bytes()
}

func contentTypeFor(p string) string {
	switch path.Ext(p) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".json":
		return "application/json"
	case ".woff2":
		return "font/woff2"
	default:
		return "application/octet-stream"
	}
}

// contentSecurityPolicy keeps scripts and connections same-origin and forbids
// framing. Inline styles are allowed because charts set sizes from data;
// inline scripts, where an injection would hurt, are not.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'; " +
	"base-uri 'none'; " +
	"object-src 'none'"

// Handler serves the panel, falling back to the application shell for any path
// that is not a file: the panel routes on the URL, so /teams loads the same
// document as /.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		name := path.Clean(r.URL.Path)
		f, ok := files[name]
		if !ok {
			// A missing asset is a 404. The shell would give the browser HTML
			// where it expected JavaScript, which fails later and less clearly.
			if strings.HasPrefix(name, "/assets/") {
				http.NotFound(w, r)
				return
			}
			f = shell
		}

		zipped := f.gz != nil && httpx.AcceptsGzip(r.Header.Get("Accept-Encoding"))
		body, etag := f.body, f.etag
		if zipped {
			body, etag = f.gz, f.gzEtag
		}
		setHeaders(w.Header(), f.contentType, etag, zipped)

		if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(body))
	})
}

func setHeaders(header http.Header, contentType, etag string, zipped bool) {
	header.Set("Content-Type", contentType)
	header.Set("ETag", etag)
	// Both bodies live at this URL, so caches must know what tells them apart.
	header.Set("Vary", "Accept-Encoding")
	if zipped {
		header.Set("Content-Encoding", "gzip")
	}
	header.Set("Content-Security-Policy", contentSecurityPolicy)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "same-origin")
	// Only the browser may cache the control panel, and only after
	// revalidating.
	header.Set("Cache-Control", "no-cache, private")
}
