// Package ldaptest is a directory that answers the LDAP requests Keera makes,
// for tests of password sign-in and of asking the directory again. It runs in
// memory, behind authn.NewLDAPWith, rather than on a socket.
//
// A filter matches an entry when it names one of the entry's values exactly,
// escaped as a filter needs. So a username that was not escaped matches
// nothing, and the test sees it.
package ldaptest

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/go-ldap/ldap/v3"

	"github.com/bespinian/keera-gateway/internal/authn"
)

// The service account every Directory accepts.
const (
	BindDN       = "cn=keera-svc,ou=service,dc=corp,dc=example,dc=ch"
	BindPassword = "service-secret"
	BaseDN       = "dc=corp,dc=example,dc=ch"
)

// Directory holds the people. Set its fields before the first request, and
// through Change after.
type Directory struct {
	// AD makes it answer as Active Directory.
	AD      bool
	Entries []*ldap.Entry
	// Passwords maps a DN to the password that binds as it.
	Passwords map[string]string
	// Down refuses every connection, like a directory that is unreachable.
	Down bool

	mu      sync.Mutex
	binds   []string
	filters []string
}

// Dial is the dial function for authn.NewLDAPWith.
func (d *Directory) Dial() (authn.LDAPConn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Down {
		return nil, ldap.NewError(ldap.ErrorNetwork, errors.New("connection refused"))
	}
	return &conn{dir: d}, nil
}

// Change edits the directory while a server may be using it.
func (d *Directory) Change(f func(d *Directory)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f(d)
}

// Binds are the DNs every bind so far was tried with.
func (d *Directory) Binds() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.binds...)
}

// Filters are the search filters so far, the root DSE's left out.
func (d *Directory) Filters() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.filters...)
}

// Reset forgets the binds and filters so far.
func (d *Directory) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.binds, d.filters = nil, nil
}

// Config is a provider for this directory, which vouches for corp.example.ch.
func Config(name string) authn.OIDCConfig {
	return authn.OIDCConfig{
		Name: name, IssuerURL: "ldaps://dc1.corp.example.ch",
		RedirectURL: "https://keera.example.ch/control/auth/callback",
		Domains:     []string{"corp.example.ch"},
		LDAP:        authn.LDAPConfig{BindDN: BindDN, BindPassword: BindPassword, BaseDN: BaseDN},
		Mapping:     authn.RoleMapping{Default: authn.RoleMember},
	}
}

// New is the provider cfg describes, in front of d.
func New(t testing.TB, cfg authn.OIDCConfig, d *Directory) *authn.OIDC {
	t.Helper()
	o, err := authn.NewLDAPWith(cfg, d.Dial)
	if err != nil {
		t.Fatalf("NewLDAPWith: %v", err)
	}
	return o
}

// Person is an OpenLDAP inetOrgPerson.
func Person(uid, mail, uuid string, groups ...string) *ldap.Entry {
	return ldap.NewEntry("uid="+uid+",ou=people,"+BaseDN, map[string][]string{
		"uid": {uid}, "mail": {mail}, "entryUUID": {uuid},
		"givenName": {strings.ToUpper(uid[:1]) + uid[1:]}, "sn": {"Example"},
		"memberOf": groups,
	})
}

// ADUser is an Active Directory user. uac is its userAccountControl: "512" is
// an enabled account, "514" a disabled one.
func ADUser(sam, mail string, guid []byte, uac string, groups ...string) *ldap.Entry {
	e := ldap.NewEntry("CN="+sam+",OU=Staff,DC=corp,DC=example,DC=ch", map[string][]string{
		"sAMAccountName": {sam}, "userPrincipalName": {sam + "@corp.example.ch"},
		"mail": {mail}, "displayName": {sam}, "userAccountControl": {uac},
		"memberOf": groups,
	})
	e.Attributes = append(e.Attributes, &ldap.EntryAttribute{
		Name: "objectGUID", Values: []string{string(guid)}, ByteValues: [][]byte{guid},
	})
	return e
}

type conn struct {
	dir   *Directory
	bound string
}

func (c *conn) Bind(dn, password string) error {
	c.dir.mu.Lock()
	defer c.dir.mu.Unlock()
	c.dir.binds = append(c.dir.binds, dn)
	if (dn == BindDN && password == BindPassword) ||
		(password != "" && c.dir.Passwords[dn] == password) {
		c.bound = dn
		return nil
	}
	return ldap.NewError(ldap.LDAPResultInvalidCredentials,
		errors.New("80090308: LdapErr: DSID-0C09044E, comment: AcceptSecurityContext error, data 52e"))
}

func (c *conn) Search(req *ldap.SearchRequest) (*ldap.SearchResult, error) {
	c.dir.mu.Lock()
	defer c.dir.mu.Unlock()
	if req.Scope == ldap.ScopeBaseObject && req.BaseDN == "" {
		root := ldap.NewEntry("", nil)
		if c.dir.AD {
			root = ldap.NewEntry("", map[string][]string{"supportedCapabilities": {authn.ADCapability}})
		}
		return &ldap.SearchResult{Entries: []*ldap.Entry{root}}, nil
	}
	if req.Scope == ldap.ScopeBaseObject {
		if req.BaseDN != BaseDN {
			return nil, ldap.NewError(ldap.LDAPResultNoSuchObject, errors.New("no such object"))
		}
		return &ldap.SearchResult{Entries: []*ldap.Entry{ldap.NewEntry(BaseDN, nil)}}, nil
	}
	// Only the service account reads people.
	if c.bound != BindDN {
		return nil, ldap.NewError(ldap.LDAPResultInsufficientAccessRights, errors.New("not bound"))
	}
	c.dir.filters = append(c.dir.filters, req.Filter)
	var out []*ldap.Entry
	for _, e := range c.dir.Entries {
		if matches(req.Filter, e) {
			out = append(out, e)
		}
	}
	if req.SizeLimit > 0 && len(out) > req.SizeLimit {
		return &ldap.SearchResult{Entries: out[:req.SizeLimit]},
			ldap.NewError(ldap.LDAPResultSizeLimitExceeded, errors.New("size limit exceeded"))
	}
	return &ldap.SearchResult{Entries: out}, nil
}

func (c *conn) Close() error { return nil }

func matches(filter string, e *ldap.Entry) bool {
	for _, a := range e.Attributes {
		for i, v := range a.Values {
			want, in := "("+a.Name+"="+ldap.EscapeFilter(v)+")", filter
			switch a.Name {
			case "objectGUID":
				var b strings.Builder
				for _, c := range a.ByteValues[i] {
					fmt.Fprintf(&b, `\%02x`, c)
				}
				want = "(objectGUID=" + b.String() + ")"
			case "entryUUID":
				// uuidMatch compares the UUID, not its spelling.
				want, in = strings.ToLower(want), strings.ToLower(filter)
			}
			if strings.Contains(in, want) {
				return true
			}
		}
	}
	return false
}
