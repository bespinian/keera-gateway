package authn

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"
)

// An LDAP provider signs people in with the password their directory holds:
// Active Directory or OpenLDAP. There is no redirect, so the panel asks for
// the password and the gateway binds as the person. See docs/sso.md.

// LDAPConfig is what an LDAP provider needs on top of the common settings.
type LDAPConfig struct {
	// BindDN and BindPassword are the service account that finds people and
	// reads them again later. A person's own bind only checks the password.
	BindDN       string
	BindPassword string
	BaseDN       string
	// UserFilter narrows who may sign in. Empty is every person, as the
	// directory's flavour defines one.
	UserFilter string
	// LoginAttributes are what a person may type as their username. Empty is
	// the flavour's.
	LoginAttributes []string
	// CAFile is a PEM bundle to trust instead of the system's roots.
	CAFile string
}

// IsLDAP reports whether this provider is a directory spoken to over LDAP,
// which its issuer says by its scheme.
func (c OIDCConfig) IsLDAP() bool {
	return strings.HasPrefix(c.IssuerURL, "ldap://") || strings.HasPrefix(c.IssuerURL, "ldaps://")
}

// validateLDAP checks an LDAP provider's issuer.
func (c OIDCConfig) validateLDAP() error {
	if u, err := url.Parse(c.IssuerURL); err != nil || u.Hostname() == "" {
		return fmt.Errorf("the issuer for the %s identity provider must be an LDAP URL, "+
			"such as ldaps://dc1.example.ch", c.Name)
	}
	return nil
}

// ErrBadCredentials is a password sign-in the directory refused. It is one
// answer for an unknown name, a wrong password and a locked account, so the
// answer does not tell anyone which accounts exist.
var ErrBadCredentials = errors.New("the username or password was not accepted")

// ErrDirectoryUnreachable is a sign-in that failed because the directory did
// not answer, which says nothing about the person.
var ErrDirectoryUnreachable = errors.New("the directory could not be reached")

// ErrUnusableEntry is a right password for an entry Keera cannot use, for a
// reason only the directory's administrators can fix. The details name the
// entry, so they go to the log and the person gets this message.
var ErrUnusableEntry = errors.New("this directory account cannot be used to sign in to Keera; " +
	"ask your administrator")

// ldapTimeout bounds every connection and request to the directory.
const ldapTimeout = 10 * time.Second

// The longest username and password a sign-in may send. Longer is not a
// mistake anyone makes, and the directory need not see it.
const (
	LDAPUsernameMax = 256
	ldapPasswordMax = 1024
)

// ldapFlavour is what differs between Active Directory and other directories.
type ldapFlavour struct {
	// subject is the attribute that names an entry for good: a rename or a
	// move to another OU changes the DN, not this.
	subject    string
	userFilter string
	login      []string
}

var (
	activeDirectory = ldapFlavour{
		subject:    "objectGUID",
		userFilter: "(&(objectCategory=person)(objectClass=user))",
		login:      []string{"sAMAccountName", "userPrincipalName", "mail"},
	}
	openLDAP = ldapFlavour{
		subject:    "entryUUID",
		userFilter: "(objectClass=inetOrgPerson)",
		login:      []string{"uid", "mail"},
	}
)

// ADCapability is in an Active Directory server's supportedCapabilities.
const ADCapability = "1.2.840.113556.1.4.800"

// adAccountDisabled is the ACCOUNTDISABLE bit of userAccountControl.
const adAccountDisabled = 0x2

// LDAPConn is the part of a connection Keera uses, so tests need no server.
type LDAPConn interface {
	Bind(username, password string) error
	Search(*ldap.SearchRequest) (*ldap.SearchResult, error)
	Close() error
}

type ldapDir struct {
	cfg     LDAPConfig
	flavour ldapFlavour
	// groups is the attribute that lists a person's groups, as DNs.
	groups string
	dial   func() (LDAPConn, error)
}

// newLDAP connects once at start-up, as discovery does for OpenID Connect, so
// a wrong address or service account stops the gateway instead of the first
// sign-in. That connection also tells the flavour apart.
func newLDAP(cfg OIDCConfig) (*OIDC, error) {
	tlsConfig, err := ldapTLS(cfg)
	if err != nil {
		return nil, err
	}
	dial := func() (LDAPConn, error) { return dialLDAP(cfg.IssuerURL, tlsConfig) }
	return NewLDAPWith(cfg, dial)
}

// NewLDAPWith is NewOIDC for a directory reached through dial. Tests use it to
// stand in for a server; see ldaptest.
func NewLDAPWith(cfg OIDCConfig, dial func() (LDAPConn, error)) (*OIDC, error) {
	d := &ldapDir{cfg: cfg.LDAP, groups: cmp.Or(cfg.GroupsClaim, "memberOf"), dial: dial}
	// Checked against both flavours before the flavour is known. A filter that
	// does not compile would otherwise fail every sign-in, and its error
	// would quote what the person typed.
	for _, f := range []ldapFlavour{activeDirectory, openLDAP} {
		d.flavour = f
		if _, err := ldap.CompileFilter(d.loginFilter("x")); err != nil {
			return nil, fmt.Errorf("the user filter or login attributes of the %s directory "+
				"do not make an LDAP filter: %w", cfg.Name, err)
		}
	}
	conn, err := d.service()
	if err != nil {
		return nil, fmt.Errorf("connecting to the %s directory at %s: %w", cfg.Name, cfg.IssuerURL, err)
	}
	defer func() { _ = conn.Close() }()
	d.flavour, err = flavourOf(conn)
	if err != nil {
		return nil, fmt.Errorf("reading the %s directory at %s: %w", cfg.Name, cfg.IssuerURL, err)
	}
	// Checked here because later a missing base looks like a directory error,
	// and every sign-in fails with it.
	if _, err := conn.Search(&ldap.SearchRequest{
		BaseDN: cfg.LDAP.BaseDN, Scope: ldap.ScopeBaseObject, Filter: "(objectClass=*)",
		Attributes: []string{"1.1"}, TimeLimit: int(ldapTimeout.Seconds()),
	}); err != nil {
		return nil, fmt.Errorf("reading the base DN %s of the %s directory: %w", cfg.LDAP.BaseDN, cfg.Name, err)
	}
	// Group DNs are compared in one spelling, so "CN=Admins, OU=Groups" in the
	// configuration matches "cn=admins,ou=groups" from the directory.
	cfg.Mapping.AdminGroups = normalDNs(cfg.Mapping.AdminGroups)
	cfg.Mapping.OperatorGroups = normalDNs(cfg.Mapping.OperatorGroups)
	return &OIDC{cfg: cfg, ldap: d}, nil
}

// ldapTLS is the TLS every connection uses. Plaintext is never offered: the
// connection carries passwords.
func ldapTLS(cfg OIDCConfig) (*tls.Config, error) {
	u, err := url.Parse(cfg.IssuerURL)
	if err != nil {
		return nil, err
	}
	// Set for StartTLS too, which, unlike ldaps://, does not fill it in.
	c := &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}
	if cfg.LDAP.CAFile != "" {
		pem, err := os.ReadFile(cfg.LDAP.CAFile)
		if err != nil {
			return nil, fmt.Errorf("reading the CA file of the %s directory: %w", cfg.Name, err)
		}
		c.RootCAs = x509.NewCertPool()
		if !c.RootCAs.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("the CA file of the %s directory holds no PEM certificate", cfg.Name)
		}
	}
	return c, nil
}

// dialLDAP opens one connection: TLS from the start for ldaps://, StartTLS
// before anything else for ldap://.
func dialLDAP(addr string, tlsConfig *tls.Config) (LDAPConn, error) {
	conn, err := ldap.DialURL(addr,
		ldap.DialWithDialer(&net.Dialer{Timeout: ldapTimeout}),
		ldap.DialWithTLSConfig(tlsConfig))
	if err != nil {
		return nil, err
	}
	conn.SetTimeout(ldapTimeout)
	if strings.HasPrefix(addr, "ldap://") {
		if err := conn.StartTLS(tlsConfig); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("starting TLS: %w", err)
		}
	}
	return conn, nil
}

// service is a connection bound as the service account.
func (d *ldapDir) service() (LDAPConn, error) {
	conn, err := d.dial()
	if err != nil {
		return nil, err
	}
	if err := conn.Bind(d.cfg.BindDN, d.cfg.BindPassword); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("binding as the service account: %w", err)
	}
	return conn, nil
}

// flavourOf reads the root DSE, which says whether this is Active Directory.
func flavourOf(conn LDAPConn) (ldapFlavour, error) {
	res, err := conn.Search(&ldap.SearchRequest{
		Scope: ldap.ScopeBaseObject, Filter: "(objectClass=*)",
		Attributes: []string{"supportedCapabilities"}, TimeLimit: int(ldapTimeout.Seconds()),
	})
	if err != nil {
		return ldapFlavour{}, fmt.Errorf("reading the root DSE: %w", err)
	}
	for _, e := range res.Entries {
		if slices.Contains(e.GetEqualFoldAttributeValues("supportedCapabilities"), ADCapability) {
			return activeDirectory, nil
		}
	}
	return openLDAP, nil
}

// Password reports whether this provider signs people in with a password typed
// into the panel, rather than by sending the browser to the directory.
func (o *OIDC) Password() bool { return o.ldap != nil }

// Authenticate checks a username and password against the directory and
// returns who they belong to.
//
// admit, if set, sees the entry's DN before the password is tried and may stop
// the sign-in with its error. That is where attempts are counted per account:
// one account can be typed as several usernames.
func (o *OIDC) Authenticate(_ context.Context, username, password string,
	admit func(dn string) error) (Identity, error) {
	d := o.ldap
	username = strings.TrimSpace(username)
	// An empty password is an unauthenticated bind, which a directory accepts
	// for any DN. It must never reach the bind.
	if username == "" || password == "" ||
		len(username) > LDAPUsernameMax || len(password) > ldapPasswordMax {
		return Identity{}, ErrBadCredentials
	}
	entry, err := d.lookup(d.loginFilter(username))
	if err != nil {
		return Identity{}, err
	}
	if admit != nil {
		if err := admit(normalDN(entry.DN)); err != nil {
			return Identity{}, err
		}
	}
	// The person's bind gets a connection of its own: a bind changes who a
	// connection acts as.
	user, err := d.dial()
	if err != nil {
		return Identity{}, errors.Join(ErrDirectoryUnreachable, err)
	}
	defer func() { _ = user.Close() }()
	if err := user.Bind(entry.DN, password); err != nil {
		if _, answered := errors.AsType[*ldap.Error](err); !answered ||
			ldap.IsErrorWithCode(err, ldap.ErrorNetwork) {
			return Identity{}, errors.Join(ErrDirectoryUnreachable, err)
		}
		// The directory answered no. Active Directory says why in the
		// message, such as "data 775" for a locked account. That goes to the
		// log, not to the person.
		return Identity{}, errors.Join(ErrBadCredentials, err)
	}
	return o.ldapIdentity(entry)
}

// recheckLDAP reads a person again by their subject, which the sign-in kept as
// the "refresh token". The user filter applies again, so someone taken out of a
// group it requires is refused.
func (o *OIDC) recheckLDAP(_ context.Context, subject string) (Identity, bool, error) {
	d := o.ldap
	filter, err := d.subjectFilter(subject)
	if err != nil {
		return Identity{}, false, fmt.Errorf("%w: %w", ErrDirectoryRefused, err)
	}
	entry, err := d.lookup(filter)
	if errors.Is(err, ErrBadCredentials) {
		return Identity{}, false, fmt.Errorf("%w: %w", ErrDirectoryRefused, err)
	}
	if err != nil {
		return Identity{}, false, err
	}
	id, err := o.ldapIdentity(entry)
	if err != nil {
		return id, false, fmt.Errorf("%w: %w", ErrDirectoryRefused, err)
	}
	return id, true, nil
}

// lookup finds the one entry a filter matches, as the service account. None,
// or more than one, is ErrBadCredentials: for a username, both mean it names
// nobody Keera can use. The errors leave out the filter, which holds what was
// typed: a password typed as the username would otherwise reach the log.
func (d *ldapDir) lookup(filter string) (*ldap.Entry, error) {
	conn, err := d.service()
	if err != nil {
		return nil, errors.Join(ErrDirectoryUnreachable, err)
	}
	defer func() { _ = conn.Close() }()
	attrs := []string{d.flavour.subject, "mail", "displayName", "givenName", "sn", "cn", d.groups}
	if d.flavour.subject == activeDirectory.subject {
		attrs = append(attrs, "userAccountControl")
	}
	res, err := conn.Search(&ldap.SearchRequest{
		BaseDN: d.cfg.BaseDN, Scope: ldap.ScopeWholeSubtree, DerefAliases: ldap.NeverDerefAliases,
		// Two is enough to know a name is ambiguous.
		SizeLimit: 2, TimeLimit: int(ldapTimeout.Seconds()),
		Filter: filter, Attributes: attrs,
	})
	switch {
	case ldap.IsErrorWithCode(err, ldap.LDAPResultSizeLimitExceeded):
		return nil, errors.Join(ErrBadCredentials, errors.New("more than one directory entry matches"))
	case ldap.IsErrorWithCode(err, ldap.ErrorFilterCompile):
		// Its message quotes the filter. The start-up check makes this rare.
		return nil, errors.Join(ErrDirectoryUnreachable, errors.New("the search filter does not compile"))
	case err != nil:
		return nil, errors.Join(ErrDirectoryUnreachable, err)
	case len(res.Entries) == 0:
		return nil, errors.Join(ErrBadCredentials, errors.New("no directory entry matches"))
	case len(res.Entries) > 1:
		return nil, errors.Join(ErrBadCredentials, errors.New("more than one directory entry matches"))
	}
	return res.Entries[0], nil
}

// loginFilter finds the person a username names.
func (d *ldapDir) loginFilter(username string) string {
	attrs := d.cfg.LoginAttributes
	if len(attrs) == 0 {
		attrs = d.flavour.login
	}
	var b strings.Builder
	b.WriteString("(&" + d.userFilter() + "(|")
	for _, a := range attrs {
		b.WriteString("(" + a + "=" + ldap.EscapeFilter(username) + ")")
	}
	b.WriteString("))")
	return b.String()
}

// subjectFilter finds the person a stored subject names.
func (d *ldapDir) subjectFilter(subject string) (string, error) {
	value := ldap.EscapeFilter(subject)
	if d.flavour.subject == activeDirectory.subject {
		raw, err := guidBytes(subject)
		if err != nil {
			return "", err
		}
		value = escapeBytes(raw)
	}
	return "(&" + d.userFilter() + "(" + d.flavour.subject + "=" + value + "))", nil
}

func (d *ldapDir) userFilter() string {
	f := cmp.Or(d.cfg.UserFilter, d.flavour.userFilter)
	if !strings.HasPrefix(f, "(") {
		f = "(" + f + ")"
	}
	return f
}

// ldapIdentity reads and checks an entry the way identity does an id_token.
func (o *OIDC) ldapIdentity(e *ldap.Entry) (Identity, error) {
	d := o.ldap
	subject := d.subjectOf(e)
	if subject == "" {
		return Identity{}, fmt.Errorf("%w: the directory entry %s has no %s",
			ErrUnusableEntry, e.DN, d.flavour.subject)
	}
	id := Identity{
		Provider: o.cfg.Name,
		Subject:  subject,
		Email:    strings.ToLower(strings.TrimSpace(e.GetEqualFoldAttributeValue("mail"))),
		Name:     entryName(e),
		Groups:   normalDNs(e.GetEqualFoldAttributeValues(d.groups)),
		// Not a secret: what to look the person up by when the gateway asks
		// again. It is kept where a refresh token would be.
		RefreshToken: subject,
	}
	if uac := e.GetEqualFoldAttributeValue("userAccountControl"); uac != "" {
		if n, err := strconv.ParseInt(uac, 10, 64); err == nil && n&adAccountDisabled != 0 {
			return id, errors.New("the account is disabled in the directory")
		}
	}
	if id.Email == "" {
		return id, fmt.Errorf("%w: the directory entry %s has no mail attribute, and "+
			"Keera needs an address to place the person with", ErrUnusableEntry, e.DN)
	}
	return id, o.checkIdentity(id, nil, d.groups)
}

// subjectOf is the entry's lasting name, as text.
func (d *ldapDir) subjectOf(e *ldap.Entry) string {
	if d.flavour.subject == activeDirectory.subject {
		return guidString(e.GetEqualFoldRawAttributeValue("objectGUID"))
	}
	return strings.ToLower(e.GetEqualFoldAttributeValue(d.flavour.subject))
}

// entryName is the person's display name, else their given and family names,
// else their common name.
func entryName(e *ldap.Entry) string {
	if n := strings.TrimSpace(e.GetEqualFoldAttributeValue("displayName")); n != "" {
		return n
	}
	if n := strings.TrimSpace(e.GetEqualFoldAttributeValue("givenName") + " " +
		e.GetEqualFoldAttributeValue("sn")); n != "" {
		return n
	}
	return strings.TrimSpace(e.GetEqualFoldAttributeValue("cn"))
}

// normalDNs spells DNs one way, so they compare as text. A value that does not
// parse is only lower-cased: it then matches nothing a directory sends.
func normalDNs(dns []string) []string {
	out := make([]string, 0, len(dns))
	for _, s := range dns {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		out = append(out, normalDN(s))
	}
	return out
}

func normalDN(s string) string {
	if dn, err := ldap.ParseDN(s); err == nil {
		s = dn.String()
	}
	return strings.ToLower(s)
}

// guidString writes an objectGUID the way Windows shows it. The first three
// fields are stored little-endian.
func guidString(b []byte) string {
	if len(b) != 16 {
		return ""
	}
	order := []byte{b[3], b[2], b[1], b[0], b[5], b[4], b[7], b[6]}
	h := hex.EncodeToString(append(order, b[8:]...))
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// guidBytes reverses guidString.
func guidBytes(s string) ([]byte, error) {
	h, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	if err != nil || len(h) != 16 {
		return nil, fmt.Errorf("%q is not an objectGUID", s)
	}
	return append([]byte{h[3], h[2], h[1], h[0], h[5], h[4], h[7], h[6]}, h[8:]...), nil
}

// escapeBytes writes a binary value for a filter, every byte escaped.
func escapeBytes(b []byte) string {
	var s strings.Builder
	for _, c := range b {
		fmt.Fprintf(&s, `\%02x`, c)
	}
	return s.String()
}
