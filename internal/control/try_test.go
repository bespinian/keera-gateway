package control

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestATryTakesOnlyAFewPromptsThatSaySomething(t *testing.T) {
	cases := map[string]struct {
		body string
		ok   bool
	}{
		"one prompt":      {`{"prompts":["rename tmp2"]}`, true},
		"no prompt":       {`{"prompts":[]}`, false},
		"a blank prompt":  {`{"prompts":["ok","  "]}`, false},
		"too many":        {`{"prompts":["a","b","c","d","e","f","g","h","i","j","k"]}`, false},
		"too long":        {`{"prompts":["` + strings.Repeat("a", maxTryPromptBytes+1) + `"]}`, false},
		"a misspelt name": {`{"prompt":["rename tmp2"]}`, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/v1/filters/f/try", strings.NewReader(tc.body))
			req, ok := readTry(w, r)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v: %s", ok, tc.ok, w.Body)
			}
			if !ok && w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", w.Code)
			}
			if ok && len(req.Prompts) != 1 {
				t.Errorf("prompts = %q, want the one given", req.Prompts)
			}
		})
	}
}
