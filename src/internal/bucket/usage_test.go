package bucket

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func usageLimiterForTest(t *testing.T, backend string, rate float64, burst int, usageRate float64, usageBurst int) (Limiter, func(time.Duration)) {
	t.Helper()
	now := time.Unix(1_700_000_000, 0)
	if backend == "memory" {
		l := newMemory(rate, burst, usageRate, usageBurst)
		l.now = func() time.Time { return now }
		return l, func(delta time.Duration) { now = now.Add(delta) }
	}
	s := miniredis.RunT(t)
	s.SetTime(now)
	cfg := redisConfig(s.Addr())
	cfg.RequestsPerSecond, cfg.Burst = rate, burst
	cfg.TokensPerSecond, cfg.TokenBurst = usageRate, usageBurst
	return redisForTest(t, cfg), func(delta time.Duration) {
		now = now.Add(delta)
		s.SetTime(now)
	}
}

func requireDecision(t *testing.T, l Limiter, allowed bool, retry time.Duration) {
	t.Helper()
	got, err := l.Allow(context.Background())
	if err != nil || got.Allowed != allowed || got.RetryAfter != retry {
		t.Fatalf("Allow = %+v, %v; want allowed=%v retry=%v", got, err, allowed, retry)
	}
}

func requireUsage(t *testing.T, l Limiter, tokens int64) {
	t.Helper()
	if err := l.RecordUsage(context.Background(), tokens); err != nil {
		t.Fatalf("RecordUsage(%d): %v", tokens, err)
	}
}

func TestTokenUsageDebtAndRefill(t *testing.T) {
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) {
			l, advance := usageLimiterForTest(t, backend, 1000, 3, 2, 4)
			requireDecision(t, l, true, 0)
			requireUsage(t, l, 7)
			requireDecision(t, l, false, 2*time.Second)
			requireDecision(t, l, false, 2*time.Second)
			advance(1500 * time.Millisecond)
			requireDecision(t, l, false, 500*time.Millisecond)
			advance(500 * time.Millisecond)
			requireDecision(t, l, true, 0)
			requireUsage(t, l, 1)
			requireDecision(t, l, false, 500*time.Millisecond)
			advance(time.Hour)
			requireUsage(t, l, 4)
			requireDecision(t, l, false, 500*time.Millisecond)
		})
	}
}

func TestTokenDenialPreservesRequestCapacity(t *testing.T) {
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) {
			l, advance := usageLimiterForTest(t, backend, 0.01, 1, 1, 1)
			requireUsage(t, l, 1)
			requireDecision(t, l, false, time.Second)
			advance(time.Second)
			requireDecision(t, l, true, 0)
			requireDecision(t, l, false, 100*time.Second)
		})
	}
}

func TestTokenRetryUsesSlowerLimit(t *testing.T) {
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) {
			l, advance := usageLimiterForTest(t, backend, 0.5, 1, 1, 2)
			requireDecision(t, l, true, 0)
			requireUsage(t, l, 4)
			requireDecision(t, l, false, 3*time.Second)
			advance(time.Second)
			requireDecision(t, l, false, 2*time.Second)
			advance(2 * time.Second)
			requireDecision(t, l, true, 0)
			requireDecision(t, l, false, 2*time.Second)
		})
	}
}

func TestTokenUsageChargedAfterAdmission(t *testing.T) {
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) {
			l, _ := usageLimiterForTest(t, backend, 100, 3, 1, 1)
			for range 3 {
				requireDecision(t, l, true, 0)
			}
			requireUsage(t, l, 1)
			requireDecision(t, l, false, time.Second)
		})
	}
}

func TestTokenUsageConcurrentAccounting(t *testing.T) {
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) {
			l, _ := usageLimiterForTest(t, backend, 100, 100, 1, 10)
			var wg sync.WaitGroup
			for range 30 {
				wg.Go(func() {
					if err := l.RecordUsage(context.Background(), 1); err != nil {
						t.Errorf("RecordUsage: %v", err)
					}
				})
			}
			wg.Wait()
			requireDecision(t, l, false, 21*time.Second)
		})
	}
}

func TestTokenUsageDisabled(t *testing.T) {
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) {
			l, _ := usageLimiterForTest(t, backend, 1, 1, 0, 0)
			requireUsage(t, l, math.MaxInt64)
			requireDecision(t, l, true, 0)
			requireDecision(t, l, false, time.Second)
		})
	}
	l := &disabledLimiter{}
	requireUsage(t, l, math.MaxInt64)
	requireDecision(t, l, true, 0)
}

func TestTokenUsageValidationCancellationAndClose(t *testing.T) {
	for _, backend := range []string{"memory", "redis", "disabled"} {
		t.Run(backend, func(t *testing.T) {
			var l Limiter
			if backend == "disabled" {
				l = &disabledLimiter{}
			} else {
				l, _ = usageLimiterForTest(t, backend, 1, 1, 1, 1)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := l.RecordUsage(ctx, 1); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled usage: %v", err)
			}
			if err := l.RecordUsage(context.Background(), -1); !errors.Is(err, ErrInvalidUsage) {
				t.Fatalf("negative usage: %v", err)
			}
			requireUsage(t, l, 0)
			requireDecision(t, l, true, 0)
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}
			if err := l.RecordUsage(context.Background(), 1); !errors.Is(err, ErrClosed) {
				t.Fatalf("closed usage: %v", err)
			}
		})
	}
}

func TestRedisTokenUsageSharedDebtAndExpiry(t *testing.T) {
	s := miniredis.RunT(t)
	now := time.Unix(1_700_000_000, 0)
	s.SetTime(now)
	cfg := redisConfig(s.Addr())
	cfg.TokensPerSecond, cfg.TokenBurst = 2, 4
	first, second := redisForTest(t, cfg), redisForTest(t, cfg)
	requireDecision(t, first, true, 0)
	requireUsage(t, first, 7)
	requireUsage(t, second, 1)
	requireDecision(t, first, false, 2500*time.Millisecond)
	if got := s.TTL(cfg.Redis.Key); got != 4*time.Second {
		t.Fatalf("token debt TTL=%v, want 4s", got)
	}
	s.FastForward(2 * time.Second)
	s.SetTime(now.Add(2 * time.Second))
	if !s.Exists(cfg.Redis.Key) {
		t.Fatal("key expired before token debt recovered")
	}
	requireDecision(t, second, false, 500*time.Millisecond)
	if got := s.TTL(cfg.Redis.Key); got != 2*time.Second {
		t.Fatalf("refilled debt TTL=%v, want 2s", got)
	}
	s.FastForward(2 * time.Second)
	s.SetTime(now.Add(4 * time.Second))
	if s.Exists(cfg.Redis.Key) {
		t.Fatal("fully recovered idle key did not expire")
	}
	requireDecision(t, second, true, 0)
	requireUsage(t, first, 4)
	requireDecision(t, second, false, 500*time.Millisecond)
}

func TestRedisTokenDebtPreservesRequestRefillTTL(t *testing.T) {
	s := miniredis.RunT(t)
	s.SetTime(time.Unix(1_700_000_000, 0))
	cfg := redisConfig(s.Addr())
	cfg.RequestsPerSecond, cfg.Burst = 0.01, 1
	cfg.TokensPerSecond, cfg.TokenBurst = 1, 1
	l := redisForTest(t, cfg)
	requireDecision(t, l, true, 0)
	requireUsage(t, l, 1)
	if got := s.TTL(cfg.Redis.Key); got != 100*time.Second {
		t.Fatalf("request refill TTL=%v, want 100s", got)
	}
}

func TestRedisTokenUsageConcurrentReplicas(t *testing.T) {
	s := miniredis.RunT(t)
	s.SetTime(time.Unix(1_700_000_000, 0))
	cfg := redisConfig(s.Addr())
	cfg.TokensPerSecond, cfg.TokenBurst = 1, 10
	limiters := []Limiter{redisForTest(t, cfg), redisForTest(t, cfg)}
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
		requireDecision(t, l, false, 21*time.Second)
	}
	if got := s.TTL(cfg.Redis.Key); got != 30*time.Second {
		t.Fatalf("concurrent debt TTL=%v, want 30s", got)
	}
}

func TestTokenUsageHugeDebt(t *testing.T) {
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) {
			l, _ := usageLimiterForTest(t, backend, 1, 1, 1, 1)
			requireUsage(t, l, math.MaxInt64)
			requireUsage(t, l, math.MaxInt64)
			decision, err := l.Allow(context.Background())
			if err != nil || decision.Allowed || decision.RetryAfter < 100*365*24*time.Hour {
				t.Fatalf("huge debt Allow = %+v, %v", decision, err)
			}
		})
	}
	s := miniredis.RunT(t)
	cfg := redisConfig(s.Addr())
	cfg.TokensPerSecond, cfg.TokenBurst = 1, 1
	l := redisForTest(t, cfg)
	requireDecision(t, l, true, 0)
	requireUsage(t, l, math.MaxInt64)
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
	if err := l.RecordUsage(context.Background(), 1); !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("usage backend failure = %v", err)
	}
}
