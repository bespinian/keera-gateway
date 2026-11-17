package control

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/store"
)

// A recurring charge stops everything, as it does for a request. With
// payments, credit decides; without them, a self-signed-up organisation
// waits, as it does for the deployment's provider keys.
func TestWhenAnOrganisationMayRunMachines(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		credit   store.CreditRow
		limited  bool
		payments bool
		want     string
	}{
		{"nothing set up", store.CreditRow{}, false, false, ""},
		{"limited without payments", store.CreditRow{}, true, false, "org_limited"},
		{"limited with credit", store.CreditRow{BalanceMicros: 1}, true, true, ""},
		{"no credit", store.CreditRow{}, false, true, "credit_exhausted"},
		{"below zero", store.CreditRow{BalanceMicros: -5}, false, true, "credit_exhausted"},
		{"invoiced", store.CreditRow{BalanceMicros: -5, Invoiced: true}, false, true, ""},
		{"credit", store.CreditRow{BalanceMicros: 1}, false, true, ""},
		{"charge due", store.CreditRow{BalanceMicros: 1, ChargeDueAt: now}, false, true, "charge_due"},
		{
			"charge due without payments",
			store.CreditRow{ChargeDueAt: now.Add(-time.Hour)},
			false, false, "charge_due",
		},
		{"charge due later", store.CreditRow{BalanceMicros: 1, ChargeDueAt: now.Add(time.Hour)}, false, true, ""},
	} {
		got := refuseMachines(tc.credit, tc.limited, tc.payments, now)
		code := ""
		if got != nil {
			code = got.Code
			if !strings.Contains(got.Message, "Keera Gateway") {
				t.Errorf("%s: %q does not say where to go", tc.name, got.Message)
			}
		}
		if code != tc.want {
			t.Errorf("%s: refusal %q, want %q", tc.name, code, tc.want)
		}
	}
}

func TestServiceLinesAreChecked(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	good := store.ServiceLine{
		ID: "agents/org_a/2026-11-17T09:00:00Z/cpu", OrgID: "org_a", TS: now.Add(-time.Hour),
		Item: "CPU", Unit: "CPU-hours", QuantityMicros: 1_500_000, Micros: 75_000,
	}
	if why := checkServiceLines([]store.ServiceLine{good}, now); why != "" {
		t.Fatalf("a good line: %s", why)
	}
	for name, change := range map[string]func(*store.ServiceLine){
		"no id":             func(l *store.ServiceLine) { l.ID = "" },
		"a long id":         func(l *store.ServiceLine) { l.ID = strings.Repeat("x", 201) },
		"no organisation":   func(l *store.ServiceLine) { l.OrgID = "" },
		"no item":           func(l *store.ServiceLine) { l.Item = "" },
		"a long item":       func(l *store.ServiceLine) { l.Item = strings.Repeat("x", 65) },
		"a long unit":       func(l *store.ServiceLine) { l.Unit = strings.Repeat("x", 33) },
		"a negative amount": func(l *store.ServiceLine) { l.QuantityMicros = -1 },
		"negative micros":   func(l *store.ServiceLine) { l.Micros = -1 },
		"too dear":          func(l *store.ServiceLine) { l.Micros = maxLineMicros + 1 },
		"no time":           func(l *store.ServiceLine) { l.TS = time.Time{} },
		"in the future":     func(l *store.ServiceLine) { l.TS = now.Add(10 * time.Minute) },
		"long ago":          func(l *store.ServiceLine) { l.TS = now.Add(-41 * 24 * time.Hour) },
	} {
		bad := good
		change(&bad)
		why := checkServiceLines([]store.ServiceLine{good, bad}, now)
		if !strings.HasPrefix(why, "line 1: ") {
			t.Errorf("%s: %q, want line 1 refused", name, why)
		}
	}
	if why := checkServiceLines(nil, now); why == "" {
		t.Error("no lines were taken")
	}
	if why := checkServiceLines(make([]store.ServiceLine, maxServiceLines+1), now); !strings.Contains(why, "1000") {
		t.Errorf("too many lines: %q", why)
	}
}

// Only Keera Agents may call the billing routes, even registered elsewhere.
func TestAgentsBillingRoutesTakeOnlyTheServiceKey(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{OperatorKey: testOperatorKey, AgentsServiceKey: testServiceKey},
		slog.New(slog.DiscardHandler))
	p := &authn.Principal{Via: authn.MethodService}
	w := httptest.NewRecorder()
	s.serviceBillingLines(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`)),
		&authn.Principal{Via: authn.MethodOperatorKey})
	if w.Code != http.StatusForbidden {
		t.Errorf("the operator key: %d, want 403", w.Code)
	}
	w = httptest.NewRecorder()
	s.serviceBillingLines(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"lines":[]}`)), p)
	if w.Code != http.StatusBadRequest {
		t.Errorf("no lines: %d %s, want 400", w.Code, w.Body)
	}
}
