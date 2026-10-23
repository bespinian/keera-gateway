package cli

import (
	"strings"
	"testing"
)

// chosenOrg is the organisation this machine remembers for the fake gateway.
func chosenOrg(t *testing.T) string {
	t.Helper()
	c, err := loadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	return c.Orgs[gatewayKey(newClient().base)]
}

// A person in several organisations moves their sign-in, found by name, and
// the machine remembers where it went.
func TestOrgSwitchMovesAPersonsSignIn(t *testing.T) {
	t.Setenv("KEERA_CONFIG_DIR", t.TempDir())
	f := newFakeControl(t, map[string]any{
		"GET /v1/me": map[string]any{"role": "admin", "org_id": "org_a", "orgs": []map[string]any{
			{"org_id": "org_a", "org_name": "Bank", "role": "admin"},
			{"org_id": "org_b", "org_name": "Insurer", "role": "member"},
		}},
		"POST /v1/me/org": map[string]any{"org_id": "org_b", "org_name": "Insurer", "role": "member"},
	})

	said := capture(t, "org", "switch", "insurer")
	if got := f.request("POST", "/v1/me/org").body["org_id"]; got != "org_b" {
		t.Errorf("switched to %v, want org_b", got)
	}
	if !strings.Contains(said, "Now in Insurer as member") {
		t.Errorf("said %q", said)
	}
	if got := chosenOrg(t); got != "org_b" {
		t.Errorf("remembered %q, want org_b", got)
	}
}

func TestOrgSwitchRefusesAnOrganisationThePersonIsNotIn(t *testing.T) {
	quiet(t)
	t.Setenv("KEERA_CONFIG_DIR", t.TempDir())
	f := newFakeControl(t, map[string]any{
		"GET /v1/me": map[string]any{"role": "member", "org_id": "org_a", "org_name": "Bank"},
	})
	if err := Run(t.Context(), []string{"org", "switch", "org_b"}); err == nil {
		t.Error("switched to an organisation the person is not in")
	}
	if f.called("POST", "/v1/me/org") {
		t.Error("asked the gateway to switch anyway")
	}
}

// An operator sees every organisation, so the choice is only remembered, and
// --all forgets it again.
func TestOrgSwitchOnlyRemembersAnOperatorsChoice(t *testing.T) {
	quiet(t)
	t.Setenv("KEERA_CONFIG_DIR", t.TempDir())
	f := newFakeControl(t, map[string]any{
		"GET /v1/me": map[string]any{"role": "operator", "unrestricted": true},
		"GET /v1/orgs": map[string]any{"data": []map[string]any{
			{"id": "org_a", "name": "Bank"}, {"id": "org_b", "name": "Insurer"},
		}},
		"GET /v1/models": map[string]any{"data": []map[string]any{}},
	})

	if err := Run(t.Context(), []string{"org", "switch", "org_b"}); err != nil {
		t.Fatal(err)
	}
	if f.called("POST", "/v1/me/org") {
		t.Error("an operator's sign-in was moved")
	}
	if err := Run(t.Context(), []string{"model", "list"}); err != nil {
		t.Fatal(err)
	}
	if got := f.request("GET", "/v1/models").query; got != "org_id=org_b" {
		t.Errorf("the models were listed for %q, want org_id=org_b", got)
	}

	if err := Run(t.Context(), []string{"org", "switch", "--all"}); err != nil {
		t.Fatal(err)
	}
	if got := chosenOrg(t); got != "" {
		t.Errorf("remembered %q after --all, want nothing", got)
	}
}
