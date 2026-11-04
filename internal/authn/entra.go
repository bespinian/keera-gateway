package authn

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// entraAnyTenantRE matches Entra's issuers for more than one tenant: any
// work or school account, any account at all, or personal accounts only.
var entraAnyTenantRE = regexp.MustCompile(`^/(common|organizations|consumers)/v2\.0/?$`)

// entraTenantPlaceholder is what Entra's multi-tenant discovery puts where
// each token's issuer has its tenant ID.
const entraTenantPlaceholder = "{tenantid}"

// entraPersonalTenant is the tenant every personal Microsoft account is in.
const entraPersonalTenant = "9188040d-6c67-4c5b-b112-36a304b66dad"

// tenantIDRE keeps a tid claim from changing the issuer beyond its tenant.
var tenantIDRE = regexp.MustCompile(`^[0-9a-fA-F-]{36}$`)

// IsEntraAnyTenant reports whether the issuer admits accounts from every
// Entra tenant, such as https://login.microsoftonline.com/common/v2.0. Each
// tenant's administrator sets their own users' addresses, groups and app
// roles.
func (c OIDCConfig) IsEntraAnyTenant() bool {
	u, err := url.Parse(c.IssuerURL)
	return err == nil && u.Host != "" && entraAnyTenantRE.MatchString(u.Path)
}

// entraIssuerTemplate is the issuer Entra's multi-tenant discovery reports,
// such as https://login.microsoftonline.com/{tenantid}/v2.0.
func entraIssuerTemplate(issuer string) string {
	u, _ := url.Parse(issuer)
	return u.Scheme + "://" + u.Host + "/" + entraTenantPlaceholder + "/v2.0"
}

// checkEntraIssuer does the issuer check the verifier skips for a
// multi-tenant issuer: the token's issuer must be the template with the
// token's own tenant in it, as Microsoft asks.
func checkEntraIssuer(template string, idToken *oidc.IDToken) error {
	var claims struct {
		TenantID string `json:"tid"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return fmt.Errorf("reading the id_token claims: %w", err)
	}
	if !tenantIDRE.MatchString(claims.TenantID) {
		return errors.New("the id_token names no tenant in its tid claim")
	}
	want := strings.Replace(template, entraTenantPlaceholder, claims.TenantID, 1)
	if idToken.Issuer != want {
		return fmt.Errorf("the id_token was issued by %s, not by %s", idToken.Issuer, want)
	}
	return nil
}

// isEntraPersonal reports whether a token is for a personal Microsoft
// account. These send no xms_edov, but Microsoft proves the address with a
// code at sign-up, and counts it as verified in its own definition of
// xms_edov. Only the issuer check makes tid trustworthy.
func isEntraPersonal(claims map[string]any) bool {
	return stringClaim(claims, "tid") == entraPersonalTenant
}
