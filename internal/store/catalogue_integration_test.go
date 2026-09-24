package store

import (
	"testing"

	"github.com/bespinian/keera-gateway/internal/policy"
)

func TestModelCatalogueRoundTrip(t *testing.T) {
	st, ctx := db(t)

	m := policy.Model{
		Alias: "keera-code", Kind: policy.KindChat,
		Backends:           []string{"http://vllm-a:8000/v1", "http://vllm-b:8000/v1"},
		BackendModel:       "Qwen/Qwen2.5-Coder-32B-Instruct",
		InputMicrosPerMTok: 1_000_000, OutputMicrosPerMTok: 4_000_000,
		CachedInputMicrosPerMTok: 100_000,
		MaxContext:               65536, APIKeyEnv: "ANTHROPIC_API_KEY", Enabled: true,
		Managed: true, Provider: "anthropic",
	}
	if err := st.UpsertModel(ctx, m); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}
	got, err := st.Model(ctx, "keera-code")
	if err != nil {
		t.Fatalf("Model: %v", err)
	}
	if !got.SameDeclaration(m) {
		t.Errorf("Model = %+v, want %+v", got, m)
	}
	if !got.Managed {
		t.Error("Managed was not stored; a file-declared alias must be refused to other writers")
	}
	// SameDeclaration covers this, but only as one of a dozen fields. It is
	// worth its own line: the provider is what says the values around it are
	// the provider's answers rather than the operator's, and a panel that
	// cannot tell goes back to asking somebody to check them.
	if got.Provider != "anthropic" {
		t.Errorf("Provider = %q, want the provider the entry was declared against", got.Provider)
	}
	// Likewise its own line: this one is the difference between a hosted
	// model's cost here and its cost in the provider's console, and a column
	// that quietly failed to round-trip would read as a rate nobody set.
	if got.CachedInputMicrosPerMTok != 100_000 {
		t.Errorf("cached-input rate = %d, want the stored 100000",
			got.CachedInputMicrosPerMTok)
	}
	if got.HasAPIKey {
		t.Error("HasAPIKey is set for a model with no stored credential")
	}

	// A credential is the one change a file-managed alias still accepts, and
	// applying a catalogue must not erase it.
	sealed := []byte{0x01, 0x02, 0x03}
	if err := st.SetModelCredential(ctx, "keera-code", sealed); err != nil {
		t.Fatalf("SetModelCredential: %v", err)
	}
	if err := st.UpsertModel(ctx, m); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}
	got, err = st.Model(ctx, "keera-code")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.APIKeyCiphertext) != string(sealed) || !got.HasAPIKey {
		t.Errorf("the stored credential was lost by re-applying the catalogue: %+v", got)
	}
	if err := st.SetModelCredential(ctx, "keera-code", nil); err != nil {
		t.Fatalf("SetModelCredential(nil): %v", err)
	}
	if got, _ = st.Model(ctx, "keera-code"); got.HasAPIKey {
		t.Error("the credential was not cleared")
	}
	if err := st.SetModelCredential(ctx, "nobody", sealed); err != ErrNotFound {
		t.Errorf("setting a credential on a missing alias gave %v, want ErrNotFound", err)
	}

	// A model the file no longer names is handed back to the operator rather
	// than deleted: a model is an API contract, so a line removed from a file
	// releases the entry rather than breaking every client that names it.
	if err := st.UnmanageModels(ctx, []string{"something-else"}); err != nil {
		t.Fatalf("UnmanageModels: %v", err)
	}
	got, err = st.Model(ctx, "keera-code")
	if err != nil {
		t.Fatalf("the model was deleted rather than released: %v", err)
	}
	if got.Managed {
		t.Error("the model is still marked as file-managed")
	}

	list, err := st.LoadModels(ctx)
	if err != nil {
		t.Fatalf("LoadModels: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("%d models, want 1", len(list))
	}
	if err := st.DeleteModel(ctx, "keera-code"); err != nil {
		t.Fatalf("DeleteModel: %v", err)
	}
	if _, err := st.Model(ctx, "keera-code"); err != ErrNotFound {
		t.Errorf("Model after delete gave %v, want ErrNotFound", err)
	}
	if err := st.DeleteModel(ctx, "keera-code"); err != ErrNotFound {
		t.Errorf("deleting it twice gave %v, want ErrNotFound", err)
	}
}
