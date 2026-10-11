package memory

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"token-router/src/internal/bucket"
)

func TestMemoryRefill(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := New(2, 2, 0, 0)
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
	l := New(10, 7, 0, 0)
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

func TestMemoryConcurrentClose(t *testing.T) {
	l := New(10, 10, 0, 0)
	var wg sync.WaitGroup
	for range 30 {
		wg.Go(func() {
			if _, err := l.Allow(context.Background()); err != nil && !errors.Is(err, bucket.ErrClosed) {
				t.Errorf("concurrent Allow: %v", err)
			}
		})
		wg.Go(func() {
			if err := l.RecordUsage(context.Background(), 1); err != nil && !errors.Is(err, bucket.ErrClosed) {
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
	if _, err := l.Allow(context.Background()); !errors.Is(err, bucket.ErrClosed) {
		t.Fatalf("Allow after concurrent Close: %v", err)
	}
}
