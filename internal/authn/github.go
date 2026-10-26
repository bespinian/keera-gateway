package authn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/oauth2"
)

// GitHubIssuer marks a provider as GitHub. GitHub speaks OAuth but not OpenID
// Connect, so Keera asks GitHub's API who signed in.
const GitHubIssuer = "https://github.com"

func (c OIDCConfig) IsGitHub() bool {
	u, err := url.Parse(c.IssuerURL)
	return err == nil && u.Host == "github.com"
}

type githubAPI struct {
	base   string
	client *http.Client
}

// newGitHub takes GitHub's addresses so tests can replace them.
func newGitHub(cfg OIDCConfig, client *http.Client, web, api string) *OIDC {
	if client == nil {
		client = http.DefaultClient
	}
	scopes := cfg.Scopes
	if len(scopes) == 0 {
		// Ask for organisations and teams only where they decide a role.
		scopes = []string{"user:email"}
		if cfg.Mapping.UsesGroups() {
			scopes = append(scopes, "read:org")
		}
	}
	return &OIDC{
		cfg:    cfg,
		github: &githubAPI{base: strings.TrimRight(api, "/"), client: client},
		oauth: &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint: oauth2.Endpoint{
				AuthURL:   web + "/login/oauth/authorize",
				TokenURL:  web + "/login/oauth/access_token",
				AuthStyle: oauth2.AuthStyleInParams,
			},
			RedirectURL: cfg.RedirectURL,
			Scopes:      scopes,
		},
	}
}

func (o *OIDC) exchangeGitHub(ctx context.Context, code string, f Flow) (Identity, error) {
	ctx = context.WithValue(ctx, oauth2.HTTPClient, o.github.client)
	token, err := o.oauth.Exchange(ctx, code,
		oauth2.SetAuthURLParam("code_verifier", f.Verifier))
	if err != nil {
		return Identity{}, fmt.Errorf("exchanging the authorization code: %w", err)
	}
	id, err := o.githubIdentity(ctx, token.AccessToken)
	if err != nil {
		return Identity{}, err
	}
	// An OAuth app's token does not expire and has no refresh token, so the
	// token itself is used to ask again.
	id.RefreshToken = token.AccessToken
	return id, o.checkGitHub(id)
}

// recheckGitHub asks GitHub again. It answers 401 once access was revoked or
// the account is gone.
func (o *OIDC) recheckGitHub(ctx context.Context, token string) (Identity, bool, error) {
	id, err := o.githubIdentity(ctx, token)
	if err != nil {
		return Identity{}, false, err
	}
	id.RefreshToken = token
	if err := o.checkGitHub(id); err != nil {
		return id, false, fmt.Errorf("%w: %w", ErrDirectoryRefused, err)
	}
	return id, true, nil
}

// checkGitHub works like checkIdentity. Every address githubIdentity picks is
// verified.
func (o *OIDC) checkGitHub(id Identity) error {
	if id.Email == "" {
		return errors.New("this GitHub account has no verified email address Keera " +
			"can use; verify one at https://github.com/settings/emails")
	}
	return o.checkIdentity(id, map[string]any{"email_verified": true}, "")
}

// githubIdentity maps a 401 to ErrDirectoryRefused; other failures say nothing
// about the person.
func (o *OIDC) githubIdentity(ctx context.Context, token string) (Identity, error) {
	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
	}
	if err := o.github.get(ctx, token, "/user", &user); err != nil {
		return Identity{}, err
	}
	if user.ID == 0 {
		return Identity{}, errors.New("GitHub answered /user without an account id")
	}
	var emails []githubEmail
	if err := o.github.get(ctx, token, "/user/emails?per_page=100", &emails); err != nil {
		return Identity{}, err
	}
	name := strings.TrimSpace(user.Name)
	if name == "" {
		name = user.Login
	}
	// The id, not the login: a login can be renamed and taken by someone else.
	id := Identity{
		Provider: o.cfg.Name,
		Subject:  strconv.FormatInt(user.ID, 10),
		Email:    o.pickEmail(emails),
		Name:     name,
	}
	if o.cfg.Mapping.UsesGroups() {
		groups, err := o.github.groups(ctx, token)
		if err != nil {
			return Identity{}, err
		}
		id.Groups = groups
	}
	return id, nil
}

type githubEmail struct {
	Email    string `json:"email"`
	Primary  bool   `json:"primary"`
	Verified bool   `json:"verified"`
}

// pickEmail picks the primary address if this provider may vouch for it, else
// the first other verified one it may, since people often add a work address.
// Failing both, it is the primary, so the refusal can name it.
func (o *OIDC) pickEmail(emails []githubEmail) string {
	var usable []string
	primary := ""
	for _, e := range emails {
		addr := strings.ToLower(strings.TrimSpace(e.Email))
		// The noreply address names nobody.
		if !e.Verified || addr == "" || strings.HasSuffix(addr, "noreply.github.com") {
			continue
		}
		if e.Primary {
			primary = addr
			usable = append([]string{addr}, usable...)
		} else {
			usable = append(usable, addr)
		}
	}
	for _, addr := range usable {
		if o.cfg.vouchesFor(Identity{Email: addr}.Domain()) {
			return addr
		}
	}
	return primary
}

// groups lists someone's organisations and teams, as "org/team". Organisation
// names are unique on GitHub, so nobody else can create one of the same name.
func (g *githubAPI) groups(ctx context.Context, token string) ([]string, error) {
	var out []string
	err := g.pages(ctx, token, "/user/orgs", func(raw json.RawMessage) error {
		var orgs []struct {
			Login string `json:"login"`
		}
		if err := json.Unmarshal(raw, &orgs); err != nil {
			return err
		}
		for _, org := range orgs {
			out = append(out, org.Login)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = g.pages(ctx, token, "/user/teams", func(raw json.RawMessage) error {
		var teams []struct {
			Slug string `json:"slug"`
			Org  struct {
				Login string `json:"login"`
			} `json:"organization"`
		}
		if err := json.Unmarshal(raw, &teams); err != nil {
			return err
		}
		for _, t := range teams {
			out = append(out, t.Org.Login+"/"+t.Slug)
		}
		return nil
	})
	return out, err
}

// githubMaxPages bounds how far a list is followed, at 100 a page.
const githubMaxPages = 10

func (g *githubAPI) pages(ctx context.Context, token, path string, read func(json.RawMessage) error) error {
	for page := 1; page <= githubMaxPages; page++ {
		var items []json.RawMessage
		if err := g.get(ctx, token, fmt.Sprintf("%s?per_page=100&page=%d", path, page), &items); err != nil {
			return err
		}
		raw, err := json.Marshal(items)
		if err != nil {
			return err
		}
		if err := read(raw); err != nil {
			return fmt.Errorf("reading GitHub's %s: %w", path, err)
		}
		if len(items) < 100 {
			return nil
		}
	}
	return nil
}

func (g *githubAPI) get(ctx context.Context, token, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	endpoint, _, _ := strings.Cut(path, "?")
	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("asking GitHub: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("reading GitHub's answer: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("%w: GitHub no longer accepts the token", ErrDirectoryRefused)
	case resp.StatusCode != http.StatusOK:
		// A 403 is usually the rate limit.
		return fmt.Errorf("GitHub answered %s with %s", endpoint, resp.Status)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("reading GitHub's answer to %s: %w", endpoint, err)
	}
	return nil
}
