package sandbox

import (
	"encoding/json"
	"testing"

	"github.com/bespinian/keera-gateway/internal/connect"
	"github.com/bespinian/keera-gateway/internal/policy"
)

// A sandbox's Pi configuration lists every reachable model, unlike the one
// `keera connect pi` prints for a laptop. The provider name, API shape and key
// reference must still match that template, or the two drift silently.
func TestPiConfigAgreesWithTheConnectCatalogue(t *testing.T) {
	client, ok := connect.Lookup("pi")
	if !ok {
		t.Fatal("the connect catalogue has no pi entry")
	}
	var laptop struct {
		Providers map[string]struct {
			BaseURL string `json:"baseUrl"`
			API     string `json:"api"`
			APIKey  string `json:"apiKey"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(
		[]byte(client.Render("https://keera.example.ch/api", "keera-code", 16384)),
		&laptop); err != nil {
		t.Fatalf("the connect template is not valid JSON: %v", err)
	}
	p, ok := laptop.Providers[piProvider]
	if !ok {
		t.Fatalf("the connect template names no %q provider; the sandbox writes one",
			piProvider)
	}
	if p.API != piAPI {
		t.Errorf("connect says api %q, the sandbox writes %q", p.API, piAPI)
	}
	if p.APIKey != piAPIKeyRef {
		t.Errorf("connect says apiKey %q, the sandbox writes %q", p.APIKey, piAPIKeyRef)
	}
	// The sandbox appends /v1 to the inference prefix, as the template does.
	if p.BaseURL != "https://keera.example.ch/api/v1" {
		t.Errorf("connect renders baseUrl %q; the sandbox's own suffix has to match", p.BaseURL)
	}
}

func TestPiConfigListsEveryReachableModel(t *testing.T) {
	m := &Manager{}
	models := []policy.Model{
		{Alias: "keera-speed", Kind: policy.KindChat, MaxContext: 16384, Enabled: true},
		{Alias: "keera-code", Kind: policy.KindChat, MaxContext: 0, Enabled: true},
	}
	var got struct {
		Providers map[string]struct {
			BaseURL string `json:"baseUrl"`
			APIKey  string `json:"apiKey"`
			Models  []struct {
				ID            string `json:"id"`
				Name          string `json:"name"`
				ContextWindow int    `json:"contextWindow"`
			} `json:"models"`
		} `json:"providers"`
	}
	raw := m.piConfig("http://keera-gateway:8080/api", models)
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, raw)
	}
	p := got.Providers[piProvider]
	if p.BaseURL != "http://keera-gateway:8080/api/v1" {
		t.Errorf("baseUrl = %q", p.BaseURL)
	}
	// A reference, never the key itself, which would then sit on the volume.
	if p.APIKey != piAPIKeyRef {
		t.Errorf("apiKey = %q, want the environment reference", p.APIKey)
	}
	if len(p.Models) != 2 {
		t.Fatalf("got %d models, want both the key can reach", len(p.Models))
	}
	if p.Models[0].ID != "keera-speed" || p.Models[0].Name != "Keera Speed" {
		t.Errorf("first model = %+v", p.Models[0])
	}
	// No declared window gives the default, not zero.
	if p.Models[1].ContextWindow != connect.DefaultContext {
		t.Errorf("a model with no declared window got %d, want %d",
			p.Models[1].ContextWindow, connect.DefaultContext)
	}
}

func TestPiConfigIsEmptyWithNothingToSay(t *testing.T) {
	m := &Manager{}
	if got := m.piConfig("", []policy.Model{{Alias: "a"}}); got != "" {
		t.Error("a sandbox with no gateway address should get no configuration " +
			"rather than one pointing nowhere")
	}
	if got := m.piConfig("http://x/api", nil); got != "" {
		t.Error("a key that can reach nothing should get no configuration rather " +
			"than an empty picker")
	}
}

func TestPiDefaultModelFallsBackToWhatTheKeyCanReach(t *testing.T) {
	models := []policy.Model{{Alias: "keera-speed"}, {Alias: "keera-code"}}
	// The deployment's favourite, when the key may reach it.
	if got := piDefaultModel("keera-code", models); got != "keera-code" {
		t.Errorf("got %q", got)
	}
	// Otherwise the first one it may.
	if got := piDefaultModel("keera-frontier", models); got != "keera-speed" {
		t.Errorf("got %q, want the first model the key can actually reach", got)
	}
	if got := piDefaultModel("keera-speed", nil); got != "keera-speed" {
		t.Errorf("got %q; with nothing to fall back to the ask is returned", got)
	}
}
