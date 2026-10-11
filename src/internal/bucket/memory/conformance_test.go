package memory

import (
	"testing"
	"time"

	"token-router/src/internal/bucket"
	"token-router/src/internal/bucket/limittest"
)

func usageLimiterForTest(t *testing.T, rate float64, burst int, usageRate float64, usageBurst int) (bucket.Limiter, func(time.Duration)) {
	t.Helper()
	now := time.Unix(1_700_000_000, 0)
	l := New(rate, burst, usageRate, usageBurst)
	l.now = func() time.Time { return now }
	return l, func(delta time.Duration) { now = now.Add(delta) }
}
func TestTokenUsage(t *testing.T)   { limittest.Usage(t, usageLimiterForTest) }
func TestLimitUpdates(t *testing.T) { limittest.Updates(t, usageLimiterForTest) }
