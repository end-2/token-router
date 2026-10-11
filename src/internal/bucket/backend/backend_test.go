package backend

import (
	"context"
	"errors"
	"math"
	"testing"

	"token-router/src/internal/bucket"
	"token-router/src/internal/bucket/memory"
	"token-router/src/internal/config"
)

func TestLimiterCancellationAndClose(t *testing.T) {
	for _, tt := range []struct {
		name string
		new  func() bucket.Limiter
	}{
		{name: "memory", new: func() bucket.Limiter { return memory.New(1, 1, 0, 0) }},
		{name: "disabled", new: func() bucket.Limiter { return &disabledLimiter{} }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			l := tt.new()

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if decision, err := l.Allow(ctx); !errors.Is(err, context.Canceled) || decision.Allowed {
				t.Fatalf("canceled Allow = %+v, %v", decision, err)
			}

			if decision, err := l.Allow(context.Background()); err != nil || !decision.Allowed {
				t.Fatalf("canceled request consumed capacity: %+v, %v", decision, err)
			}

			for range 2 {
				if err := l.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if decision, err := l.Allow(context.Background()); !errors.Is(err, bucket.ErrClosed) || decision.Allowed {
				t.Fatalf("closed Allow = %+v, %v", decision, err)
			}
		})
	}
}

func TestNewConfiguration(t *testing.T) {
	for _, tt := range []struct {
		name string
		cfg  config.RateLimit
		fail bool
	}{
		{name: "disabled", cfg: config.RateLimit{}},
		{name: "memory", cfg: config.RateLimit{Enabled: true, Backend: "memory", RequestsPerSecond: 1, Burst: 1}},
		{name: "memory with tokens", cfg: config.RateLimit{Enabled: true, Backend: "memory", RequestsPerSecond: 1, Burst: 1, TokensPerSecond: 2, TokenBurst: 3}},
		{name: "token rate only", cfg: config.RateLimit{Enabled: true, Backend: "memory", RequestsPerSecond: 1, Burst: 1, TokensPerSecond: 1}, fail: true},
		{name: "token burst only", cfg: config.RateLimit{Enabled: true, Backend: "memory", RequestsPerSecond: 1, Burst: 1, TokenBurst: 1}, fail: true},
		{name: "negative token rate", cfg: config.RateLimit{Enabled: true, Backend: "memory", RequestsPerSecond: 1, Burst: 1, TokensPerSecond: -1, TokenBurst: 1}, fail: true},
		{name: "negative token burst", cfg: config.RateLimit{Enabled: true, Backend: "memory", RequestsPerSecond: 1, Burst: 1, TokensPerSecond: 1, TokenBurst: -1}, fail: true},
		{name: "nan token rate", cfg: config.RateLimit{Enabled: true, Backend: "memory", RequestsPerSecond: 1, Burst: 1, TokensPerSecond: math.NaN(), TokenBurst: 1}, fail: true},
		{name: "infinite token rate", cfg: config.RateLimit{Enabled: true, Backend: "memory", RequestsPerSecond: 1, Burst: 1, TokensPerSecond: math.Inf(1), TokenBurst: 1}, fail: true},
		{name: "token refill overflow", cfg: config.RateLimit{Enabled: true, Backend: "memory", RequestsPerSecond: 1, Burst: 1, TokensPerSecond: math.SmallestNonzeroFloat64, TokenBurst: 1}, fail: true},
		{name: "unknown", cfg: config.RateLimit{Enabled: true, Backend: "unknown", RequestsPerSecond: 1, Burst: 1}, fail: true},
		{name: "zero rate", cfg: config.RateLimit{Enabled: true, Backend: "memory", Burst: 1}, fail: true},
		{name: "nan rate", cfg: config.RateLimit{Enabled: true, Backend: "memory", RequestsPerSecond: math.NaN(), Burst: 1}, fail: true},
		{name: "infinite rate", cfg: config.RateLimit{Enabled: true, Backend: "memory", RequestsPerSecond: math.Inf(1), Burst: 1}, fail: true},
		{name: "zero burst", cfg: config.RateLimit{Enabled: true, Backend: "memory", RequestsPerSecond: 1}, fail: true},
		{name: "overflow", cfg: config.RateLimit{Enabled: true, Backend: "memory", RequestsPerSecond: math.SmallestNonzeroFloat64, Burst: 1}, fail: true},
		{name: "invalid redis", cfg: config.RateLimit{Enabled: true, Backend: "redis", RequestsPerSecond: 1, Burst: 1}, fail: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			l, err := New(context.Background(), tt.cfg)
			if (err != nil) != tt.fail {
				t.Fatalf("New error=%v, want failure=%v", err, tt.fail)
			}
			if l != nil {
				if err := l.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New(ctx, config.RateLimit{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("New with canceled context: %v", err)
	}
}
