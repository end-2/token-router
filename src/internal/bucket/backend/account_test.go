package backend

import (
	"context"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"token-router/src/internal/bucket"
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

			alice := bucket.WithAccount(t.Context(), "alice-secret")
			bob := bucket.WithAccount(t.Context(), "bob-secret")
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
