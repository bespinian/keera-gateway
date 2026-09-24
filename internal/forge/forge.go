// Package forge mints the short-lived repository credentials sandboxes check
// out with: a GitHub App installation token, or a GitLab project access token.
//
// Each needs a credential of the deployment's own, which never leaves the
// gateway. What a sandbox gets is scoped to one repository and runs out.
package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/sandbox"
)

// requestTimeout bounds one call to a forge. Minting runs while somebody
// waits for their sandbox.
const requestTimeout = 20 * time.Second

// repo is a repository on one host: "owner/name", or "group/sub/name" on
// GitLab.
type repo struct {
	host string
	path string
}

// segment is one part of a repository path. It is stricter than either forge,
// so an address can never reach another API route.
var segment = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// parseRepo reads the ways a repository is usually written: an https URL, an
// ssh URL, or git@host:path.
func parseRepo(raw string) (repo, error) {
	raw = strings.TrimSpace(raw)
	var host, path string
	switch {
	case strings.Contains(raw, "://"):
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
			return repo{}, badRepo(raw)
		}
		host, path = u.Hostname(), u.Path
	case strings.Contains(raw, ":"):
		// git@host:owner/name.git, as a forge's "clone with ssh" button writes it.
		before, after, _ := strings.Cut(raw, ":")
		_, host, _ = strings.Cut(before, "@")
		if host == "" {
			host = before
		}
		path = after
	default:
		return repo{}, badRepo(raw)
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	parts := strings.Split(path, "/")
	if host == "" || len(parts) < 2 {
		return repo{}, badRepo(raw)
	}
	for _, p := range parts {
		if !segment.MatchString(p) || p == "." || p == ".." {
			return repo{}, badRepo(raw)
		}
	}
	return repo{host: strings.ToLower(host), path: path}, nil
}

func badRepo(raw string) error {
	return &sandbox.ErrRefused{Reason: fmt.Sprintf("%q is not a repository address; use the "+
		"https or ssh address the forge shows for cloning", raw)}
}

// on checks that a repository is on the forge this deployment mints for, and
// returns its path on that forge. A forge served under a path, such as
// https://example.ch/gitlab, has that path taken off.
func (r repo) on(web *url.URL) (repo, error) {
	if !strings.EqualFold(r.host, web.Hostname()) {
		return repo{}, &sandbox.ErrRefused{Reason: fmt.Sprintf("%s is on %s, and this "+
			"deployment mints repository credentials for %s only",
			r.path, r.host, web.Hostname())}
	}
	if prefix := strings.Trim(web.Path, "/"); prefix != "" {
		r.path = strings.TrimPrefix(r.path, prefix+"/")
	}
	return r, nil
}

// cloneURL is the https address a token works with, whatever address was
// asked for.
func (r repo) cloneURL(web *url.URL) string {
	return strings.TrimRight(web.String(), "/") + "/" + r.path + ".git"
}

// call sends one JSON request and decodes a 2xx answer into out. Anything
// else comes back as an *apiError, with the forge's own message.
func call(ctx context.Context, c *http.Client, method, target string, header http.Header,
	body, out any,
) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return err
	}
	req.Header = header.Clone()
	req.Header.Set("User-Agent", "keera-gateway")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode/100 != 2 {
		return &apiError{Status: res.StatusCode, Message: message(raw)}
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// status is the HTTP status of a forge's refusal, or 0.
func status(err error) int {
	var api *apiError
	if errors.As(err, &api) {
		return api.Status
	}
	return 0
}

// reason is what a forge said when it refused.
func reason(err error) string {
	var api *apiError
	if errors.As(err, &api) && api.Message != "" {
		return api.Message
	}
	return "no reason given"
}

// apiError is a forge's refusal.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("the forge answered %d", e.Status)
	}
	return fmt.Sprintf("the forge answered %d: %s", e.Status, e.Message)
}

// message is the readable part of an error body. GitHub puts it in "message";
// GitLab in "message" or "error", sometimes as an object.
func message(raw []byte) string {
	var body struct {
		Message any    `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(raw, &body) == nil {
		switch m := body.Message.(type) {
		case string:
			return m
		case nil:
			if body.Error != "" {
				return body.Error
			}
		default:
			if b, err := json.Marshal(m); err == nil {
				return string(b)
			}
		}
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func httpClient(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: requestTimeout}
}
