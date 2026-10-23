package control

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// A key lasts for a lifetime from now or until a moment, never both, and a
// moment already past would issue a key that is dead on arrival.
func TestKeyExpiry(t *testing.T) {
	future := time.Now().Add(48 * time.Hour)
	past := time.Now().Add(-time.Hour)
	month, zero, negative := int64(30*24*3600), int64(0), int64(-3600)
	for _, tc := range []struct {
		in     *int64
		at     *time.Time
		ok     bool
		expiry bool
	}{
		{nil, nil, true, false},
		{&month, nil, true, true},
		{nil, &future, true, true},
		{&zero, nil, false, false},
		{&negative, nil, false, false},
		{&month, &future, false, false},
		{nil, &past, false, false},
	} {
		w := httptest.NewRecorder()
		got, ok := keyExpiry(w, tc.in, tc.at)
		if ok != tc.ok || (got != nil) != tc.expiry {
			t.Errorf("keyExpiry(%v, %v) = %v, %v; want ok %v, expiry %v",
				tc.in, tc.at, got, ok, tc.ok, tc.expiry)
		}
		if !ok && w.Code != http.StatusBadRequest {
			t.Errorf("keyExpiry(%v, %v) answered %d, want 400", tc.in, tc.at, w.Code)
		}
	}
	if got, _ := keyExpiry(httptest.NewRecorder(), nil, &future); !got.Equal(future) {
		t.Errorf("expires_at = %v, want %v", got, future)
	}
}

func TestAKeyRequestIsCheckedOnItsFace(t *testing.T) {
	for _, tc := range []struct {
		in      keyRequest
		refused string
	}{
		{keyRequest{}, ""},
		{keyRequest{Kind: "admin"}, "'kind'"},
		{keyRequest{Kind: policy.KeySubscription}, "'user_id'"},
		{keyRequest{Kind: policy.KeySubscription, UserID: "user_a"}, ""},
		{keyRequest{Pending: true}, "'user_id'"},
		{keyRequest{Pending: true, UserID: "user_a"}, ""},
	} {
		in := tc.in
		err := in.check()
		switch {
		case tc.refused == "" && err != nil:
			t.Errorf("%+v: %v", tc.in, err)
		case tc.refused != "" && (err == nil || !strings.Contains(err.Error(), tc.refused)):
			t.Errorf("%+v: %v; want it refused for %s", tc.in, err, tc.refused)
		}
	}
	in := keyRequest{Name: "  "}
	if err := in.check(); err != nil || in.Name != "unnamed" || in.Kind != policy.KeyStandard {
		t.Errorf("defaults: %+v, %v", in, err)
	}
}
