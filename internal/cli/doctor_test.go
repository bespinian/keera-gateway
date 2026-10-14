package cli

import (
	"context"
	"testing"
)

// An install script may read the checks as JSON and still rely on the exit
// status, so a failed check fails the command either way.
func TestDoctorFailsOnAFailedCheck(t *testing.T) {
	quiet(t)
	for _, args := range [][]string{nil, {"--json"}} {
		newFakeControl(t, map[string]any{
			"GET /v1/orgs":        oneOrg,
			"GET /v1/diagnostics": map[string]any{"checks": []any{}, "failures": 1},
		})
		if err := doctorCmd(context.Background(), args); err == nil {
			t.Errorf("doctor %v: no error for a failed check", args)
		}
	}
}
