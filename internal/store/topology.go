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
	// Refused is what a guardrail stopped, and Failed is what the inference
	// plane could not answer. One is working as configured and the other is a
	// fault, so they are counted apart.
	Refused int64 `json:"refused"`
	Failed  int64 `json:"failed"`
	// TTFTMedianMS is the median wait for the first token. A median, because
	// one very slow request should not move it.
	TTFTMedianMS int64 `json:"ttft_median_ms"`
}

// cellColumns aggregates usage_events into a Cell. cellTargets scans the same
// columns in the same order.
const cellColumns = `count(*),
	COALESCE(sum(input_tokens), 0), COALESCE(sum(output_tokens), 0),
	COALESCE(sum(cost_micros), 0),
	count(*) FILTER (WHERE status IN (402, 403, 404, 429)),
	count(*) FILTER (WHERE status >= 500),
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

// Flows aggregates the window into the four readings the map draws.
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
		WHERE ts >= $1 AND ts < $2 AND ($3 = '' OR org_id = $3)
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
