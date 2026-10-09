package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// An organisation that signed itself up and has not paid gets nothing that
// runs on the deployment's own machines. The registry decides which models
// those are; the gateway has to hold to it on every path.

func TestALockedModelIsRefusedAndNotOffered(t *testing.T) {
	models := map[string]policy.Model{
		"engine": {
			Alias: "engine", Kind: policy.KindChat, BackendModel: "a", Enabled: true,
			Limited: true, Locked: true,
		},
		"own-key": {
			Alias: "own-key", Kind: policy.KindChat, BackendModel: "b", Enabled: true,
			Limited: true,
		},
	}
	h := newHarness(t, jsonBackend(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`), models, nil)

	resp := h.post(t, "/v1/chat/completions", `{"model":"engine","messages":[]}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "org_limited") || !strings.Contains(string(raw), "credit") {
		t.Errorf("the refusal does not say why or what lifts it: %s", raw)
	}
	select {
	case <-h.upstreamBodies:
		t.Error("a locked model's request reached the inference plane")
	default:
	}

	// The test backend is on the loopback, which is not a private range, so
	// an open model of a limited organisation still reaches it.
	if resp := h.post(t, "/v1/chat/completions", `{"model":"own-key","messages":[]}`); resp.StatusCode != http.StatusOK {
		t.Errorf("an open model of a limited organisation: status = %d, want 200", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, h.url("/v1/models"), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	listed, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listed.Body.Close() }()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(listed.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 1 || out.Data[0].ID != "own-key" {
		t.Errorf("/v1/models lists %+v, want only own-key", out.Data)
	}
}

func TestALimitedOrganisationCannotDialAPrivateAddress(t *testing.T) {
	// A public name can resolve to a private address, so the check is on
	// the address dialled. Nothing listens there; the dial is refused first.
	models := map[string]policy.Model{
		"rebound": {
			Alias: "rebound", Kind: policy.KindChat, BackendModel: "a", Enabled: true,
			Backends: []string{"http://10.255.255.1:9/v1"}, Limited: true,
		},
	}
	h := newHarness(t, jsonBackend(`{}`), models, nil)
	resp := h.post(t, "/v1/chat/completions", `{"model":"rebound","messages":[]}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	// The client is told only that the plane could not be reached; the
	// reason is the dialler's.
	_, err := h.srv.clientFor(true).Get("http://10.255.255.1:9/")
	if err == nil || !strings.Contains(err.Error(), "public addresses") {
		t.Errorf("dialling a private address: %v, want it refused", err)
	}
	// Everyone else keeps reaching a self-hosted plane on a private address.
	if h.srv.clientFor(false) == h.srv.clientFor(true) {
		t.Error("an organisation that is not limited dials through the limited client")
	}
}

func TestALockedModelFailsWhereARouterOrFilterWouldUseIt(t *testing.T) {
	if why := chatProblem(policy.Model{Enabled: true, Kind: policy.KindChat,
		Backends: []string{"http://x"}, Locked: true}, true); !strings.Contains(why, "credit") {
		t.Errorf("chatProblem = %q, want it to say the model is locked", why)
	}
}
