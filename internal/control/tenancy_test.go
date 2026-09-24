package control

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/store"
)

// Deleting a tenant takes its people and its keys with it. An organisation's
// own administrator must not be able to reach it, however much else inside that
// organisation they own.
func TestDeleteOrgIsOperatorOnly(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))

	for _, p := range []*authn.Principal{
		{Via: authn.MethodSession, Role: authn.RoleAdmin, OrgID: "org_1"},
		{Via: authn.MethodSession, Role: authn.RoleMember, OrgID: "org_1"},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodDelete, httpx.ControlPrefix+"/v1/orgs/org_2", nil)
		r.SetPathValue("id", "org_2")

		s.deleteOrg(w, r, p)

		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d for a %s, want 403", w.Code, p.Role)
		}
		if !strings.Contains(w.Body.String(), "only an operator") {
			t.Errorf("body = %q, want it to say who may delete one", w.Body.String())
		}
	}
}

// An operator signed in through the panel has a user row and a session inside
// some organisation. Deleting that one would cascade over both, signing them out
// mid-request and possibly removing the last operator in the deployment, so the
// handler refuses before it reaches the store - which is why a nil store here
// does not panic.
func TestOperatorCannotDeleteTheirOwnOrg(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodDelete, httpx.ControlPrefix+"/v1/orgs/org_1", nil)
	r.SetPathValue("id", "org_1")

	s.deleteOrg(w, r, &authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleOperator, OrgID: "org_1",
	})

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "signed in to") {
		t.Errorf("body = %q, want it to explain the way out", w.Body.String())
	}
}

// Two organisations cannot share a domain: one domain places a sign-in in one
// tenant. That is an ordinary mistake - a domain typed against the wrong id -
// and the generic failure path would answer it with "internal error" and a
// request id, which names neither what was wrong nor what to change.
func TestADomainAnotherOrganisationHoldsIsAConflict(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	w := httptest.NewRecorder()

	s.failDomain(w, fmt.Errorf("setting it: %w", store.ErrDomainTaken))

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	if !strings.Contains(w.Body.String(), "another organisation") {
		t.Errorf("body = %q, want it to say what holds the domain", w.Body.String())
	}
}

// A member issues keys for themselves, which is the whole point of letting them
// issue any: the secret is readable exactly once, and the fewer screens that
// moment happens on the better. Naming a colleague is the one thing that must
// not work - attribution is what a budget and a usage report are read through,
// so it would put this member's spend under somebody else's name.
//
// The refusal happens before the store is touched, which is why a nil store
// here does not panic.
func TestMemberCanOnlyIssueAKeyForThemselves(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	member := &authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleMember, OrgID: "org_1", UserID: "user_1",
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, httpx.ControlPrefix+"/v1/keys",
		strings.NewReader(`{"org_id":"org_1","user_id":"user_2","alias":"not mine"}`))

	s.createKey(w, r, member)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "attributed to themselves") {
		t.Errorf("body = %q, want it to say whose key a member may issue", w.Body.String())
	}
}

// A team is a set of guardrails, and there is no membership to read the right
// one off. Choosing one is refused rather than dropped, so that a member is
// never handed a key that belongs somewhere other than they asked for.
func TestMemberCannotChooseTheTeamOfTheirOwnKey(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	member := &authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleMember, OrgID: "org_1", UserID: "user_1",
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, httpx.ControlPrefix+"/v1/keys",
		strings.NewReader(`{"org_id":"org_1","user_id":"user_1","team_id":"team_1"}`))

	s.createKey(w, r, member)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "team") {
		t.Errorf("body = %q, want it to name the field it refused", w.Body.String())
	}
}

// Somebody signed in to one organisation must not issue a key in another, and
// the refusal must not depend on the person they attributed it to being theirs.
func TestKeysCannotBeIssuedIntoAnotherOrganisation(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))

	for _, p := range []*authn.Principal{
		{Via: authn.MethodSession, Role: authn.RoleMember, OrgID: "org_1", UserID: "user_1"},
		{Via: authn.MethodSession, Role: authn.RoleAdmin, OrgID: "org_1", UserID: "user_admin"},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, httpx.ControlPrefix+"/v1/keys",
			strings.NewReader(`{"org_id":"org_2","user_id":"user_1"}`))

		s.createKey(w, r, p)

		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d for a %s, want 403", w.Code, p.Role)
		}
	}
}

// The operator role crosses organisations and owns the model catalogue, so it
// comes from KEERA_OPERATORS or an operator group - configuration a customer
// does not hold. No caller grants it through the API, an operator included:
// otherwise the answer to "who are the operators of this deployment" would be a
// database query rather than the environment.
func TestOperatorRoleCannotBeGrantedThroughTheAPI(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	operator := &authn.Principal{Via: authn.MethodOperatorKey, Role: authn.RoleOperator}

	t.Run("upsert", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, httpx.ControlPrefix+"/v1/users",
			strings.NewReader(`{"org_id":"org_1","email":"a@example.ch","role":"operator"}`))

		s.upsertUser(w, r, operator)

		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
		if !strings.Contains(w.Body.String(), "KEERA_OPERATORS") {
			t.Errorf("body = %q, want it to name what does grant it", w.Body.String())
		}
	})
	// The store is nil, so this also pins that the refusal comes before the row
	// is read: whether a role can be granted at all is a property of the
	// deployment, not of who is being changed.
	t.Run("set role", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPatch, httpx.ControlPrefix+"/v1/users/user_1",
			strings.NewReader(`{"role":"operator"}`))
		r.SetPathValue("id", "user_1")

		s.updateUser(w, r, operator)

		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
		if !strings.Contains(w.Body.String(), "KEERA_OPERATORS") {
			t.Errorf("body = %q, want it to name what does grant it", w.Body.String())
		}
	})
}
