package store

import (
	"cmp"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// usage_daily holds the totals of usage_events per organisation, key, model,
// client and UTC day. It is written in the same batch as the events, so the
// two agree, and kept longer, so reports over more than two days still work
// after retention.

// dailyKey is one row of usage_daily. Missing ids are "", as in the table.
type dailyKey struct {
	day                                    time.Time
	org, key, alias, client, project, user string
}

// dailySum is what one batch adds to a row.
type dailySum struct {
	requests, refused, failed int64
	input, output             int64
	cost, list                int64
	last                      time.Time
}

// addDaily counts one written usage row into sums.
func addDaily(sums map[dailyKey]*dailySum, e Event) {
	k := dailyKey{
		policy.PeriodDay.Start(e.TS), e.OrgID, e.KeyID, e.Alias, e.Client,
		e.ProjectID, e.UserID,
	}
	d := sums[k]
	if d == nil {
		d = &dailySum{}
		sums[k] = d
	}
	d.requests++
	switch {
	case e.Status >= 500:
		d.failed++
	case e.Status >= 400:
		d.refused++
	}
	d.input += int64(e.InputTokens)
	d.output += int64(e.OutputTokens)
	d.cost += e.CostMicros
	d.list += e.ListCostMicros
	if e.TS.After(d.last) {
		d.last = e.TS
	}
}

// queueDaily adds sums to usage_daily, sorted so every replica locks rows in
// the same order.
func queueDaily(batch *pgx.Batch, sums map[dailyKey]*dailySum) {
	keys := slices.SortedFunc(maps.Keys(sums), func(a, b dailyKey) int {
		return cmp.Or(
			cmp.Compare(a.org, b.org),
			a.day.Compare(b.day),
			cmp.Compare(a.key, b.key),
			cmp.Compare(a.alias, b.alias),
			cmp.Compare(a.client, b.client),
			cmp.Compare(a.project, b.project),
			cmp.Compare(a.user, b.user),
		)
	})
	for _, k := range keys {
		d := sums[k]
		batch.Queue(`INSERT INTO usage_daily (day, org_id, key_id, alias, client, project_id,
			user_id, requests, refused, failed, input_tokens, output_tokens, cost_micros,
			list_cost_micros, last_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
			ON CONFLICT (org_id, day, key_id, alias, client, project_id, user_id) DO UPDATE SET
				requests = usage_daily.requests + EXCLUDED.requests,
				refused = usage_daily.refused + EXCLUDED.refused,
				failed = usage_daily.failed + EXCLUDED.failed,
				input_tokens = usage_daily.input_tokens + EXCLUDED.input_tokens,
				output_tokens = usage_daily.output_tokens + EXCLUDED.output_tokens,
				cost_micros = usage_daily.cost_micros + EXCLUDED.cost_micros,
				list_cost_micros = usage_daily.list_cost_micros + EXCLUDED.list_cost_micros,
				last_at = GREATEST(usage_daily.last_at, EXCLUDED.last_at)`,
			k.day, k.org, k.key, k.alias, k.client, k.project, k.user,
			d.requests, d.refused, d.failed, d.input, d.output, d.cost,
			d.list, d.last)
	}
}

// dailyAfter is the longest window still read from usage_events. It matches
// chartBucket, so a window drawn by the day is counted by the day.
const dailyAfter = 48 * time.Hour

// DayWindow reports whether a report over from..to reads usage_daily, and if
// so widens the window to whole UTC days, so a window ending now includes
// today. Applying it twice changes nothing.
func DayWindow(from, to time.Time) (time.Time, time.Time, bool) {
	if to.Sub(from) <= dailyAfter {
		return from, to, false
	}
	end := policy.PeriodDay.Start(to)
	if end.Before(to) {
		end = end.AddDate(0, 0, 1)
	}
	return policy.PeriodDay.Start(from), end, true
}

// usageSource is where a report counts from: usage_events, or usage_daily for
// a window DayWindow widened. Both count the same, so they agree on any whole
// day.
type usageSource struct {
	daily bool
	table string
	// window is the condition on $1 and $2.
	window string
	// all, served, refused and failed count requests.
	all, served, refused, failed string
	// at is a row's time, for a chart's bucket.
	at string
}

var (
	rawSource = usageSource{
		table:   "usage_events",
		window:  "ts >= $1 AND ts < $2",
		all:     "count(*)",
		served:  "count(*) FILTER (WHERE status < 400)",
		refused: countOutcome(OutcomeRefused),
		failed:  countOutcome(OutcomeFailed),
		at:      "ts",
	}
	dailySource = usageSource{
		daily:   true,
		table:   "usage_daily",
		window:  "day >= $1::date AND day < $2::date",
		all:     "COALESCE(sum(requests), 0)",
		served:  "COALESCE(sum(requests - refused - failed), 0)",
		refused: "COALESCE(sum(refused), 0)",
		failed:  "COALESCE(sum(failed), 0)",
		at:      "day::timestamptz",
	}
)

// sourceFor picks the source for a report over from..to, and the window it
// then covers.
func sourceFor(from, to time.Time) (usageSource, time.Time, time.Time) {
	from, to, daily := DayWindow(from, to)
	if daily {
		return dailySource, from, to
	}
	return rawSource, from, to
}

// group is the column a report grouped by groupBy reads.
func (src usageSource) group(groupBy string) string {
	if src.daily && groupBy == "day" {
		return "to_char(day, 'YYYY-MM-DD')"
	}
	return groupColumns[groupBy]
}

// lastBucket is the time of a chart's last bucket. A widened window ends at
// the midnight after it, which is not a day of its own.
func (src usageSource) lastBucket(to time.Time) time.Time {
	if src.daily {
		return to.AddDate(0, 0, -1)
	}
	return to
}
