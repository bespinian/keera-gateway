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
	"github.com/redis/go-redis/v9"
)

// Which buckets a deployment gets, and what happens when its Redis is down.
// An unreachable Redis must warn and start, so Redis stays optional.

// logged runs buildLimiter with a logger whose output can be read back.
func logged(t *testing.T, cfg config.Config) (limiter, string) {
	t.Helper()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	lim, closeLimiter := buildLimiter(t.Context(), cfg, metrics.New(), log)
	t.Cleanup(closeLimiter)
	return lim, buf.String()
}

// unreachable is a Redis nothing listens on, so the connection is refused at
// once.
func unreachable() *redis.Options { return &redis.Options{Addr: "127.0.0.1:1"} }

func TestWithNoRedisTheBucketsStayInThisProcess(t *testing.T) {
	// The default: one replica needs no Redis.
	lim, logs := logged(t, config.Config{})
	if _, ok := lim.(*ratelimit.Limiter); !ok {
		t.Errorf("limiter is %T, want the in-memory one", lim)
	}
	if strings.Contains(logs, "Redis") {
		t.Errorf("a deployment that configured no Redis was told about Redis: %s", logs)
	}
}

func TestARedisThatCannotBeReachedWarnsAndTheGatewayStillStarts(t *testing.T) {
	// Limits fall back to the in-memory buckets.
	lim, logs := logged(t, config.Config{Redis: unreachable(), RedisPrefix: "keera"})
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
		{Redis: unreachable()},
	} {
		lim, _ := logged(t, cfg)
		lim.Sweep(time.Hour, time.Now())
	}
}

func TestClosingTheLimiterIsSafeInBothShapes(t *testing.T) {
	// serve defers the closer unconditionally, so the no-Redis case has to
	// return one that does nothing rather than nil.
	for _, cfg := range []config.Config{
		{},
		{Redis: unreachable()},
	} {
		_, closeLimiter := buildLimiter(context.Background(), cfg,
			metrics.New(), slog.New(slog.DiscardHandler))
		if closeLimiter == nil {
			t.Fatal("buildLimiter returned no closer; serve defers it unconditionally")
		}
		closeLimiter()
	}
}
