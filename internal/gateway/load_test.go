package gateway

import (
	"slices"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// order is what the whole file is for, so most of it is tested through order:
// what is being asserted is not that a number was stored but that a request
// would go to a particular model because of it.

func TestLeastBusyPrefersTheEmptiestDestination(t *testing.T) {
	l := newLoads()
	stop := []func(){l.begin("a"), l.begin("a"), l.begin("b")}

	got := []string{"a", "b", "c"}
	l.order(policy.RouterModeLeastBusy, got)
	if want := []string{"c", "b", "a"}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v - two in flight, one in flight, none", got, want)
	}

	for _, release := range stop {
		release()
	}
	// Everything is back to nothing in flight, so nothing tells the three
	// apart and what is left is the order the administrator wrote.
	got = []string{"a", "b", "c"}
	l.order(policy.RouterModeLeastBusy, got)
	if want := []string{"a", "b", "c"}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v - a tie is broken by what was written", got, want)
	}
}

func TestReleasingTwiceDoesNotUncountARequest(t *testing.T) {
	l := newLoads()
	release := l.begin("a")
	release()
	release()

	if got := l.read("a", time.Now()).inflight; got != 0 {
		t.Errorf("in flight = %d, want 0 - a count that can go negative is a "+
			"destination this process believes is emptier than empty", got)
	}
}

func TestLatencyPrefersTheDestinationAnsweringFastest(t *testing.T) {
	l := newLoads()
	now := time.Now()
	l.observe("slow", 900*time.Millisecond, now)
	l.observe("quick", 80*time.Millisecond, now)
	l.observe("middling", 300*time.Millisecond, now)

	got := []string{"slow", "middling", "quick"}
	l.order(policy.RouterModeLatency, got)
	if want := []string{"quick", "middling", "slow"}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestLatencyTriesADestinationItHasNotMeasured(t *testing.T) {
	l := newLoads()
	now := time.Now()
	l.observe("measured", 50*time.Millisecond, now)

	got := []string{"measured", "fresh"}
	l.order(policy.RouterModeLatency, got)
	if want := []string{"fresh", "measured"}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v - a destination with no recent reading is worth "+
			"one request to find out about, or the first model to post a good number "+
			"keeps the traffic forever", got, want)
	}
}

func TestAnOldReadingStopsBeingAReading(t *testing.T) {
	l := newLoads()
	// Anchored so that "now" is the real one, because order is what is being
	// asserted and order reads the clock for itself.
	now := time.Now()
	l.observe("stale", 50*time.Millisecond, now.Add(-loadStale))
	l.observe("recent", 400*time.Millisecond, now)

	// 'stale' was the faster of the two and is now the one nothing knows
	// about, which puts it first: this is how a measured router notices that
	// the model it stopped sending to has changed.
	if r := l.read("stale", now); r.known {
		t.Errorf("a reading %s old is still being treated as one", loadStale)
	}
	got := []string{"recent", "stale"}
	l.order(policy.RouterModeLatency, got)
	if want := []string{"stale", "recent"}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestLatencyIsWeightedTowardsWhatJustHappened(t *testing.T) {
	l := newLoads()
	now := time.Now()
	// A destination that was fast, and has just answered slowly.
	l.observe("a", 100*time.Millisecond, now)
	l.observe("a", 900*time.Millisecond, now.Add(loadHalfLife))

	// One half-life on, the old reading is worth half of what it was: the
	// average is about halfway between the two rather than at either end.
	got := l.read("a", now.Add(loadHalfLife)).latency
	if got < 450*time.Millisecond || got > 550*time.Millisecond {
		t.Errorf("latency = %s, want about 500ms - one sample should move the average "+
			"without becoming it, and an hour-old sample should not still be holding "+
			"it down", got)
	}
}

func TestAFailedDestinationIsRankedLastInBothModes(t *testing.T) {
	l := newLoads()
	now := time.Now()
	// The trap both modes fall into without this. A destination that is down
	// answers nothing, so it measures as unmeasured and as completely idle -
	// which is the best score there is in either mode.
	l.fail("down", now)
	l.observe("up", 700*time.Millisecond, now)
	release := l.begin("up")
	defer release()

	for _, mode := range []policy.RouterMode{
		policy.RouterModeLatency, policy.RouterModeLeastBusy,
	} {
		got := []string{"down", "up"}
		l.order(mode, got)
		if want := []string{"up", "down"}; !slices.Equal(got, want) {
			t.Errorf("%s: order = %v, want %v", mode, got, want)
		}
	}
}

func TestAFailedDestinationComesBack(t *testing.T) {
	l := newLoads()
	now := time.Now()
	l.fail("down", now)

	if r := l.read("down", now.Add(loadPenalty)); r.failing {
		t.Errorf("a destination is still being ranked last %s after it failed; the "+
			"penalty is for not walking into it again, not a verdict on the model",
			loadPenalty)
	}
}

func TestAnUnmeasuredModeIsLeftAlone(t *testing.T) {
	l := newLoads()
	l.observe("b", time.Millisecond, time.Now())
	l.observe("a", time.Second, time.Now())

	// A fallback router's order is the administrator's statement about which
	// destination is wanted, and nothing measured here may reorder it.
	got := []string{"a", "b"}
	l.order(policy.RouterModeFallback, got)
	if want := []string{"a", "b"}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v - a fallback router's order is written, not "+
			"measured", got, want)
	}
}

func TestDescribeSaysNothingAboutAModeThatMeasuresNothing(t *testing.T) {
	l := newLoads()
	if got := l.describe(policy.RouterModeFallback, "a"); got != "" {
		t.Errorf("describe = %q, want empty - there is no reading behind a fallback "+
			"router's order, and printing one would invent a reason for it", got)
	}
	if got := l.describe(policy.RouterModeLeastBusy, "a"); got == "" {
		t.Error("describe said nothing about a least-busy destination; the whole point " +
			"of the column is answering why this one is first")
	}
}
