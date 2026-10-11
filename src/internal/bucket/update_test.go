package bucket

import (
	"sync"
	"testing"
	"time"

	"token-router/src/internal/config"
)

func TestLimitUpdatePreservesBalances(t *testing.T) {
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) {
			l, advance := usageLimiterForTest(t, backend, 1, 2, 1, 5)
			requireDecision(t, l, true, 0)
			requireDecision(t, l, true, 0)
			requireUsage(t, l, 8)
			l.(Reconfigurable).Update(config.LimitValues{RequestsPerSecond: 2, Burst: 10, TokensPerSecond: 2, TokenBurst: 20})
			requireDecision(t, l, false, 2*time.Second)
			advance(time.Second)
			requireDecision(t, l, false, time.Second)
			advance(time.Second)
			requireDecision(t, l, true, 0)
			requireUsage(t, l, 1)
			requireDecision(t, l, false, 500*time.Millisecond)
		})
	}
}

func TestLimitUpdateClampsCapacityAndEnablesUsage(t *testing.T) {
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) {
			l, _ := usageLimiterForTest(t, backend, 1, 10, 0, 0)
			l.(Reconfigurable).Update(config.LimitValues{RequestsPerSecond: 1, Burst: 1, TokensPerSecond: 2, TokenBurst: 2})
			requireDecision(t, l, true, 0)
			requireDecision(t, l, false, time.Second)
			requireUsage(t, l, 5)
			requireDecision(t, l, false, 2*time.Second)
		})
	}
}

func TestConcurrentLimitUpdates(t *testing.T) {
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) {
			l, _ := usageLimiterForTest(t, backend, 100, 100, 100, 100)
			var wg sync.WaitGroup
			for i := range 20 {
				wg.Go(func() {
					l.(Reconfigurable).Update(config.LimitValues{RequestsPerSecond: float64(i + 1), Burst: i + 1, TokensPerSecond: float64(i + 1), TokenBurst: i + 1})
					if _, err := l.Allow(t.Context()); err != nil {
						t.Error(err)
					}
					if err := l.RecordUsage(t.Context(), 2); err != nil {
						t.Error(err)
					}
				})
			}
			wg.Wait()
		})
	}
}
