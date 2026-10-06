package store

import (
	"context"
	"time"
)

// The event log read as a map of the deployment: which clients connect to
// which models. One report carries the total, one row per client, one per
// model and one per pair. They come from a single query, so the edges always
// add up to the nodes they join.

// Cell is the set of numbers every node and edge of the map carries.
type Cell struct {
	Requests     int64 `json:"requests"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	CostMicros   int64 `json:"cost_micros"`
	// Refused is every 4xx: what a guardrail stopped, or a request that could
	// not be taken. Failed is every 5xx: what the inference plane could not
	// answer. One is working as configured and the other is a fault, so they
	// are counted apart. The request log splits them the same way.
	Refused int64 `json:"refused"`
	Failed  int64 `json:"failed"`
	// TTFTMedianMS is the median wait for the first token. A median, because
	// one very slow request should not move it.
	TTFTMedianMS int64 `json:"ttft_median_ms"`
}

// cellColumns aggregates usage_events into a Cell. cellTargets scans the same
// columns in the same order.
var cellColumns = `count(*),
	COALESCE(sum(input_tokens), 0), COALESCE(sum(output_tokens), 0),
	COALESCE(sum(cost_micros), 0),
	` + countOutcome(OutcomeRefused) + `,
	` + countOutcome(OutcomeFailed) + `,
	COALESCE(round(percentile_cont(0.5) WITHIN GROUP (ORDER BY ttft_ms)
	               FILTER (WHERE ttft_ms > 0)), 0)::bigint`

func cellTargets(c *Cell) []any {
	return []any{&c.Requests, &c.InputTokens, &c.OutputTokens, &c.CostMicros,
		&c.Refused, &c.Failed, &c.TTFTMedianMS}
}

// Flow is the traffic between one client and one model: an edge of the map.
type Flow struct {
	// Client is empty for requests whose sender did not name itself. They are
	// drawn as a client like any other.
	Client string `json:"client"`
	Alias  string `json:"alias"`
	Cell
}

// FlowReport is the whole map's arithmetic over one window.
type FlowReport struct {
	Total   Cell            `json:"total"`
	Clients map[string]Cell `json:"clients"`
	Models  map[string]Cell `json:"models"`
	Flows   []Flow          `json:"flows"`
}

// Flows aggregates one organisation's window into the four readings the map
// draws.
//
// GROUPING SETS does it in one pass. The coarser rows are computed by the
// database, not summed here, because a median cannot be added up from the
// medians of its parts.
func (s *Store) Flows(ctx context.Context, orgID string, from, to time.Time) (FlowReport, error) {
	rep := FlowReport{
		Clients: map[string]Cell{},
		Models:  map[string]Cell{},
		Flows:   []Flow{},
	}
	// grouping() is 1 for a column the row aggregates over. It is the only way
	// to tell "every client" from "the client with no name": both arrive as an
	// empty string.
	rows, err := s.pool.Query(ctx, `
		SELECT COALESCE(client, ''), COALESCE(alias, ''),
		       grouping(client), grouping(alias), `+cellColumns+`
		FROM usage_events
		WHERE ts >= $1 AND ts < $2 AND org_id = $3
		GROUP BY GROUPING SETS ((client, alias), (client), (alias), ())`,
		from, to, orgID)
	if err != nil {
		return rep, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			f                   Flow
			anyClient, anyAlias int
		)
		if err := rows.Scan(append([]any{&f.Client, &f.Alias, &anyClient, &anyAlias},
			cellTargets(&f.Cell)...)...); err != nil {
			return rep, err
		}
		switch {
		case anyClient == 1 && anyAlias == 1:
			rep.Total = f.Cell
		case anyClient == 1:
			rep.Models[f.Alias] = f.Cell
		case anyAlias == 1:
			rep.Clients[f.Client] = f.Cell
		default:
			rep.Flows = append(rep.Flows, f)
		}
	}
	return rep, rows.Err()
}

// ClientUse is one client's traffic through one key.
type ClientUse struct {
	// Client is empty for requests whose sender did not name itself.
	Client   string    `json:"client"`
	KeyID    string    `json:"key_id"`
	Requests int64     `json:"requests"`
	LastUsed time.Time `json:"last_used"`
}

// ClientUses lists which clients called with one person's keys in a window,
// and through which key, most recent first. It is the Clients screen: a
// developer's own view of what they have connected.
func (s *Store) ClientUses(ctx context.Context, orgID, userID string, from, to time.Time) ([]ClientUse, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT COALESCE(client, ''), COALESCE(key_id, ''), count(*), max(ts)
		FROM usage_events
		WHERE user_id = $1 AND org_id = $2 AND ts >= $3 AND ts < $4
		GROUP BY 1, 2
		ORDER BY 4 DESC`,
		userID, orgID, from, to)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(r row) (ClientUse, error) {
		var u ClientUse
		err := r.Scan(&u.Client, &u.KeyID, &u.Requests, &u.LastUsed)
		return u, err
	})
}
