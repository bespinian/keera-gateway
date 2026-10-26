package authn_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/go-ldap/ldap/v3"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/authn/ldaptest"
)

const aliceUUID = "5B0F4C2E-1D3A-4F6B-9C8D-7E6F5A4B3C2D"

// adGUID is an objectGUID as AD stores it, and guidText as Windows shows it.
var adGUID = []byte{
	0x78, 0x56, 0x34, 0x12, 0xbc, 0x9a, 0xf0, 0xde,
	0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef,
}

const guidText = "12345678-9abc-def0-0123-456789abcdef"

func TestLDAPSignInOpenLDAP(t *testing.T) {
	alice := ldaptest.Person("alice", "Alice@Corp.Example.ch", aliceUUID)
	dir := &ldaptest.Directory{
		Entries:   []*ldap.Entry{alice},
		Passwords: map[string]string{alice.DN: "right"},
	}
	o := ldaptest.New(t, ldaptest.Config("corp"), dir)
	if !o.Password() {
		t.Fatal("an LDAP provider asks for a password")
	}

	id, err := o.Authenticate(context.Background(), " alice ", "right", nil)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	want := strings.ToLower(aliceUUID)
	if id.Subject != want || id.RefreshToken != want || id.ExternalID() != "corp:"+want {
		t.Errorf("subject = %q, token %q, want %q for both", id.Subject, id.RefreshToken, want)
	}
	if id.Email != "alice@corp.example.ch" || id.Name != "Alice Example" {
		t.Errorf("identity = %+v", id)
	}
	if binds := dir.Binds(); binds[len(binds)-1] != alice.DN {
		t.Errorf("last bind as %q, want the person's DN", binds[len(binds)-1])
	}
	if f := dir.Filters()[0]; f != "(&(objectClass=inetOrgPerson)(|(uid=alice)(mail=alice)))" {
		t.Errorf("filter = %s", f)
	}
	if o.Mapping().RoleFor(id.Email, id.Groups) != authn.RoleMember {
		t.Error("someone in no admin group is a member")
	}
}

func TestLDAPSignInActiveDirectory(t *testing.T) {
	admins := "CN=Keera Admins,OU=Groups,DC=corp,DC=example,DC=ch"
	ada := ldaptest.ADUser("ada", "ada@corp.example.ch", adGUID, "512", admins)
	dir := &ldaptest.Directory{
		AD: true, Entries: []*ldap.Entry{ada},
		Passwords: map[string]string{ada.DN: "right"},
	}
	cfg := ldaptest.Config("corp")
	// Spelled differently from the directory on purpose.
	cfg.Mapping.AdminGroups = []string{"cn=keera admins, ou=groups, dc=corp, dc=example, dc=ch"}
	o := ldaptest.New(t, cfg, dir)

	id, err := o.Authenticate(context.Background(), "ada@corp.example.ch", "right", nil)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.Subject != guidText {
		t.Errorf("subject = %q, want %q", id.Subject, guidText)
	}
	if f := dir.Filters()[0]; !strings.Contains(f, "(objectCategory=person)") ||
		!strings.Contains(f, "(userPrincipalName=ada@corp.example.ch)") {
		t.Errorf("filter = %s", f)
	}
	if role := o.Mapping().RoleFor(id.Email, id.Groups); role != authn.RoleAdmin {
		t.Errorf("role = %s, want admin from a group spelled another way", role)
	}

	// AD refuses a disabled account's bind itself, but a re-check only reads.
	dir.Entries = []*ldap.Entry{ldaptest.ADUser("ada", "ada@corp.example.ch", adGUID, "514", admins)}
	if _, _, err := o.Recheck(context.Background(), guidText); !errors.Is(err, authn.ErrDirectoryRefused) {
		t.Errorf("Recheck of a disabled account = %v, want ErrDirectoryRefused", err)
	}
}

func TestLDAPRefusals(t *testing.T) {
	alice := ldaptest.Person("alice", "shared@corp.example.ch", aliceUUID)
	bob := ldaptest.Person("bob", "shared@corp.example.ch", "11111111-2222-3333-4444-555555555555")
	dir := &ldaptest.Directory{
		Entries:   []*ldap.Entry{alice, bob},
		Passwords: map[string]string{alice.DN: "right"},
	}
	o := ldaptest.New(t, ldaptest.Config("corp"), dir)
	ctx := context.Background()

	for _, tc := range []struct{ name, user, pass string }{
		{"wrong password", "alice", "wrong"},
		{"unknown name", "carol", "right"},
		{"two entries match", "shared@corp.example.ch", "right"},
		{"filter injection", "*)(uid=*", "right"},
		{"too long", strings.Repeat("a", 300), "right"},
	} {
		if _, err := o.Authenticate(ctx, tc.user, tc.pass, nil); !errors.Is(err, authn.ErrBadCredentials) {
			t.Errorf("%s: err = %v, want ErrBadCredentials", tc.name, err)
		}
	}
	if !slices.ContainsFunc(dir.Filters(), func(f string) bool {
		return strings.Contains(f, `(uid=\2a\29\28uid=\2a)`)
	}) {
		t.Errorf("the username was not escaped: %q", dir.Filters())
	}

	// An empty password is an anonymous bind, which a directory accepts.
	dir.Reset()
	if _, err := o.Authenticate(ctx, "alice", "", nil); !errors.Is(err, authn.ErrBadCredentials) {
		t.Errorf("empty password: err = %v", err)
	}
	if binds := dir.Binds(); len(binds) != 0 {
		t.Errorf("an empty password reached the directory: %q", binds)
	}

	// The password is right, but the address is another domain's.
	dir.Entries = []*ldap.Entry{ldaptest.Person("alice", "alice@elsewhere.ch", aliceUUID)}
	_, err := o.Authenticate(ctx, "alice", "right", nil)
	if err == nil || errors.Is(err, authn.ErrBadCredentials) || !strings.Contains(err.Error(), "elsewhere.ch") {
		t.Errorf("foreign domain: err = %v, want a refusal that names it", err)
	}

	// A password typed as the username must not reach the log.
	if _, err := o.Authenticate(ctx, "S3cret!pass", "S3cret!pass", nil); err == nil ||
		strings.Contains(err.Error(), "S3cret") {
		t.Errorf("unknown name: err = %v, want one that leaves out what was typed", err)
	}

	// The entry has no address. The person is not told its DN.
	dir.Entries = []*ldap.Entry{ldaptest.Person("alice", "", aliceUUID)}
	_, err = o.Authenticate(ctx, "alice", "right", nil)
	if !errors.Is(err, authn.ErrUnusableEntry) {
		t.Errorf("no mail: err = %v, want ErrUnusableEntry", err)
	}

	dir.Down = true
	if _, err := o.Authenticate(ctx, "alice", "right", nil); !errors.Is(err, authn.ErrDirectoryUnreachable) {
		t.Errorf("directory down: err = %v, want ErrDirectoryUnreachable", err)
	}
}

func TestLDAPAdmitSeesTheAccountBeforeThePassword(t *testing.T) {
	alice := ldaptest.Person("alice", "alice@corp.example.ch", aliceUUID)
	dir := &ldaptest.Directory{
		Entries:   []*ldap.Entry{alice},
		Passwords: map[string]string{alice.DN: "right"},
	}
	o := ldaptest.New(t, ldaptest.Config("corp"), dir)
	dir.Reset()

	stop := errors.New("too many attempts")
	var seen []string
	admit := func(dn string) error { seen = append(seen, dn); return stop }
	// Two usernames for one account are one account to admit.
	for _, user := range []string{"alice", "alice@corp.example.ch"} {
		if _, err := o.Authenticate(context.Background(), user, "right", admit); !errors.Is(err, stop) {
			t.Errorf("%s: err = %v, want admit's", user, err)
		}
	}
	if len(seen) != 2 || seen[0] != seen[1] || seen[0] != strings.ToLower(alice.DN) {
		t.Errorf("admit saw %q, want the one DN twice", seen)
	}
	for _, b := range dir.Binds() {
		if b == alice.DN {
			t.Error("the password was tried after admit refused")
		}
	}
}

func TestLDAPRecheck(t *testing.T) {
	dir := &ldaptest.Directory{Entries: []*ldap.Entry{
		ldaptest.Person("alice", "alice@corp.example.ch", aliceUUID),
	}}
	o := ldaptest.New(t, ldaptest.Config("corp"), dir)
	ctx := context.Background()
	subject := strings.ToLower(aliceUUID)

	id, fresh, err := o.Recheck(ctx, subject)
	if err != nil || !fresh || id.Subject != subject || id.RefreshToken != subject {
		t.Fatalf("Recheck = %+v, %v, %v", id, fresh, err)
	}

	dir.Down = true
	if _, _, err := o.Recheck(ctx, subject); err == nil || errors.Is(err, authn.ErrDirectoryRefused) {
		t.Errorf("directory down: err = %v, want an error that keeps the sign-in", err)
	}
	dir.Down = false

	dir.Entries = nil
	if _, _, err := o.Recheck(ctx, subject); !errors.Is(err, authn.ErrDirectoryRefused) {
		t.Errorf("entry gone: err = %v, want ErrDirectoryRefused", err)
	}
}

func TestLDAPStart(t *testing.T) {
	cfg := ldaptest.Config("corp")
	if _, err := authn.NewLDAPWith(cfg, (&ldaptest.Directory{Down: true}).Dial); err == nil {
		t.Error("an unreachable directory must stop the start")
	}
	cfg.LDAP.BaseDN = "dc=elsewhere,dc=ch"
	if _, err := authn.NewLDAPWith(cfg, (&ldaptest.Directory{}).Dial); err == nil {
		t.Error("a base DN that does not exist must stop the start")
	}
	cfg.LDAP.BindPassword = "wrong"
	if _, err := authn.NewLDAPWith(cfg, (&ldaptest.Directory{}).Dial); err == nil {
		t.Error("a wrong service password must stop the start")
	}
	cfg = ldaptest.Config("corp")
	cfg.LDAP.UserFilter = "(objectClass=inetOrgPerson))"
	if _, err := authn.NewLDAPWith(cfg, (&ldaptest.Directory{}).Dial); err == nil {
		t.Error("a user filter that does not compile must stop the start")
	}
}

func TestLDAPValidate(t *testing.T) {
	cfg := ldaptest.Config("corp")
	if !cfg.Configured() || !cfg.IsLDAP() {
		t.Fatal("an ldaps:// issuer is a configured LDAP provider without a client ID")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	cfg.IssuerURL = "ldaps://"
	if err := cfg.Validate(); err == nil {
		t.Error("Validate accepted an issuer with no host")
	}
}
