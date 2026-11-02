package control

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/policy"
)

func TestDisablingAPersonTurnsOffEverythingTheyHold(t *testing.T) {
	tn := twoTenants(t)
	carol := &authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleAdmin, OrgID: "org_a", UserID: "user_carol",
	}
	alice := &authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleMember, OrgID: "org_a", UserID: "user_alice",
	}
	disable := func(p *authn.Principal, who string) int {
		return tn.call(tn.srv.disableUser, p, http.MethodPost, "/v1/users/"+who+"/disable", "").Code
	}

	// Only an administrator, only inside their organisation, and not themselves.
	if code := disable(alice, "user_bob"); code != http.StatusForbidden {
		t.Errorf("a member disabling a colleague = %d, want 403", code)
	}
	if code := disable(carol, "user_dave"); code == http.StatusOK {
		t.Error("an administrator disabled somebody in another organisation")
	}
	if code := disable(carol, "user_carol"); code != http.StatusForbidden {
		t.Errorf("disabling yourself = %d, want 403", code)
	}

	if code := disable(carol, "user_alice"); code != http.StatusOK {
		t.Fatalf("disabling alice = %d, want 200", code)
	}
	if _, err := tn.srv.st.LookupKey(tn.ctx, []byte("hash-of-key_alice")); !errors.Is(err, policy.ErrKeyRevoked) {
		t.Errorf("alice's key = %v, want revoked", err)
	}
	if _, err := tn.srv.st.LookupKey(tn.ctx, []byte("hash-of-key_bob")); err != nil {
		t.Errorf("bob's key stopped working: %v", err)
	}

	// Nobody can hand her a new one while she is disabled.
	w := tn.call(tn.srv.createKey, carol, http.MethodPost, "/v1/keys",
		`{"org_id":"org_a","user_id":"user_alice","name":"new laptop"}`)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "disabled") {
		t.Errorf("a key for a disabled person = %d %s, want 403 saying why", w.Code, w.Body)
	}

	w = tn.call(tn.srv.enableUser, carol, http.MethodPost, "/v1/users/user_alice/enable", "")
	if w.Code != http.StatusOK {
		t.Fatalf("enabling alice = %d: %s", w.Code, w.Body)
	}
	if _, err := tn.srv.st.LookupKey(tn.ctx, []byte("hash-of-key_alice")); !errors.Is(err, policy.ErrKeyRevoked) {
		t.Errorf("enabling brought her old key back: %v", err)
	}
}
