package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/store"
)

// One row per client and key becomes one entry per client, newest first, with
// the keys it used.
func TestGroupClientsFoldsKeysUnderTheirClient(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	uses := []store.ClientUse{
		{Client: "claude-code", KeyID: "key_1", Requests: 3, LastUsed: now},
		{Client: "", KeyID: "key_1", Requests: 1, LastUsed: now.Add(-time.Hour)},
		{Client: "claude-code", KeyID: "key_2", Requests: 2, LastUsed: now.Add(-2 * time.Hour)},
	}
	got := groupClients(uses, map[string]string{"key_1": "laptop", "key_2": "desktop"})

	if len(got) != 2 {
		t.Fatalf("got %d clients, want 2: %+v", len(got), got)
	}
	cc := got[0]
	if cc.Key != "claude-code" || cc.Label != "Claude Code" {
		t.Errorf("first client = %q (%q), want Claude Code", cc.Key, cc.Label)
	}
	if cc.Requests != 5 || !cc.LastUsed.Equal(now) {
		t.Errorf("claude-code: %d requests, last %v; want 5 and the newest", cc.Requests, cc.LastUsed)
	}
	if len(cc.Keys) != 2 || cc.Keys[0].Name != "laptop" || cc.Keys[1].Name != "desktop" {
		t.Errorf("claude-code keys = %+v, want laptop then desktop", cc.Keys)
	}
	if got[1].Key != "" || got[1].Label != "Unidentified" {
		t.Errorf("a client that did not name itself is %q (%q)", got[1].Key, got[1].Label)
	}
}

// The operator key is not a person and has no keys, so it has no clients
// either. It gets an empty list, not an error.
func TestTheOperatorKeyHasNoClients(t *testing.T) {
	s := newServer()
	operator := &authn.Principal{Via: authn.MethodOperatorKey, Role: authn.RoleOperator}

	w := httptest.NewRecorder()
	s.listClients(w, httptest.NewRequest(http.MethodGet, httpx.ControlPrefix+"/v1/clients", nil), operator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var res struct {
		Anonymous bool              `json:"anonymous"`
		Data      []connectedClient `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.Anonymous || res.Data == nil || len(res.Data) != 0 {
		t.Errorf("got %+v, want an anonymous empty list", res)
	}
}
