// Package limittest provides shared backend conformance checks for tests.
package limittest

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"token-router/src/internal/bucket"
	"token-router/src/internal/config"
)

// Factory constructs a limiter and a deterministic clock advancement function.
type Factory func(t *testing.T, rate float64, burst int, usageRate float64, usageBurst int) (bucket.Limiter, func(time.Duration))

// RequireDecision checks admission and its retry delay.
func RequireDecision(t *testing.T, l bucket.Limiter, allowed bool, retry time.Duration) {
	t.Helper()
	got, err := l.Allow(context.Background())
	if err != nil || got.Allowed != allowed || got.RetryAfter != retry {
		t.Fatalf("Allow = %+v, %v; want allowed=%v retry=%v", got, err, allowed, retry)
	}
}

// RequireUsage records usage and checks the result.
func RequireUsage(t *testing.T, l bucket.Limiter, tokens int64) {
	t.Helper()
	if err := l.RecordUsage(context.Background(), tokens); err != nil {
		t.Fatalf("RecordUsage(%d): %v", tokens, err)
	}
}

func testTokenUsageDebtAndRefill(t *testing.T, newLimiter Factory) {
	l, advance := newLimiter(t, 1000, 3, 2, 4)

	RequireDecision(t, l, true, 0)
	RequireUsage(t, l, 7)
	RequireDecision(t, l, false, 2*time.Second)
	RequireDecision(t, l, false, 2*time.Second)

	advance(1500 * time.Millisecond)
	RequireDecision(t, l, false, 500*time.Millisecond)

	advance(500 * time.Millisecond)
	RequireDecision(t, l, true, 0)
	RequireUsage(t, l, 1)
	RequireDecision(t, l, false, 500*time.Millisecond)

	advance(time.Hour)
	RequireUsage(t, l, 4)
	RequireDecision(t, l, false, 500*time.Millisecond)
}

func testTokenDenialPreservesRequestCapacity(t *testing.T, newLimiter Factory) {
	l, advance := newLimiter(t, 0.01, 1, 1, 1)
	RequireUsage(t, l, 1)
	RequireDecision(t, l, false, time.Second)
	advance(time.Second)
	RequireDecision(t, l, true, 0)
	RequireDecision(t, l, false, 100*time.Second)
}

func testTokenRetryUsesSlowerLimit(t *testing.T, newLimiter Factory) {
	l, advance := newLimiter(t, 0.5, 1, 1, 2)

	RequireDecision(t, l, true, 0)
	RequireUsage(t, l, 4)
	RequireDecision(t, l, false, 3*time.Second)

	advance(time.Second)
	RequireDecision(t, l, false, 2*time.Second)

	advance(2 * time.Second)
	RequireDecision(t, l, true, 0)
	RequireDecision(t, l, false, 2*time.Second)
}

func testTokenUsageChargedAfterAdmission(t *testing.T, newLimiter Factory) {
	l, _ := newLimiter(t, 100, 3, 1, 1)
	for range 3 {
		RequireDecision(t, l, true, 0)
	}
	RequireUsage(t, l, 1)
	RequireDecision(t, l, false, time.Second)
}

func testTokenUsageConcurrentAccounting(t *testing.T, newLimiter Factory) {
	l, _ := newLimiter(t, 100, 100, 1, 10)

	var wg sync.WaitGroup
	for range 30 {
		wg.Go(func() {
			if err := l.RecordUsage(context.Background(), 1); err != nil {
				t.Errorf("RecordUsage: %v", err)
			}
		})
	}
	wg.Wait()

	RequireDecision(t, l, false, 21*time.Second)
}

func testTokenUsageDisabled(t *testing.T, newLimiter Factory) {
	l, _ := newLimiter(t, 1, 1, 0, 0)
	RequireUsage(t, l, math.MaxInt64)
	RequireDecision(t, l, true, 0)
	RequireDecision(t, l, false, time.Second)
}

func testTokenUsageValidationCancellationAndClose(t *testing.T, newLimiter Factory) {
	l, _ := newLimiter(t, 1, 1, 1, 1)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.RecordUsage(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled usage: %v", err)
	}

	if err := l.RecordUsage(context.Background(), -1); !errors.Is(err, bucket.ErrInvalidUsage) {
		t.Fatalf("negative usage: %v", err)
	}
	RequireUsage(t, l, 0)
	RequireDecision(t, l, true, 0)

	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordUsage(context.Background(), 1); !errors.Is(err, bucket.ErrClosed) {
		t.Fatalf("closed usage: %v", err)
	}
}

func testTokenUsageHugeDebt(t *testing.T, newLimiter Factory) {
	l, _ := newLimiter(t, 1, 1, 1, 1)

	RequireUsage(t, l, math.MaxInt64)
	RequireUsage(t, l, math.MaxInt64)

	decision, err := l.Allow(context.Background())
	if err != nil || decision.Allowed || decision.RetryAfter < 100*365*24*time.Hour {
		t.Fatalf("huge debt Allow = %+v, %v", decision, err)
	}
}

func testLimitUpdatePreservesBalances(t *testing.T, newLimiter Factory) {
	l, advance := newLimiter(t, 1, 2, 1, 5)

	RequireDecision(t, l, true, 0)
	RequireDecision(t, l, true, 0)
	RequireUsage(t, l, 8)

	l.(bucket.Reconfigurable).Update(config.LimitValues{RequestsPerSecond: 2, Burst: 10, TokensPerSecond: 2, TokenBurst: 20})
	RequireDecision(t, l, false, 2*time.Second)

	advance(time.Second)
	RequireDecision(t, l, false, time.Second)

	advance(time.Second)
	RequireDecision(t, l, true, 0)
	RequireUsage(t, l, 1)
	RequireDecision(t, l, false, 500*time.Millisecond)
}

func testLimitUpdateClampsCapacityAndEnablesUsage(t *testing.T, newLimiter Factory) {
	l, _ := newLimiter(t, 1, 10, 0, 0)
	l.(bucket.Reconfigurable).Update(config.LimitValues{RequestsPerSecond: 1, Burst: 1, TokensPerSecond: 2, TokenBurst: 2})
	RequireDecision(t, l, true, 0)
	RequireDecision(t, l, false, time.Second)
	RequireUsage(t, l, 5)
	RequireDecision(t, l, false, 2*time.Second)
}

func testConcurrentLimitUpdates(t *testing.T, newLimiter Factory) {
	l, _ := newLimiter(t, 100, 100, 100, 100)

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			l.(bucket.Reconfigurable).Update(config.LimitValues{RequestsPerSecond: float64(i + 1), Burst: i + 1, TokensPerSecond: float64(i + 1), TokenBurst: i + 1})
			if _, err := l.Allow(t.Context()); err != nil {
				t.Error(err)
			}
			if err := l.RecordUsage(t.Context(), 2); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}

// Usage runs shared usage conformance checks with a deterministic clock.
func Usage(t *testing.T, newLimiter Factory) {
	t.Helper()
	t.Run("TokenUsageDebtAndRefill", func(t *testing.T) { testTokenUsageDebtAndRefill(t, newLimiter) })
	t.Run("TokenDenialPreservesRequestCapacity", func(t *testing.T) { testTokenDenialPreservesRequestCapacity(t, newLimiter) })
	t.Run("TokenRetryUsesSlowerLimit", func(t *testing.T) { testTokenRetryUsesSlowerLimit(t, newLimiter) })
	t.Run("TokenUsageChargedAfterAdmission", func(t *testing.T) { testTokenUsageChargedAfterAdmission(t, newLimiter) })
	t.Run("TokenUsageConcurrentAccounting", func(t *testing.T) { testTokenUsageConcurrentAccounting(t, newLimiter) })
	t.Run("TokenUsageDisabled", func(t *testing.T) { testTokenUsageDisabled(t, newLimiter) })
	t.Run("TokenUsageValidationCancellationAndClose", func(t *testing.T) { testTokenUsageValidationCancellationAndClose(t, newLimiter) })
	t.Run("TokenUsageHugeDebt", func(t *testing.T) { testTokenUsageHugeDebt(t, newLimiter) })
}

// Updates runs shared updates conformance checks with a deterministic clock.
func Updates(t *testing.T, newLimiter Factory) {
	t.Helper()
	t.Run("LimitUpdatePreservesBalances", func(t *testing.T) { testLimitUpdatePreservesBalances(t, newLimiter) })
	t.Run("LimitUpdateClampsCapacityAndEnablesUsage", func(t *testing.T) { testLimitUpdateClampsCapacityAndEnablesUsage(t, newLimiter) })
	t.Run("ConcurrentLimitUpdates", func(t *testing.T) { testConcurrentLimitUpdates(t, newLimiter) })
}
