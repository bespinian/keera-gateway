package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/sandbox"
)

// GitLab mints a project access token per sandbox: one project, the Developer
// role, and repository read and write.
//
// GitLab sets expiry by the day, so a token runs until midnight UTC after its
// sandbox ends. The manager revokes it when the sandbox ends, so in practice
// it lives exactly as long as the sandbox.
//
// Developer can push to unprotected branches only, so an agent cannot push to
// a protected default branch.
type GitLab struct {
	web    *url.URL
	token  string
	client *http.Client
}

// GitLabOptions configures the GitLab minter.
type GitLabOptions struct {
	// URL is the instance, such as https://gitlab.example.ch. Empty is
	// GitLab.com.
	URL string
	// Token may create project access tokens on the projects sandboxes use:
	// a personal, group or service account token with the api scope, held by
	// a Maintainer of those projects.
	Token  string
	Client *http.Client
}

// NewGitLab builds the minter.
func NewGitLab(o GitLabOptions) (*GitLab, error) {
	if o.URL == "" {
		o.URL = "https://gitlab.com"
	}
	web, err := url.Parse(strings.TrimRight(o.URL, "/"))
	if err != nil || web.Host == "" {
		return nil, fmt.Errorf("%q is not a GitLab address", o.URL)
	}
	if strings.TrimSpace(o.Token) == "" {
		return nil, errors.New("a GitLab token is required")
	}
	return &GitLab{web: web, token: strings.TrimSpace(o.Token), client: httpClient(o.Client)}, nil
}

// Mint implements sandbox.GitMinter.
func (g *GitLab) Mint(ctx context.Context, req sandbox.GitRequest) (sandbox.GitCredential, error) {
	r, err := parseRepo(req.Repo)
	if err != nil {
		return sandbox.GitCredential{}, err
	}
	if r, err = r.on(g.web); err != nil {
		return sandbox.GitCredential{}, err
	}
	expires := dayAfter(req.Until)
	name := "keera sandbox"
	if req.Sandbox != "" {
		name += " " + req.Sandbox
	}
	var token struct {
		ID    int64  `json:"id"`
		Token string `json:"token"`
	}
	err = call(ctx, g.client, http.MethodPost, g.tokens(r.path, ""), g.header(),
		map[string]any{
			"name":         name,
			"scopes":       []string{"read_repository", "write_repository"},
			"access_level": 30, // Developer
			"expires_at":   expires.Format(time.DateOnly),
		}, &token)
	switch status(err) {
	case 0:
	case http.StatusNotFound:
		return sandbox.GitCredential{}, &sandbox.ErrRefused{Reason: fmt.Sprintf(
			"there is no project %s on %s, or Keera's GitLab token cannot see it",
			r.path, g.web.Hostname())}
	case http.StatusForbidden, http.StatusUnauthorized:
		return sandbox.GitCredential{}, &sandbox.ErrRefused{Reason: fmt.Sprintf(
			"Keera's GitLab token may not create access tokens on %s; it needs the api "+
				"scope and the Maintainer role on that project", r.path)}
	case http.StatusBadRequest:
		return sandbox.GitCredential{}, &sandbox.ErrRefused{Reason: fmt.Sprintf(
			"GitLab refused a token for %s: %s", r.path, reason(err))}
	}
	if err != nil {
		return sandbox.GitCredential{}, fmt.Errorf("minting a GitLab project access token: %w", err)
	}
	return sandbox.GitCredential{
		Repo: r.cloneURL(g.web),
		// GitLab accepts any username with a project access token.
		Username: "keera",
		Token:    token.Token,
		ID:       strconv.FormatInt(token.ID, 10),
		Expires:  expires,
	}, nil
}

// Revoke implements sandbox.GitRevoker. A token already gone is fine.
func (g *GitLab) Revoke(ctx context.Context, repo, id string) error {
	r, err := parseRepo(repo)
	if err != nil {
		return err
	}
	if r, err = r.on(g.web); err != nil {
		return err
	}
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return fmt.Errorf("%q is not a GitLab token id", id)
	}
	err = call(ctx, g.client, http.MethodDelete, g.tokens(r.path, id), g.header(), nil, nil)
	if status(err) == http.StatusNotFound {
		return nil
	}
	return err
}

// tokens is the project access token route of one project, or of one token.
func (g *GitLab) tokens(project, id string) string {
	u := g.web.String() + "/api/v4/projects/" + url.PathEscape(project) + "/access_tokens"
	if id != "" {
		u += "/" + id
	}
	return u
}

func (g *GitLab) header() http.Header {
	return http.Header{"Private-Token": {g.token}}
}

// dayAfter is the first midnight UTC after t: the earliest expiry date GitLab
// accepts that still covers t.
func dayAfter(t time.Time) time.Time {
	day := t.UTC().Truncate(24 * time.Hour)
	return day.Add(24 * time.Hour)
}
