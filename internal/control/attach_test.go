package control

import (
	"errors"
	"testing"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/store"
)

// A sandbox name is unique per person, so one name can find several. What a
// member is told must not depend on whether a colleague used the name.
func TestPickSandboxNeverRevealsAColleaguesSandbox(t *testing.T) {
	mine := store.Sandbox{ID: "sbx_mine", OrgID: "org_1", UserID: "user_me"}
	theirs := store.Sandbox{ID: "sbx_theirs", OrgID: "org_1", UserID: "user_them"}
	member := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleMember,
		OrgID: "org_1", UserID: "user_me"}
	admin := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleAdmin,
		OrgID: "org_1", UserID: "user_admin"}

	if sb, err := pickSandbox(member, []store.Sandbox{theirs, mine}); err != nil || sb.ID != mine.ID {
		t.Errorf("member = %s, %v; want their own", sb.ID, err)
	}
	if _, err := pickSandbox(member, []store.Sandbox{theirs}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("member naming a colleague's = %v, want ErrNotFound", err)
	}
	if sb, err := pickSandbox(admin, []store.Sandbox{theirs}); err != nil || sb.ID != theirs.ID {
		t.Errorf("admin = %s, %v; want the only one", sb.ID, err)
	}
	if _, err := pickSandbox(admin, []store.Sandbox{theirs, mine}); !errors.Is(err, errSandboxAmbiguous) {
		t.Errorf("admin with two = %v, want errSandboxAmbiguous", err)
	}
}
