package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/httpx"
)

// client talks to the control API.
//
// base is the gateway's origin. Calls name paths relative to
// httpx.ControlPrefix and send() adds the prefix, so call sites read like the
// routes.
type client struct {
	base string
	// baseFrom says what chose base, so commands can tell which gateway they
	// act on and why.
	baseFrom string
	key      string
	// signedIn is what `keera login` stored for this gateway, used when no
	// operator key is set. See credentials.go.
	signedIn signIn
	http     *http.Client
}

// urlFlag is the global --url, which Run takes off the arguments before any
// command sees them.
var urlFlag string

// defaultBase is the hosted gateway, used when nothing names another.
const defaultBase = "https://gateway.keera.ch"

// resolveBase decides which gateway this invocation talks to, and says what
// decided it. The most deliberate choice wins: --url, then
// KEERA_CONTROL_URL, then the gateway of the last `keera login`.
func resolveBase() (base, from string) {
	switch {
	case urlFlag != "":
		return urlFlag, "--url"
	case os.Getenv("KEERA_CONTROL_URL") != "":
		return os.Getenv("KEERA_CONTROL_URL"), "KEERA_CONTROL_URL"
	}
	if saved := defaultGateway(); saved != "" {
		return saved, "the gateway you last signed in to"
	}
	return defaultBase, "the default"
}

// newClient reads the environment and the credentials file.
//
// An exported operator key wins over a stored sign-in, because it was set on
// purpose for this shell. A missing credential only fails at the first
// request, so that `--help` works without one.
func newClient() *client {
	base, from := resolveBase()
	base = strings.TrimRight(base, "/")
	c := &client{
		base:     base,
		baseFrom: from,
		key:      os.Getenv("KEERA_OPERATOR_KEY"),
		http:     &http.Client{Timeout: 30 * time.Second},
	}
	if c.key == "" {
		c.signedIn = signInFor(base)
	}
	return c
}

// bearer is what this client presents, and empty when it has none.
func (c *client) bearer() string {
	if c.key != "" {
		return c.key
	}
	return c.signedIn.Token
}

// apiError carries a control-plane error envelope.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("control API returned %d", e.Status)
	}
	return e.Message
}

// do makes an authenticated call, and is what every command goes through.
func (c *client) do(ctx context.Context, method, path string, in, out any) error {
	cred := c.bearer()
	switch {
	case cred == "":
		return fmt.Errorf("not signed in to %s (%s): run "+
			"'keera login --url <your deployment>', or set KEERA_OPERATOR_KEY to the "+
			"deployment's own credential", c.base, c.baseFrom)
	case c.key == "" && c.signedIn.expired():
		return fmt.Errorf("your sign-in to %s ran out on %s: run 'keera login' again",
			c.base, c.signedIn.ExpiresAt.Local().Format("2 January 2006"))
	}
	err := c.send(ctx, method, path, cred, in, out)
	// A refused token is a sign-in that ended elsewhere: it expired, the
	// person's role changed, or they signed out everywhere.
	var apiErr *apiError
	if c.key == "" && errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized {
		return fmt.Errorf("your sign-in to %s is no longer valid: run 'keera login' again", c.base)
	}
	return err
}

// anon makes a call without a credential, for the sign-in routes.
func (c *client) anon(ctx context.Context, method, path string, in, out any) error {
	return c.send(ctx, method, path, "", in, out)
}

func (c *client) send(ctx context.Context, method, path, cred string, in, out any) error {
	var reader io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+httpx.ControlPrefix+path, reader)
	if err != nil {
		return err
	}
	if cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// When the built-in default does not answer, the likelier cause is
		// that the person meant their own deployment and never said so.
		if c.baseFrom == "the default" {
			return fmt.Errorf("reaching the control API at %s: %w; nothing said which "+
				"gateway to use, so this is the built-in default - the hosted one. "+
				"Sign in to yours with 'keera login --url https://keera.example.ch'", c.base, err)
		}
		return fmt.Errorf("reaching the control API at %s (%s): %w", c.base, c.baseFrom, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var envelope struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &envelope)
		return &apiError{Status: resp.StatusCode, Message: envelope.Error.Message}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}
