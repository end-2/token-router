package bucket

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRedisIntegrationSharedAtomicBucket(t *testing.T) {
	address := os.Getenv("TOKEN_ROUTER_TEST_REDIS_ADDR")
	if address == "" {
		t.Skip("set TOKEN_ROUTER_TEST_REDIS_ADDR to run against a Redis server")
	}
	cfg := redisConfig(address)
	cfg.RequestsPerSecond = 0.1
	cfg.Burst = 7
	cfg.Redis.Key = fmt.Sprintf("token-router:test:%s:%d", t.Name(), time.Now().UnixNano())
	first := redisForTest(t, cfg).(*redisLimiter)
	second := redisForTest(t, cfg)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := first.client.Del(ctx, cfg.Redis.Key).Err(); err != nil {
			t.Errorf("delete test bucket: %v", err)
		}
	})
	limiters := []Limiter{first, second}
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			decision, err := limiters[i%len(limiters)].Allow(context.Background())
			if err != nil {
				t.Errorf("Allow: %v", err)
				return
			}
			if decision.Allowed {
				allowed.Add(1)
			} else if decision.RetryAfter <= 0 || decision.RetryAfter > 10*time.Second {
				t.Errorf("denied request retry duration: %v", decision.RetryAfter)
			}
		})
	}
	wg.Wait()
	if got := allowed.Load(); got != int64(cfg.Burst) {
		t.Fatalf("shared bucket allowed %d requests, want %d", got, cfg.Burst)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ttl, err := first.client.PTTL(ctx, cfg.Redis.Key).Result()
	if err != nil || ttl <= 0 || ttl > 70*time.Second {
		t.Fatalf("bucket TTL=%v, error=%v", ttl, err)
	}
}
