package bucket

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"token-router/src/internal/config"
)

func TestMemoryRefill(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newMemory(2, 2, 0, 0)
	l.now = func() time.Time { return now }

	for _, tt := range []struct {
		name    string
		advance time.Duration
		allowed bool
		retry   time.Duration
	}{
		{name: "initial first", allowed: true},
		{name: "initial second", allowed: true},
		{name: "empty", retry: 500 * time.Millisecond},
		{name: "repeated denial", retry: 500 * time.Millisecond},
		{name: "partial refill", advance: 250 * time.Millisecond, retry: 250 * time.Millisecond},
		{name: "one token refilled", advance: 250 * time.Millisecond, allowed: true},
		{name: "refill consumed", retry: 500 * time.Millisecond},
		{name: "long idle", advance: time.Hour, allowed: true},
		{name: "idle burst", allowed: true},
		{name: "idle capped", retry: 500 * time.Millisecond},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now = now.Add(tt.advance)
			got, err := l.Allow(context.Background())
			if err != nil || got.Allowed != tt.allowed || got.RetryAfter != tt.retry {
				t.Fatalf("Allow = %+v, %v; want allowed=%v retry=%v", got, err, tt.allowed, tt.retry)
			}
		})
	}
}

func TestMemoryConcurrentBurst(t *testing.T) {
	l := newMemory(10, 7, 0, 0)
	now := time.Now()
	l.now = func() time.Time { return now }
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			decision, err := l.Allow(context.Background())
			if err != nil {
				t.Errorf("Allow: %v", err)
			}
			if decision.Allowed {
				allowed.Add(1)
			}
		})
	}
	wg.Wait()
	if got := allowed.Load(); got != 7 {
		t.Fatalf("allowed=%d, want 7", got)
	}
}

func TestLimiterCancellationAndClose(t *testing.T) {
	for _, tt := range []struct {
		name string
		new  func() Limiter
	}{
		{name: "memory", new: func() Limiter { return newMemory(1, 1, 0, 0) }},
		{name: "disabled", new: func() Limiter { return &disabledLimiter{} }},
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
			if decision, err := l.Allow(context.Background()); !errors.Is(err, ErrClosed) || decision.Allowed {
				t.Fatalf("closed Allow = %+v, %v", decision, err)
			}
		})
	}
}

func TestMemoryConcurrentClose(t *testing.T) {
	l := newMemory(10, 10, 0, 0)
	var wg sync.WaitGroup
	for range 30 {
		wg.Go(func() {
			if _, err := l.Allow(context.Background()); err != nil && !errors.Is(err, ErrClosed) {
				t.Errorf("concurrent Allow: %v", err)
			}
		})
		wg.Go(func() {
			if err := l.RecordUsage(context.Background(), 1); err != nil && !errors.Is(err, ErrClosed) {
				t.Errorf("concurrent RecordUsage: %v", err)
			}
		})
		wg.Go(func() {
			if err := l.Close(); err != nil {
				t.Errorf("concurrent Close: %v", err)
			}
		})
	}
	wg.Wait()
	if _, err := l.Allow(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Allow after concurrent Close: %v", err)
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
