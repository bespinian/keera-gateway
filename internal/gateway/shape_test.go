package gateway

import (
	"encoding/json"
	"net/http"
	"testing"
)

// An OpenAI SDK reads only error.message, so an upstream error in any other
// envelope reaches the client wrapped, and one already in it is left alone.
func TestOpenAIErrorBody(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		status  int
		message string
		typ     string
	}{
		{`{"error":{"message":"bad","type":"invalid_request_error","code":"x"}}`, 400, "bad", "invalid_request_error"},
		{`{"detail":"The model does not exist."}`, 404, "The model does not exist.", "invalid_request_error"},
		{`{"message":"too long","object":"error"}`, 400, "too long", "invalid_request_error"},
		{`upstream connect error`, http.StatusBadGateway, "upstream connect error", "server_error"},
	} {
		var got struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(openAIErrorBody([]byte(tc.raw), tc.status), &got); err != nil {
			t.Fatalf("%s: not JSON: %v", tc.raw, err)
		}
		if got.Error.Message != tc.message || got.Error.Type != tc.typ {
			t.Errorf("%s: got %q (%s), want %q (%s)", tc.raw, got.Error.Message, got.Error.Type, tc.message, tc.typ)
		}
	}
}
