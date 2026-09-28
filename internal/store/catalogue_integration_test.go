package store

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// anOrg creates an organisation for a test that needs somewhere to put models.
func anOrg(t *testing.T, st *Store, ctx context.Context, id string) {
	t.Helper()
	if _, err := st.CreateOrg(ctx, Org{ID: id, Name: id}, OrgTemplate{}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
}

// A model with no release date is stored as null and read back as empty.
func TestModelWithoutAReleaseDate(t *testing.T) {
	st, ctx := db(t)
	anOrg(t, st, ctx, "org_1")

	m := policy.Model{
		OrgID: "org_1", Alias: "keera-code", Kind: policy.KindChat, Backends: []string{"http://x/v1"},
		BackendModel: "x", Location: policy.LocationOnPrem, Enabled: true,
	}
	if err := st.UpsertModel(ctx, m); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}
	got, err := st.Model(ctx, "org_1", "keera-code")
	if err != nil {
		t.Fatalf("Model: %v", err)
	}
	if got.ReleaseDate != "" || got.Location != policy.LocationOnPrem {
		t.Errorf("release date = %q, location = %q, want empty and onprem",
			got.ReleaseDate, got.Location)
	}
}

func TestModelCatalogueRoundTrip(t *testing.T) {
	st, ctx := db(t)
	anOrg(t, st, ctx, "org_1")

	m := policy.Model{
		OrgID: "org_1", Alias: "keera-code", Kind: policy.KindChat,
		Backends:           []string{"http://vllm-a:8000/v1", "http://vllm-b:8000/v1"},
		BackendModel:       "Qwen/Qwen2.5-Coder-32B-Instruct",
		InputMicrosPerMTok: 1_000_000, OutputMicrosPerMTok: 4_000_000,
		CachedInputMicrosPerMTok: 100_000,
		MaxContext:               65536, Enabled: true,
		Provider: "anthropic", ReleaseDate: "2026-07-24", Location: "usa",
	}
	if err := st.UpsertModel(ctx, m); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}
	got, err := st.Model(ctx, "org_1", "keera-code")
	if err != nil {
		t.Fatalf("Model: %v", err)
	}
	// The provider is what says the values around it are the provider's
	// answers rather than the operator's, and a panel that cannot tell goes
	// back to asking somebody to check them.
	if got.Provider != "anthropic" {
		t.Errorf("Provider = %q, want the provider the entry was declared against", got.Provider)
	}
	// This one is the difference between a hosted model's cost here and its
	// cost in the provider's console, and a column that quietly failed to
	// round-trip would read as a rate nobody set.
	if got.CachedInputMicrosPerMTok != 100_000 {
		t.Errorf("cached-input rate = %d, want the stored 100000",
			got.CachedInputMicrosPerMTok)
	}
	if !slices.Equal(got.Backends, m.Backends) || got.MaxContext != m.MaxContext ||
		got.OutputMicrosPerMTok != m.OutputMicrosPerMTok {
		t.Errorf("Model = %+v, want %+v", got, m)
	}
	if got.HasAPIKey {
		t.Error("HasAPIKey is set for a model with no stored credential")
	}
	// The date is stored as a date and read back as text, so it is worth
	// checking it comes back as it went in.
	if got.ReleaseDate != "2026-07-24" || got.Location != "usa" {
		t.Errorf("release date = %q, location = %q, want 2026-07-24 and usa",
			got.ReleaseDate, got.Location)
	}

	// An edit that does not mention the credential keeps it.
	sealed := []byte{0x01, 0x02, 0x03}
	if err := st.SetModelCredential(ctx, "org_1", "keera-code", sealed); err != nil {
		t.Fatalf("SetModelCredential: %v", err)
	}
	if err := st.UpsertModel(ctx, m); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}
	got, err = st.Model(ctx, "org_1", "keera-code")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.APIKeyCiphertext) != string(sealed) || !got.HasAPIKey {
		t.Errorf("the stored credential was lost by an edit: %+v", got)
	}
	if err := st.SetModelCredential(ctx, "org_1", "keera-code", nil); err != nil {
		t.Fatalf("SetModelCredential(nil): %v", err)
	}
	if got, _ = st.Model(ctx, "org_1", "keera-code"); got.HasAPIKey {
		t.Error("the credential was not cleared")
	}
	if err := st.SetModelCredential(ctx, "org_1", "nobody", sealed); err != ErrNotFound {
		t.Errorf("setting a credential on a missing alias gave %v, want ErrNotFound", err)
	}

	if err := st.DeleteModel(ctx, "org_1", "keera-code"); err != nil {
		t.Fatalf("DeleteModel: %v", err)
	}
	if _, err := st.Model(ctx, "org_1", "keera-code"); err != ErrNotFound {
		t.Errorf("Model after delete gave %v, want ErrNotFound", err)
	}
	if err := st.DeleteModel(ctx, "org_1", "keera-code"); err != ErrNotFound {
		t.Errorf("deleting it twice gave %v, want ErrNotFound", err)
	}
}

// A new organisation starts with a copy of each template model and sandbox
// class, as its own.
func TestANewOrganisationStartsWithTheTemplate(t *testing.T) {
	st, ctx := db(t)
	template := []policy.Model{{
		Alias: "keera-speed", Kind: policy.KindChat, Backends: []string{"http://x/v1"},
		BackendModel: "x", Location: policy.LocationOnPrem, Enabled: true,
	}}
	for _, id := range []string{"org_1", "org_2"} {
		classes := []policy.SandboxClass{{
			Name: "standard", Image: "example/sandbox:1", Isolation: policy.IsolationIsolated,
			DefaultTTL: time.Hour, MaxTTL: 4 * time.Hour,
		}}
		if _, err := st.CreateOrg(ctx, Org{ID: id, Name: id}, OrgTemplate{Models: template, SandboxClasses: classes}); err != nil {
			t.Fatalf("CreateOrg: %v", err)
		}
	}
	for _, id := range []string{"org_1", "org_2"} {
		if m, err := st.Model(ctx, id, "keera-speed"); err != nil || m.OrgID != id {
			t.Errorf("%s keera-speed = %+v, %v; want its own copy", id, m, err)
		}
		if c, err := st.SandboxClass(ctx, id, "standard"); err != nil || c.OrgID != id {
			t.Errorf("%s standard = %+v, %v; want its own copy", id, c, err)
		}
	}
	// A copy is the organisation's own, so removing it leaves the other.
	if err := st.DeleteModel(ctx, "org_1", "keera-speed"); err != nil {
		t.Fatalf("DeleteModel: %v", err)
	}
	if _, err := st.Model(ctx, "org_2", "keera-speed"); err != nil {
		t.Errorf("removing org_1's copy removed org_2's: %v", err)
	}
}

// An organisation's models are its own: another tenant does not see them, and
// two tenants can each use an alias.
func TestOrganisationModelsAreScopedToTheirOrganisation(t *testing.T) {
	st, ctx := db(t)
	anOrg(t, st, ctx, "org_1")
	anOrg(t, st, ctx, "org_2")
	put := func(orgID, alias, backendModel string) {
		t.Helper()
		m := policy.Model{
			OrgID: orgID, Alias: alias, Kind: policy.KindChat, Backends: []string{"http://x/v1"},
			BackendModel: backendModel, Location: policy.LocationOnPrem, Enabled: true,
		}
		if err := st.UpsertModel(ctx, m); err != nil {
			t.Fatalf("UpsertModel(%s/%s): %v", orgID, alias, err)
		}
	}
	put("org_1", "fast", "org_1")
	put("org_1", "private", "org_1")
	put("org_2", "fast", "org_2")
	put("org_2", "private", "org_2")
	// Written again, so the upsert updates it in place rather than adding a
	// second one.
	put("org_1", "private", "org_1 again")

	aliases := func(ms []policy.Model) []string {
		var out []string
		for _, m := range ms {
			out = append(out, m.OrgID+"/"+m.Alias+"="+m.BackendModel)
		}
		return out
	}
	list, err := st.ListModels(ctx, "org_1")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := []string{"org_1/fast=org_1", "org_1/private=org_1 again"}
	if got := aliases(list); !slices.Equal(got, want) {
		t.Errorf("org_1 sees %v, want %v", got, want)
	}
	if all, _ := st.LoadModels(ctx); len(all) != 4 {
		t.Errorf("LoadModels read %d models, want every one of the 4", len(all))
	}

	// A credential and a removal reach only the organisation they name.
	if err := st.SetModelCredential(ctx, "org_1", "private", []byte("sealed")); err != nil {
		t.Fatalf("SetModelCredential: %v", err)
	}
	if m, _ := st.Model(ctx, "org_2", "private"); m.HasAPIKey {
		t.Error("setting org_1's credential set org_2's")
	}
	if err := st.DeleteModel(ctx, "org_2", "private"); err != nil {
		t.Fatalf("DeleteModel: %v", err)
	}
	if m, err := st.Model(ctx, "org_1", "private"); err != nil || !m.HasAPIKey {
		t.Errorf("org_1 private = %+v, %v after org_2 removed its own", m, err)
	}

	// The organisation's models go with it.
	if _, err := st.DeleteOrg(ctx, "org_1"); err != nil {
		t.Fatalf("DeleteOrg: %v", err)
	}
	if all, _ := st.LoadModels(ctx); len(all) != 1 {
		t.Errorf("after org_1 left there are %v, want org_2's one", aliases(all))
	}
}
