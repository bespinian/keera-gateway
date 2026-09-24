package control

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/sandbox"
	"github.com/bespinian/keera-gateway/internal/store"
)

// The attach surface: a byte stream between a signed-in caller and a port
// inside a sandbox.
//
// It is a path on the one listener rather than a second port, so a
// deployment keeps one address and one certificate. The caller sends a GET
// with an Upgrade header; the gateway authenticates it like any control
// request, dials the sandbox, answers 101 and copies bytes until one side
// stops.
//
// The far end is a plain ssh server, so VS Code Remote-SSH and JetBrains
// Gateway work unchanged, with `keera sandbox ssh` as the ProxyCommand.

// upgradeProtocol is the token both ends send. It is versioned because the
// developer's `keera` binary may be older than the gateway.
const upgradeProtocol = "keera-sandbox/1"

// attachIdleTimeout closes a connection with no bytes either way for this
// long. It is generous, because the traffic is a person typing. It is there
// for connections nobody closes, like a laptop put in a bag.
const attachIdleTimeout = 2 * time.Hour

// attachRoutes registers the surface.
func (s *Server) attachRoutes(mux *http.ServeMux) {
	if s.opts.Sandboxes == nil {
		// Without a driver, the path says so. A 404 would look like a version
		// mismatch.
		mux.HandleFunc(httpx.SandboxPrefix+"/", func(w http.ResponseWriter, _ *http.Request) {
			s.requireSandboxes(w)
		})
		return
	}
	mux.Handle("GET "+httpx.SandboxPrefix+"/v1/{ref}/tcp/{port}", s.authenticated(s.attach))
	// Asked for by a sandbox, not a person: it has no session, only its key.
	mux.Handle("POST "+httpx.SandboxPrefix+sandbox.GitCredentialPath,
		s.throttle("git_credential", signInRPM, s.gitCredential))
}

// attach proxies one connection.
func (s *Server) attach(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), upgradeProtocol) {
		httpx.WriteError(w, http.StatusUpgradeRequired, "invalid_request_error", "upgrade_required",
			"this route carries a byte stream, not JSON; send 'Upgrade: "+upgradeProtocol+
				"'. `keera sandbox ssh <name>` is the client for it")
		return
	}
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || !sandbox.ValidPort(port) {
		badRequest(w, fmt.Sprintf("%q is not a port this surface will reach: it has to be unprivileged "+
			"(%d-%d), which the shell on %d is. Nothing inside a sandbox can bind a "+
			"privileged port anyway - it runs as an ordinary user with every capability "+
			"dropped",
			r.PathValue("port"), sandbox.PortForwardMin, sandbox.PortForwardMax,
			sandbox.PortSSH))
		return
	}

	sb, ok := s.resolveSandbox(w, r, p)
	if !ok {
		return
	}
	// Seeing a sandbox is not enough to open a shell in it. See canAttach.
	if !canAttach(p, sb) {
		s.forbid(w, "that sandbox belongs to somebody else; an administrator can see it and "+
			"can delete it, and opening a shell in it is the owner's alone - it holds their "+
			"working copy")
		return
	}

	conn, err := s.opts.Sandboxes.Dial(r.Context(), sb, port)
	if err != nil {
		s.failSandbox(w, err)
		return
	}
	defer func() { _ = conn.Close() }()

	client, err := hijack(w)
	if err != nil {
		s.log.Error("sandbox attach could not take over the connection", "error", err,
			"request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "server_error", "",
			"this connection cannot be upgraded; a proxy in front of the gateway may be "+
				"rewriting it")
		return
	}
	defer func() { _ = client.Close() }()

	s.auditf(r, p, sb.OrgID, "sandbox.attach", "sandbox", sb.ID, map[string]any{
		"name": sb.Name, "port": port,
	})

	if _, err := client.write([]byte(
		"HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: " + upgradeProtocol + "\r\n" +
			"Connection: Upgrade\r\n\r\n")); err != nil {
		return
	}
	pipe(client, conn, attachIdleTimeout)
}

// resolveSandbox finds the sandbox a request names, by id or by name.
//
// Names are accepted so `keera sandbox ssh` needs no extra round trip before
// ssh gets its bytes. A name is unique only inside one organisation.
func (s *Server) resolveSandbox(w http.ResponseWriter, r *http.Request, p *authn.Principal) (
	store.Sandbox, bool,
) {
	ref := r.PathValue("ref")
	if ref == "" {
		ref = r.PathValue("id")
	}
	var (
		sb  store.Sandbox
		err error
	)
	if strings.HasPrefix(ref, "sbx_") {
		sb, err = s.st.Sandbox(r.Context(), ref)
	} else {
		orgID, ok := s.requireOrg(w, p, r.URL.Query().Get("org_id"),
			"name an organisation, or name the sandbox by its id; a sandbox name is only "+
				"unique inside one organisation")
		if !ok {
			return store.Sandbox{}, false
		}
		sb, err = s.st.LiveSandboxByName(r.Context(), orgID, ref)
	}
	if err != nil {
		s.fail(w, err)
		return store.Sandbox{}, false
	}
	if !p.CanReadOrg(sb.OrgID) {
		// The same answer as a missing sandbox, so this route does not reveal
		// other tenants' sandbox names.
		s.fail(w, store.ErrNotFound)
		return store.Sandbox{}, false
	}
	return sb, true
}

// canAttach reports whether this principal may open a connection into a
// sandbox: an operator, or the person it belongs to.
//
// An organisation's administrator may not. A sandbox holds someone's working
// copy and terminal, and seeing the bill does not mean reading the desk. A
// sandbox that belongs to nobody, such as one made for a pipeline, is the
// operator's alone, so it never becomes a shared shell.
func canAttach(p *authn.Principal, sb store.Sandbox) bool {
	if p.Unrestricted() {
		return true
	}
	return sb.UserID != "" && p.UserID == sb.UserID
}

/* --------------------------------------------------------------- the plumbing */

// hijacked is a connection taken over from the HTTP server, with whatever the
// server had already buffered from the client still in front of it.
type hijacked struct {
	conn net.Conn
	buf  *bufio.ReadWriter
}

func (h *hijacked) write(b []byte) (int, error) {
	n, err := h.buf.Write(b)
	if err != nil {
		return n, err
	}
	return n, h.buf.Flush()
}

func (h *hijacked) Read(b []byte) (int, error)  { return h.buf.Read(b) }
func (h *hijacked) Write(b []byte) (int, error) { return h.write(b) }
func (h *hijacked) Close() error                { return h.conn.Close() }

// hijack takes the connection away from the HTTP server.
//
// It uses http.NewResponseController, not a type assertion, because the
// writer is wrapped by the access log and the compressor. Both implement
// Unwrap, which the controller follows.
func hijack(w http.ResponseWriter) (*hijacked, error) {
	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return nil, err
	}
	// No deadline: it would cut an interactive session mid-typing. The idle
	// timeout in pipe bounds the connection instead.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &hijacked{conn: conn, buf: buf}, nil
}

// pipe copies bytes both ways until either end stops, or until neither has
// sent anything for idle.
//
// Either copy ending closes both sides: a client that leaves should not keep
// the sandbox side open, and a sandbox whose sshd exits should end the
// client's session.
func pipe(client io.ReadWriteCloser, remote net.Conn, idle time.Duration) {
	var (
		once sync.Once
		done = make(chan struct{})
	)
	stop := func() {
		once.Do(func() {
			close(done)
			_ = client.Close()
			_ = remote.Close()
		})
	}

	// Both copies record activity here, and the watchdog reads it. Resetting a
	// deadline instead would cost a syscall per keystroke.
	var activity atomic64
	activity.set(time.Now().UnixNano())

	go func() {
		defer stop()
		_, _ = io.Copy(&noticing{w: remote, seen: &activity}, client)
	}()
	go func() {
		defer stop()
		_, _ = io.Copy(&noticing{w: client, seen: &activity}, remote)
	}()

	if idle <= 0 {
		<-done
		return
	}
	// A coarse tick is enough: noticing an abandoned connection a minute late
	// costs nothing.
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case now := <-t.C:
			if now.UnixNano()-activity.get() > int64(idle) {
				stop()
				return
			}
		}
	}
}

// noticing records that bytes moved, for the idle watchdog.
type noticing struct {
	w    io.Writer
	seen *atomic64
}

func (n *noticing) Write(b []byte) (int, error) {
	n.seen.set(time.Now().UnixNano())
	return n.w.Write(b)
}

// failSandbox maps a driver's refusal onto a status code.
//
// A refusal is 409, not 400: the request was fine, but the sandbox is in the
// wrong state, which can change. The driver's own message is passed on, as it
// is written for the developer.
func (s *Server) failSandbox(w http.ResponseWriter, err error) {
	var refused *sandbox.ErrRefused
	switch {
	case errors.As(err, &refused):
		httpx.WriteError(w, http.StatusConflict, "invalid_request_error", "sandbox_state",
			refused.Reason)
	case errors.Is(err, sandbox.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "invalid_request_error", "not_found",
			"that sandbox is no longer in the cluster")
	case errors.Is(err, sandbox.ErrNotReady):
		httpx.WriteError(w, http.StatusConflict, "invalid_request_error", "sandbox_not_ready",
			err.Error())
	case errors.Is(err, sandbox.ErrUnsupported):
		httpx.WriteError(w, http.StatusNotImplemented, "invalid_request_error", "unsupported",
			err.Error())
	default:
		s.fail(w, err)
	}
}
