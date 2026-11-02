package gateway

import (
	"net/http"
	"strings"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// filterOutside says why a request may not pass its filters: one runs on a
// model outside the locations the key's guardrails allow. A filter reads the
// request before anything is redacted from it, so it would carry the prompt
// there in full. It is "" when every filter stays inside. Refusing is safer
// than skipping the filter, which the guardrails also asked for.
func (s *Server) filterOutside(res *policy.Resolved, filters []string) string {
	if res.AllowedLocations == nil {
		return ""
	}
	for _, alias := range filters {
		f, ok := s.src.Filter(res.Key.OrgID, alias)
		if !ok || !f.UsesModel() {
			continue
		}
		m, ok := s.src.Model(res.Key.OrgID, f.Model)
		if ok && res.AllowsLocation(m.Location) {
			continue
		}
		where := "an unknown location"
		if ok && m.Location != "" {
			where = "'" + m.Location + "'"
		}
		return "the filter '" + alias + "' runs on the model '" + f.Model + "', in " + where +
			", but this key's guardrails allow only models in " +
			strings.Join(res.AllowedLocations, ", ") + ". An administrator can point the " +
			"filter at a model inside them"
	}
	return ""
}

// locationRefusal refuses a request whose filters would leave the allowed
// locations.
func locationRefusal(msg string) refusal {
	return refusal{
		status: http.StatusForbidden,
		typ:    "permission_error", code: "location_not_allowed",
		msg:    msg,
		advise: true,
	}
}
