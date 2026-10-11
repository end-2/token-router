package redis

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"token-router/src/internal/bucket"
	"token-router/src/internal/bucket/limittest"
)

func usageLimiterForTest(t *testing.T, rate float64, burst int, usageRate float64, usageBurst int) (bucket.Limiter, func(time.Duration)) {
	t.Helper()
	now := time.Unix(1_700_000_000, 0)
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
func TestTokenUsage(t *testing.T)   { limittest.Usage(t, usageLimiterForTest) }
func TestLimitUpdates(t *testing.T) { limittest.Updates(t, usageLimiterForTest) }
