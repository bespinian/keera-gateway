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

// client talks to the control API. send adds httpx.ControlPrefix, so call
// sites read like the routes.
type client struct {
	base string
	// baseFrom says what chose base.
	baseFrom string
	key      string
	// signedIn is used when no operator key is set.
	signedIn signIn
	// org is used when a command names none; orgFrom says what chose it.
	org     string
	orgFrom string
	http    *http.Client
}

var urlFlag string

// defaultBase is the hosted gateway.
const defaultBase = "https://gateway.keera.ch"

// resolveBase lets the most deliberate choice win: --url, then
// KEERA_CONTROL_URL, then the gateway of the last `keera login`.
func resolveBase(saved string) (base, from string) {
	switch {
	case urlFlag != "":
		return urlFlag, "--url"
	case env("KEERA_CONTROL_URL") != "":
		return env("KEERA_CONTROL_URL"), "KEERA_CONTROL_URL"
	case saved != "":
		return saved, "the gateway you last signed in to"
	}
	return defaultBase, "the default"
}

// env trims the spaces a pasted value often brings, as the gateway does.
func env(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}

// newClient lets an exported operator key win over a stored sign-in. A
// missing credential only fails at the first request, so `--help` works.
func newClient() *client {
	// A file that cannot be read holds no sign-in.
	creds, _ := loadCredentials()
	base, from := resolveBase(creds.Default)
	base = strings.TrimRight(base, "/")
	c := &client{
		base:     base,
		baseFrom: from,
		key:      env("KEERA_OPERATOR_KEY"),
		// send sets the timeout per request.
		http: &http.Client{},
	}
	if c.key == "" {
		c.signedIn = creds.Gateways[gatewayKey(base)]
	}
	if org := env("KEERA_ORG"); org != "" {
		c.org, c.orgFrom = org, "KEERA_ORG"
	} else if org := creds.Orgs[gatewayKey(base)]; org != "" {
		c.org, c.orgFrom = org, "keera org switch"
	}
	return c
}

func (c *client) bearer() string {
	if c.key != "" {
		return c.key
	}
	return c.signedIn.Token
}

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

// errorMessage is separate from client.do because sandbox attach reads a
// refused upgrade off a bare connection.
func errorMessage(raw []byte) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return ""
	}
	return strings.TrimSpace(envelope.Error.Message)
}

func (c *client) credential() (string, error) {
	cred := c.bearer()
	switch {
	case cred == "":
		return "", fmt.Errorf("not signed in to %s (%s): run "+
			"'keera login --url <your deployment>', or set KEERA_OPERATOR_KEY to the "+
			"deployment's own credential", c.base, c.baseFrom)
	case c.key == "" && c.signedIn.expired():
		return "", fmt.Errorf("your sign-in to %s ran out on %s: run 'keera login' again",
			c.base, c.signedIn.ExpiresAt.Local().Format("2 January 2006"))
	}
	return cred, nil
}

func (c *client) do(ctx context.Context, method, path string, in, out any) error {
	raw, _, err := c.fetch(ctx, method, path, in)
	return decode(raw, err, out)
}

// fetch is do for a file answer. The headers carry the file name.
func (c *client) fetch(ctx context.Context, method, path string, in any) ([]byte, http.Header, error) {
	cred, err := c.credential()
	if err != nil {
		return nil, nil, err
	}
	raw, header, err := c.send(ctx, method, path, cred, in)
	// A refused token means the sign-in ended: it expired, the role changed,
	// or they signed out everywhere.
	var apiErr *apiError
	if c.key == "" && errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized {
		return nil, nil, fmt.Errorf("your sign-in to %s is no longer valid: run 'keera login' again", c.base)
	}
	return raw, header, err
}

// anon is for the sign-in routes.
func (c *client) anon(ctx context.Context, method, path string, in, out any) error {
	raw, _, err := c.send(ctx, method, path, "", in)
	return decode(raw, err, out)
}

func decode(raw []byte, err error, out any) error {
	if err != nil || out == nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// maxAnswer fits a receipt, the largest answer.
const maxAnswer = 16 << 20

// requestTimeout applies when the caller set no deadline.
const requestTimeout = 30 * time.Second

// probeTimeout is for a request that has the gateway call models, up to 30
// seconds each.
const probeTimeout = 10 * time.Minute

func (c *client) send(ctx context.Context, method, path, cred string, in any) ([]byte, http.Header, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, requestTimeout)
		defer cancel()
	}
	var reader io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return nil, nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+httpx.ControlPrefix+path, reader)
	if err != nil {
		return nil, nil, err
	}
	if cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// They more likely meant their own deployment and never said so.
		if c.baseFrom == "the default" {
			return nil, nil, fmt.Errorf("reaching the control API at %s: %w; nothing said which "+
				"gateway to use, so this is the built-in default - the hosted one. "+
				"Sign in to yours with 'keera login --url https://keera.example.ch'", c.base, err)
		}
		return nil, nil, fmt.Errorf("reaching the control API at %s (%s): %w", c.base, c.baseFrom, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, nil, &apiError{Status: resp.StatusCode, Message: errorMessage(raw)}
	}
	// Cut short, a file would be saved broken.
	if len(raw) > maxAnswer {
		return nil, nil, fmt.Errorf("the answer from %s is larger than %d MiB", c.base, maxAnswer>>20)
	}
	return raw, resp.Header, nil
}
