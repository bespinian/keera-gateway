package sandbox

import (
	"encoding/json"
	"testing"

	"github.com/bespinian/keera-gateway/internal/connect"
	"github.com/bespinian/keera-gateway/internal/policy"
)

// piFile is what the tests read of a rendered models.json.
type piFile struct {
	Providers map[string]struct {
		BaseURL string            `json:"baseUrl"`
		APIKey  string            `json:"apiKey"`
		Headers map[string]string `json:"headers"`
		Models  []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			ContextWindow int    `json:"contextWindow"`
		} `json:"models"`
	} `json:"providers"`
}

func readPi(t *testing.T, raw string) piFile {
	t.Helper()
	var got piFile
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, raw)
	}
	if _, ok := got.Providers[piProvider]; !ok {
		t.Fatalf("no %q provider, which Pi's settings make the default:\n%s", piProvider, raw)
	}
	return got
}

// A sandbox's Pi configuration is the one `keera connect pi` prints for a
// laptop, so Pi is identified and its models named the same way.
func TestPiConfigListsEveryReachableModel(t *testing.T) {
	models := []connect.Model{
		{Alias: "keera-speed", Name: "Keera Speed 2", MaxContext: 16384},
		{Alias: "keera-code"},
		{Alias: "auto"},
	}
	p := readPi(t, piConfig("http://keera-gateway:8080/api", models, "")).Providers[piProvider]
	if p.BaseURL != "http://keera-gateway:8080/api/v1" {
		t.Errorf("baseUrl = %q", p.BaseURL)
	}
	// A reference, never the key itself, which an edited copy would keep after
	// a resume gave the sandbox a new one.
	if p.APIKey != "${KEERA_API_KEY}" {
		t.Errorf("apiKey = %q, want the environment reference", p.APIKey)
	}
	if p.Headers[connect.ClientHeader] != "pi" {
		t.Errorf("headers = %v, want Pi to name itself", p.Headers)
	}
	if len(p.Models) != 3 {
		t.Fatalf("got %d models, want all the key can reach", len(p.Models))
	}
	if p.Models[0].ID != "keera-speed" || p.Models[0].Name != "Keera Speed 2" ||
		p.Models[0].ContextWindow != 16384 {
		t.Errorf("first model = %+v", p.Models[0])
	}
	// No declared window gives the default, not zero.
	if p.Models[1].ContextWindow != connect.DefaultContext {
		t.Errorf("a model with no declared window got %d, want %d",
			p.Models[1].ContextWindow, connect.DefaultContext)
	}
}

func TestPiConfigIsEmptyWithNothingToSay(t *testing.T) {
	if got := piConfig("", []connect.Model{{Alias: "a"}}, ""); got != "" {
		t.Error("a sandbox with no gateway address should get no configuration " +
			"rather than one pointing nowhere")
	}
	if got := piConfig("http://x/api", nil, ""); got != "" {
		t.Error("a key that can reach nothing should get no configuration rather " +
			"than an empty picker")
	}
}

// An agent's sandbox sends its own id, so its task is one session.
func TestPiConfigSendsTheAgentSession(t *testing.T) {
	models := []connect.Model{{Alias: "keera-code"}}
	h := readPi(t, piConfig("http://x/api", models, "sbx_1")).Providers[piProvider].Headers
	if h["X-Keera-Session"] != "sbx_1" || h[connect.ClientHeader] != "pi" {
		t.Errorf("headers = %v, want the session and the client", h)
	}
	h = readPi(t, piConfig("http://x/api", models, "")).Providers[piProvider].Headers
	if _, ok := h["X-Keera-Session"]; ok {
		t.Errorf("an engineer's sandbox got a session header: %v", h)
	}
}

// A sandbox key is a standard key. Its agent must not be offered a
// subscription model, which the gateway refuses that key.
func TestASandboxIsNotOfferedASubscriptionModel(t *testing.T) {
	resolved := policy.Resolve(policy.Key{OrgID: "org_1", Kind: policy.KeyStandard}, nil, nil, nil)
	catalogue := []policy.Model{
		{Alias: "keera-code", Kind: policy.KindChat, Enabled: true},
		{Alias: "claude", Kind: policy.KindChat, Enabled: true, Subscription: true},
		{Alias: "embed", Kind: policy.KindEmbedding, Enabled: true},
		{Alias: "off", Kind: policy.KindChat},
	}
	got := chatModelsFor(resolved, catalogue, nil)
	if len(got) != 1 || got[0].Alias != "keera-code" {
		t.Errorf("offered %+v, want only keera-code", got)
	}
}

// A router is a model to a client, so the agent is offered the ones the
// guardrails allow, after the models.
func TestASandboxIsOfferedItsRouters(t *testing.T) {
	resolved := policy.Resolve(policy.Key{OrgID: "org_1", Kind: policy.KeyStandard},
		&policy.Limits{AllowedModels: []string{"keera-code", "auto"}}, nil, nil)
	catalogue := []policy.Model{
		{Alias: "keera-code", DisplayName: "Keera Code", Kind: policy.KindChat, Enabled: true},
	}
	routers := []policy.Router{{Alias: "auto"}, {Alias: "other"}}
	got := chatModelsFor(resolved, catalogue, routers)
	if len(got) != 2 || got[0].Name != "Keera Code" || got[1].Alias != "auto" {
		t.Errorf("offered %+v, want keera-code by its name, then auto", got)
	}
}
