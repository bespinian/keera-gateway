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

// GitHubIssuer is the issuer that makes a provider GitHub's. GitHub speaks
// OAuth but not OpenID Connect: there is no discovery and no id_token, so
// Keera asks GitHub's API who signed in.
const GitHubIssuer = "https://github.com"

// IsGitHub reports whether this configuration is for GitHub.
func (c OIDCConfig) IsGitHub() bool { return isGitHub(c.IssuerURL) }

func isGitHub(issuer string) bool {
	u, err := url.Parse(issuer)
	return err == nil && u.Host == "github.com"
}

// githubAPI is where a GitHub provider reads people from.
type githubAPI struct {
	base   string
	client *http.Client
}

// newGitHub sets up a GitHub provider. web and api are GitHub's addresses,
// which tests replace.
func newGitHub(cfg OIDCConfig, client *http.Client, web, api string) *OIDC {
	if client == nil {
		client = http.DefaultClient
	}
	scopes := cfg.Scopes
	if len(scopes) == 0 {
		// Only the addresses, since the profile is public. Organisations and
		// teams only where they decide a role, so nobody is asked for more.
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

// IsGitHub reports whether this provider is GitHub's.
func (o *OIDC) IsGitHub() bool { return o.github != nil }

// exchangeGitHub turns an authorization code into an identity.
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
	// An OAuth app's token does not expire and comes with no refresh token,
	// so the token itself is what asks GitHub again.
	id.RefreshToken = token.AccessToken
	return id, o.checkGitHub(id)
}

// recheckGitHub asks GitHub about someone again with the token they signed
// in with. GitHub answers 401 once the person revoked Keera's access or the
// account is gone.
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

// checkGitHub refuses an identity Keera cannot place, as checkIdentity does
// for an id_token. Every address githubIdentity picks is one GitHub verified.
func (o *OIDC) checkGitHub(id Identity) error {
	if id.Email == "" {
		return errors.New("this GitHub account has no verified email address Keera " +
			"can use; verify one at https://github.com/settings/emails")
	}
	return o.checkIdentity(id, map[string]any{"email_verified": true}, "")
}

// githubIdentity reads the person a token belongs to. A 401 is
// ErrDirectoryRefused; any other failure says nothing about the person.
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
	// The id, not the login: a login can be renamed and then taken by
	// somebody else.
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

// pickEmail chooses the address to sign someone in with: the primary one if
// this provider may vouch for it, else the first other verified one it may.
// People often keep a private primary address and add their work one. Failing
// both, it is the primary address, so the refusal can name it.
func (o *OIDC) pickEmail(emails []githubEmail) string {
	var usable []string
	primary := ""
	for _, e := range emails {
		addr := strings.ToLower(strings.TrimSpace(e.Email))
		// The noreply address forwards nowhere, so it names nobody.
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

// groups lists the organisations someone is in, by login, and their teams, as
// "org/team". Organisation names are unique on GitHub, so nobody else can
// create a group of the same name, as a directory admin elsewhere could.
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

// githubMaxPages bounds how far a list is followed. At 100 a page, nobody is in
// that many organisations or teams.
const githubMaxPages = 10

// pages reads every page of a list, handing each to read.
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

// get calls GitHub's API as the person the token belongs to.
func (g *githubAPI) get(ctx context.Context, token, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
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
		// A 403 is usually the rate limit, which says nothing about the person.
		return fmt.Errorf("GitHub answered %s with %s", strings.SplitN(path, "?", 2)[0], resp.Status)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("reading GitHub's answer to %s: %w", strings.SplitN(path, "?", 2)[0], err)
	}
	return nil
}
