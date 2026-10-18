package store

import (
	"testing"
	"time"
)

func TestDayWindowWidensOnlyWindowsDrawnByTheDay(t *testing.T) {
	now := time.Date(2026, 10, 18, 14, 30, 0, 0, time.UTC)
	midnight := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)

	// Two days or less is the request log, as it is.
	from, to, daily := DayWindow(now.Add(-48*time.Hour), now)
	if daily || !from.Equal(now.Add(-48*time.Hour)) || !to.Equal(now) {
		t.Errorf("48h: %v..%v daily=%v, want it unchanged", from, to, daily)
	}

	// Longer covers whole days, today so far included.
	from, to, daily = DayWindow(now.Add(-7*24*time.Hour), now)
	if !daily || !from.Equal(midnight.AddDate(0, 0, -7)) || !to.Equal(midnight.AddDate(0, 0, 1)) {
		t.Errorf("7d: %v..%v daily=%v, want whole days through today", from, to, daily)
	}

	// Doing it again changes nothing.
	again, againTo, _ := DayWindow(from, to)
	if !again.Equal(from) || !againTo.Equal(to) {
		t.Errorf("widened twice: %v..%v, want %v..%v", again, againTo, from, to)
	}

	// Another zone gives the same UTC days.
	zurich := time.FixedZone("CEST", 2*60*60)
	from, to, _ = DayWindow(now.Add(-7*24*time.Hour).In(zurich), now.In(zurich))
	if from.Location() != time.UTC || to.Location() != time.UTC ||
		!to.Equal(midnight.AddDate(0, 0, 1)) {
		t.Errorf("from another zone: %v..%v, want UTC days", from, to)
	}
}

func TestDailyTotalsCountWhatTheReportsCount(t *testing.T) {
	day := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	sums := map[dailyKey]*dailySum{}
	for _, e := range []Event{
		{TS: day.Add(time.Hour), OrgID: "o", KeyID: "k", Alias: "m", Status: 200,
			InputTokens: 10, OutputTokens: 5, CostMicros: 7},
		{TS: day.Add(3 * time.Hour), OrgID: "o", KeyID: "k", Alias: "m", Status: 429},
		{TS: day.Add(2 * time.Hour), OrgID: "o", KeyID: "k", Alias: "m", Status: 502,
			CostMicros: 1},
		{TS: day.Add(25 * time.Hour), OrgID: "o", KeyID: "k", Alias: "m", Status: 200},
	} {
		addDaily(sums, e)
	}
	if len(sums) != 2 {
		t.Fatalf("%d rows, want one per day", len(sums))
	}
	d := sums[dailyKey{day: day, org: "o", key: "k", alias: "m"}]
	if d == nil {
		t.Fatal("no row for the first day")
	}
	if d.requests != 3 || d.refused != 1 || d.failed != 1 || d.input != 10 || d.output != 5 ||
		d.cost != 8 || !d.last.Equal(day.Add(3*time.Hour)) {
		t.Errorf("first day = %+v", *d)
	}
}
