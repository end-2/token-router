package bucket

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"token-router/src/internal/config"
)

func TestAccountIsolationAcrossBackends(t *testing.T) {
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) {
			cfg := config.Defaults().RateLimit
			cfg.Backend, cfg.RequestsPerSecond, cfg.Burst = backend, .001, 2
			cfg.TokensPerSecond, cfg.TokenBurst = .001, 3
			var redis *miniredis.Miniredis
			if backend == "redis" {
				redis = miniredis.RunT(t)
				cfg.Redis.Address = redis.Addr()
			}
			limiter, err := New(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := limiter.Close(); err != nil {
					t.Error(err)
				}
			})
			alice := WithAccount(t.Context(), "alice-secret")
			bob := WithAccount(t.Context(), "bob-secret")
			allow := func(ctx context.Context, want bool) {
				t.Helper()
				decision, err := limiter.Allow(ctx)
				if err != nil || decision.Allowed != want {
					t.Fatalf("allow: %+v, %v; want %v", decision, err, want)
				}
			}
			allow(alice, true)
			if err := limiter.RecordUsage(alice, 5); err != nil {
				t.Fatal(err)
			}
			allow(alice, false)
			allow(bob, true)
			allow(bob, true)
			allow(bob, false)
			allow(t.Context(), true)
			limiter.Update(config.LimitValues{RequestsPerSecond: .001, Burst: 3, TokensPerSecond: .001, TokenBurst: 4})
			allow(alice, false)
			allow(bob, false)
			if redis != nil {
				for _, key := range redis.Keys() {
					if strings.Contains(key, "secret") {
						t.Fatal("raw account identifier stored in Redis key")
					}
				}
			}
		})
	}
}

func TestMemoryAccountCleanupPreservesDebt(t *testing.T) {
	l := newMemory(1, 1, 1, 1)
	now := time.Unix(1700000000, 0)
	l.now = func() time.Time { return now }
	idle := WithAccount(t.Context(), "idle")
	debtor := WithAccount(t.Context(), "debtor")
	for _, ctx := range []context.Context{idle, debtor} {
		if _, err := l.Allow(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.RecordUsage(debtor, 1000); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := l.Allow(WithAccount(t.Context(), "trigger")); err != nil {
		t.Fatal(err)
	}
	if l.states[accountKey(idle)] != nil || l.states[accountKey(debtor)] == nil {
		t.Fatal("cleanup must remove refilled accounts and retain token debt")
	}
	if decision, err := l.Allow(debtor); err != nil || decision.Allowed {
		t.Fatalf("cleanup lost debt: %+v, %v", decision, err)
	}
}
