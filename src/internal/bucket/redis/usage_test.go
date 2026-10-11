package redis

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"token-router/src/internal/bucket"
	"token-router/src/internal/bucket/limittest"
)

func TestRedisTokenUsageSharedDebtAndExpiry(t *testing.T) {
	s := miniredis.RunT(t)
	now := time.Unix(1_700_000_000, 0)
	s.SetTime(now)
	cfg := redisConfig(s.Addr())
	cfg.TokensPerSecond, cfg.TokenBurst = 2, 4
	first, second := redisForTest(t, cfg), redisForTest(t, cfg)

	limittest.RequireDecision(t, first, true, 0)
	limittest.RequireUsage(t, first, 7)
	limittest.RequireUsage(t, second, 1)
	limittest.RequireDecision(t, first, false, 2500*time.Millisecond)
	if got := s.TTL(cfg.Redis.Key); got != 4*time.Second {
		t.Fatalf("token debt TTL=%v, want 4s", got)
	}

	s.FastForward(2 * time.Second)
	s.SetTime(now.Add(2 * time.Second))
	if !s.Exists(cfg.Redis.Key) {
		t.Fatal("key expired before token debt recovered")
	}
	limittest.RequireDecision(t, second, false, 500*time.Millisecond)
	if got := s.TTL(cfg.Redis.Key); got != 2*time.Second {
		t.Fatalf("refilled debt TTL=%v, want 2s", got)
	}

	s.FastForward(2 * time.Second)
	s.SetTime(now.Add(4 * time.Second))
	if s.Exists(cfg.Redis.Key) {
		t.Fatal("fully recovered idle key did not expire")
	}

	limittest.RequireDecision(t, second, true, 0)
	limittest.RequireUsage(t, first, 4)
	limittest.RequireDecision(t, second, false, 500*time.Millisecond)
}

func TestRedisTokenDebtPreservesRequestRefillTTL(t *testing.T) {
	s := miniredis.RunT(t)
	s.SetTime(time.Unix(1_700_000_000, 0))
	cfg := redisConfig(s.Addr())
	cfg.RequestsPerSecond, cfg.Burst = 0.01, 1
	cfg.TokensPerSecond, cfg.TokenBurst = 1, 1
	l := redisForTest(t, cfg)

	limittest.RequireDecision(t, l, true, 0)
	limittest.RequireUsage(t, l, 1)

	if got := s.TTL(cfg.Redis.Key); got != 100*time.Second {
		t.Fatalf("request refill TTL=%v, want 100s", got)
	}
}

func TestRedisTokenUsageConcurrentReplicas(t *testing.T) {
	s := miniredis.RunT(t)
	s.SetTime(time.Unix(1_700_000_000, 0))
	cfg := redisConfig(s.Addr())
	cfg.TokensPerSecond, cfg.TokenBurst = 1, 10
	limiters := []bucket.Limiter{redisForTest(t, cfg), redisForTest(t, cfg)}

	var wg sync.WaitGroup
	for i := range 30 {
		wg.Go(func() {
			if err := limiters[i%len(limiters)].RecordUsage(context.Background(), 1); err != nil {
				t.Errorf("RecordUsage: %v", err)
			}
		})
	}
	wg.Wait()

	for _, l := range limiters {
		limittest.RequireDecision(t, l, false, 21*time.Second)
	}
	if got := s.TTL(cfg.Redis.Key); got != 30*time.Second {
		t.Fatalf("concurrent debt TTL=%v, want 30s", got)
	}
}

func TestRedisTokenUsageHugeDebtExpiry(t *testing.T) {
	s := miniredis.RunT(t)
	cfg := redisConfig(s.Addr())
	cfg.TokensPerSecond, cfg.TokenBurst = 1, 1
	l := redisForTest(t, cfg)

	limittest.RequireDecision(t, l, true, 0)
	limittest.RequireUsage(t, l, math.MaxInt64)

	if got := s.TTL(cfg.Redis.Key); got != 0 {
		t.Fatalf("unrepresentable token debt must not expire: TTL=%v", got)
	}
}

func TestRedisTokenUsageFailureHidesBackendDetails(t *testing.T) {
	s := miniredis.RunT(t)
	cfg := redisConfig(s.Addr())
	cfg.TokensPerSecond, cfg.TokenBurst = 1, 1
	l := redisForTest(t, cfg)

	s.SetError("ERR credential=secret-value")
	if err := l.RecordUsage(context.Background(), 1); !errors.Is(err, bucket.ErrUnavailable) || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("usage backend failure = %v", err)
	}
}
