package server

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/config"
	"github.com/bespinian/keera-gateway/internal/metrics"
	"github.com/bespinian/keera-gateway/internal/ratelimit"
)

// Which buckets a deployment gets, and what happens when its Redis is down.
// An unreachable Redis must warn and start, so Redis stays optional.

// logged runs buildLimiter with a logger whose output can be read back.
func logged(t *testing.T, cfg config.Config) (limiter, string, error) {
	t.Helper()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	lim, closeLimiter, err := buildLimiter(t.Context(), cfg, metrics.New(), log)
	if closeLimiter != nil {
		t.Cleanup(closeLimiter)
	}
	return lim, buf.String(), err
}

func TestWithNoRedisTheBucketsStayInThisProcess(t *testing.T) {
	// The default: one replica needs no Redis.
	lim, logs, err := logged(t, config.Config{})
	if err != nil {
		t.Fatalf("buildLimiter: %v", err)
	}
	if _, ok := lim.(*ratelimit.Limiter); !ok {
		t.Errorf("limiter is %T, want the in-memory one", lim)
	}
	if strings.Contains(logs, "Redis") {
		t.Errorf("a deployment that configured no Redis was told about Redis: %s", logs)
	}
}

func TestAnUnusableRedisURLStopsTheGateway(t *testing.T) {
	// A refusal, not a warning: an unparsable address is a typo nobody would
	// notice while the gateway works. An unreachable one is an outage.
	_, _, err := logged(t, config.Config{RedisURL: "127.0.0.1:6379"})
	if err == nil {
		t.Fatal("buildLimiter accepted an address that is not a URL")
	}
	if !strings.Contains(err.Error(), "KEERA_REDIS_URL") {
		t.Errorf("error = %q, want it to name the setting that is wrong", err)
	}
}

func TestARedisThatCannotBeReachedWarnsAndTheGatewayStillStarts(t *testing.T) {
	// Limits fall back to the in-memory buckets. Nothing listens on port 1,
	// so the connection is refused at once.
	lim, logs, err := logged(t, config.Config{
		RedisURL:    "redis://127.0.0.1:1/0",
		RedisPrefix: "keera",
	})
	if err != nil {
		t.Fatalf("an unreachable Redis stopped the gateway: %v", err)
	}
	if _, ok := lim.(*ratelimit.Redis); !ok {
		t.Fatalf("limiter is %T, want the Redis one with the local buckets under it", lim)
	}
	if !strings.Contains(logs, "cannot be reached") {
		t.Errorf("nothing was logged about the Redis being down: %s", logs)
	}
	// The address is the one thing an operator needs in order to fix it.
	if !strings.Contains(logs, "127.0.0.1:1") {
		t.Errorf("the warning does not say which address failed: %s", logs)
	}

	// It still decides requests, from the local buckets.
	now := time.Now()
	reqs := []ratelimit.Requirement{{Key: "org|rpm", PerMinute: 2, Take: true}}
	for i := range 2 {
		if got := lim.Admit(reqs, now); got != -1 {
			t.Fatalf("request %d was refused (%d) by a degraded limiter", i, got)
		}
	}
	if got := lim.Admit(reqs, now); got != 0 {
		t.Errorf("Admit = %d, want 0: the limit still has to bind locally", got)
	}
}

func TestTheLimiterIsAlwaysSweepable(t *testing.T) {
	// Both shapes go to the sweep loop, which bounds their memory.
	for _, cfg := range []config.Config{
		{},
		{RedisURL: "redis://127.0.0.1:1/0"},
	} {
		lim, _, err := logged(t, cfg)
		if err != nil {
			t.Fatalf("buildLimiter: %v", err)
		}
		lim.Sweep(time.Hour, time.Now())
	}
}

func TestClosingTheLimiterIsSafeInBothShapes(t *testing.T) {
	// serve defers the closer unconditionally, so the no-Redis case has to
	// return one that does nothing rather than nil.
	for _, cfg := range []config.Config{
		{},
		{RedisURL: "redis://127.0.0.1:1/0"},
	} {
		_, closeLimiter, err := buildLimiter(context.Background(), cfg,
			metrics.New(), slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatalf("buildLimiter: %v", err)
		}
		if closeLimiter == nil {
			t.Fatal("buildLimiter returned no closer; serve defers it unconditionally")
		}
		closeLimiter()
	}
}
