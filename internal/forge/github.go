package forge

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/sandbox"
)

// GitHub mints installation tokens of a GitHub App: one repository, contents
// read and write, and nothing else. GitHub fixes their lifetime at an hour, so
// a sandbox that lives longer asks the gateway for a fresh one.
//
// GitHub cannot revoke such a token without the token itself, which is not
// kept, so the last one can outlive its sandbox by up to an hour.
type GitHub struct {
	api    *url.URL
	web    *url.URL
	appID  string
	key    *rsa.PrivateKey
	client *http.Client
	now    func() time.Time
}

// GitHubOptions configures the GitHub minter.
type GitHubOptions struct {
	// API is the REST API: https://api.github.com, or https://<host>/api/v3
	// for GitHub Enterprise Server. Empty is GitHub.com.
	API string
	// AppID is the App's id, or its client id.
	AppID string
	// Key is the App's private key, as GitHub downloads it (PEM).
	Key    []byte
	Client *http.Client
}

// NewGitHub builds the minter.
func NewGitHub(o GitHubOptions) (*GitHub, error) {
	if o.API == "" {
		o.API = "https://api.github.com"
	}
	api, err := url.Parse(strings.TrimRight(o.API, "/"))
	if err != nil || api.Host == "" {
		return nil, fmt.Errorf("%q is not a GitHub API address", o.API)
	}
	if o.AppID == "" {
		return nil, errors.New("a GitHub App id is required")
	}
	key, err := parseRSAKey(o.Key)
	if err != nil {
		return nil, fmt.Errorf("the GitHub App's private key: %w", err)
	}
	return &GitHub{
		api: api, web: githubWeb(api), appID: o.AppID, key: key,
		client: httpClient(o.Client), now: time.Now,
	}, nil
}

// githubWeb is where repositories are cloned from, for an API address.
func githubWeb(api *url.URL) *url.URL {
	web := *api
	web.Path = strings.TrimSuffix(web.Path, "/api/v3")
	if strings.EqualFold(api.Hostname(), "api.github.com") {
		web.Host = "github.com"
	}
	return &web
}

// Mint implements sandbox.GitMinter.
func (g *GitHub) Mint(ctx context.Context, req sandbox.GitRequest) (sandbox.GitCredential, error) {
	r, err := parseRepo(req.Repo)
	if err != nil {
		return sandbox.GitCredential{}, err
	}
	if r, err = r.on(g.web); err != nil {
		return sandbox.GitCredential{}, err
	}
	owner, name, _ := strings.Cut(r.path, "/")
	if strings.Contains(name, "/") {
		return sandbox.GitCredential{}, badRepo(req.Repo)
	}
	jwt, err := g.appJWT()
	if err != nil {
		return sandbox.GitCredential{}, err
	}
	header := http.Header{
		"Accept":               {"application/vnd.github+json"},
		"Authorization":        {"Bearer " + jwt},
		"X-Github-Api-Version": {"2022-11-28"},
	}

	var install struct {
		ID int64 `json:"id"`
	}
	err = call(ctx, g.client, http.MethodGet,
		g.endpoint("repos", owner, name, "installation"), header, nil, &install)
	if status(err) == http.StatusNotFound {
		return sandbox.GitCredential{}, &sandbox.ErrRefused{Reason: fmt.Sprintf(
			"Keera's GitHub App is not installed on %s, so it cannot give a sandbox access; "+
				"install it on that repository first", r.path)}
	}
	if err != nil {
		return sandbox.GitCredential{}, fmt.Errorf("finding the GitHub App installation: %w", err)
	}

	var token struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	err = call(ctx, g.client, http.MethodPost,
		g.endpoint("app", "installations", strconv.FormatInt(install.ID, 10), "access_tokens"),
		header, map[string]any{
			"repositories": []string{name},
			// Enough to clone and push a branch. Not workflows: an agent must
			// not be able to change what CI runs with.
			"permissions": map[string]string{"contents": "write"},
		}, &token)
	if s := status(err); s == http.StatusUnprocessableEntity || s == http.StatusForbidden {
		return sandbox.GitCredential{}, &sandbox.ErrRefused{Reason: fmt.Sprintf(
			"GitHub refused a token for %s: %s. The App needs read and write access to "+
				"repository contents", r.path, reason(err))}
	}
	if err != nil {
		return sandbox.GitCredential{}, fmt.Errorf("minting a GitHub installation token: %w", err)
	}
	return sandbox.GitCredential{
		Repo:     r.cloneURL(g.web),
		Username: "x-access-token",
		Token:    token.Token,
		Expires:  token.ExpiresAt,
	}, nil
}

func (g *GitHub) endpoint(parts ...string) string {
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return g.api.String() + "/" + strings.Join(parts, "/")
}

// appJWT is the App's own short-lived identity, which only asks for
// installation tokens.
func (g *GitHub) appJWT() (string, error) {
	now := g.now()
	var iss any = g.appID
	if n, err := strconv.ParseInt(g.appID, 10, 64); err == nil {
		iss = n
	}
	claims, err := json.Marshal(map[string]any{
		// A minute back, for a clock that runs ahead of GitHub's.
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": iss,
	})
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	unsigned := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." +
		enc.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, g.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + enc.EncodeToString(sig), nil
}

// parseRSAKey reads a PEM private key. GitHub hands out PKCS #1; PKCS #8 is
// what a key converted by another tool usually is.
func parseRSAKey(raw []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("not a PEM file")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("not an RSA private key")
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA private key")
	}
	return rsaKey, nil
}
