package gateway

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// locatedHarness is a key limited to Switzerland, with its client's model
// there and its filter's model in the USA.
func locatedHarness(t *testing.T) *harness {
	t.Helper()
	res := policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"},
		&policy.Limits{AllowedLocations: []string{"ch"}}, nil, nil)
	res.Filters = []string{"redact"}
	h := filterHarness(t, redactor, res)
	place(h, "keera-code", "ch")
	place(h, "keera-guard", "usa")
	return h
}

func place(h *harness, alias, loc string) {
	m := h.src.models[alias]
	m.Location = loc
	h.src.models[alias] = m
}

// A filter reads the request before anything is redacted, so one on a model
// outside the allowed locations refuses the request rather than send it there.
func TestAFilterOutsideTheAllowedLocationsRefusesTheRequest(t *testing.T) {
	h := locatedHarness(t)
	body := `{"model":"keera-code","messages":[{"role":"user","content":"hunter2"}]}`
	resp := h.post(t, "/v1/chat/completions", body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	var envelope struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&envelope)
	if envelope.Error.Code != "location_not_allowed" {
		t.Errorf("code = %q", envelope.Error.Code)
	}
	select {
	case sent := <-h.upstreamBodies:
		t.Errorf("a request left the allowed locations: %s", sent)
	default:
	}

	place(h, "keera-guard", "ch")
	if resp := h.post(t, "/v1/chat/completions", body); resp.StatusCode != http.StatusOK {
		t.Errorf("with the filter inside: status = %d, want 200", resp.StatusCode)
	}
}

func TestAModelOutsideTheAllowedLocationsIsNotFound(t *testing.T) {
	h := locatedHarness(t)
	place(h, "keera-guard", "ch")
	place(h, "keera-embed", "usa")
	resp := h.post(t, "/v1/embeddings", `{"model":"keera-embed","input":"hi"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}

	// A router is listed only while every model it reaches is inside.
	h.withRouters(policy.Route{
		OrgID: "org_1", Router: "inside", Model: "keera-guard",
		Destinations: []string{"keera-code", "keera-guard"},
	}, policy.Route{
		OrgID: "org_1", Router: "outside", Model: "keera-guard",
		Destinations: []string{"keera-code", "keera-rerank"},
	})
	place(h, "keera-rerank", "usa")
	var list struct {
		Data []struct{ ID string } `json:"data"`
	}
	_ = json.NewDecoder(h.get(t, "/v1/models").Body).Decode(&list)
	var ids []string
	for _, m := range list.Data {
		ids = append(ids, m.ID)
	}
	if !slices.Contains(ids, "keera-code") || !slices.Contains(ids, "inside") ||
		slices.Contains(ids, "keera-embed") || slices.Contains(ids, "outside") {
		t.Errorf("listed %v", ids)
	}
}
