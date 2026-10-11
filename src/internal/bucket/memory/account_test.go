package memory

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"token-router/src/internal/bucket"
	"token-router/src/internal/config"
)

func TestMemoryAccountCleanupPreservesDebt(t *testing.T) {
	l := New(1, 1, 1, 1)
	now := time.Unix(1700000000, 0)
	l.now = func() time.Time { return now }
	idle := bucket.WithAccount(t.Context(), "idle")
	debtor := bucket.WithAccount(t.Context(), "debtor")

	for _, ctx := range []context.Context{idle, debtor} {
		if _, err := l.Allow(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.RecordUsage(debtor, 1000); err != nil {
		t.Fatal(err)
	}

	now = now.Add(2 * time.Minute)
	if _, err := l.Allow(bucket.WithAccount(t.Context(), "trigger")); err != nil {
		t.Fatal(err)
	}

	if l.shard(bucket.AccountKey(idle)).states[bucket.AccountKey(idle)] != nil || l.shard(bucket.AccountKey(debtor)).states[bucket.AccountKey(debtor)] == nil {
		t.Fatal("cleanup must remove refilled accounts and retain token debt")
	}
	if decision, err := l.Allow(debtor); err != nil || decision.Allowed {
		t.Fatalf("cleanup lost debt: %+v, %v", decision, err)
	}
}

func TestMemoryConcurrentAccountBurst(t *testing.T) {
	for _, count := range []int{1, 64} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			l := New(1, 3, 0, 0)
			now := time.Now()
			l.now = func() time.Time { return now }

			contexts := make([]context.Context, count)
			allowed := make([]atomic.Int64, count)
			var wg sync.WaitGroup
			for i := range contexts {
				contexts[i] = bucket.WithAccount(t.Context(), strconv.Itoa(i))
				for range 10 {
					wg.Go(func() {
						decision, err := l.Allow(contexts[i])
						if err != nil {
							t.Error(err)
						} else if decision.Allowed {
							allowed[i].Add(1)
						}
					})
				}
			}
			wg.Wait()

			for i := range contexts {
				if got := allowed[i].Load(); got != 3 {
					t.Errorf("account %d allowed %d requests, want 3", i, got)
				}
			}
		})
	}
}

func TestMemoryConcurrentAccountUpdatesAndClose(t *testing.T) {
	l := New(100, 100, 100, 100)
	var ticks atomic.Int64
	l.now = func() time.Time { return time.Unix(1700000000+ticks.Add(1), 0) }

	var wg sync.WaitGroup
	for i := range 64 {
		wg.Go(func() {
			ctx := bucket.WithAccount(t.Context(), strconv.Itoa(i))
			for range 10 {
				if _, err := l.Allow(ctx); err != nil && !errors.Is(err, bucket.ErrClosed) {
					t.Errorf("Allow: %v", err)
				}
				if err := l.RecordUsage(ctx, 200); err != nil && !errors.Is(err, bucket.ErrClosed) {
					t.Errorf("RecordUsage: %v", err)
				}
			}
		})
	}
	wg.Go(func() {
		for i := range 20 {
			l.Update(config.LimitValues{RequestsPerSecond: float64(i + 1), Burst: i + 1,
				TokensPerSecond: float64(i + 1), TokenBurst: i + 1})
		}
		if err := l.Close(); err != nil {
			t.Error(err)
		}
	})
	wg.Wait()

	for i := range 64 {
		ctx := bucket.WithAccount(t.Context(), strconv.Itoa(i))
		if _, err := l.Allow(ctx); !errors.Is(err, bucket.ErrClosed) {
			t.Errorf("account %d Allow after Close: %v", i, err)
		}
		if err := l.RecordUsage(ctx, 1); !errors.Is(err, bucket.ErrClosed) {
			t.Errorf("account %d RecordUsage after Close: %v", i, err)
		}
	}
}
