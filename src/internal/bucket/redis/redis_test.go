package redis

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"token-router/src/internal/bucket"
	"token-router/src/internal/config"
)

func redisConfig(address string) config.RateLimit {
	return config.RateLimit{
		Enabled: true, Backend: "redis", RequestsPerSecond: 2, Burst: 3,
		Redis: config.Redis{
			Address: address, Key: "test:bucket", DialTimeout: time.Second,
			ReadTimeout: time.Second, WriteTimeout: time.Second,
		},
	}
}

func redisForTest(t *testing.T, cfg config.RateLimit) bucket.Limiter {
	t.Helper()
	l, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := l.Close(); err != nil {
			t.Error(err)
		}
	})
	return l
}

func TestRedisSharedRefillAndExpiry(t *testing.T) {
	s := miniredis.RunT(t)
	now := time.Unix(1_700_000_000, 0)
	s.SetTime(now)
	cfg := redisConfig(s.Addr())
	first := redisForTest(t, cfg)
	second := redisForTest(t, cfg)
	for _, l := range []bucket.Limiter{first, second, first} {
		if got, err := l.Allow(context.Background()); err != nil || !got.Allowed {
			t.Fatalf("initial burst = %+v, %v", got, err)
		}
	}
	for range 3 {
		if got, err := second.Allow(context.Background()); err != nil || got.Allowed || got.RetryAfter != 500*time.Millisecond {
			t.Fatalf("shared empty bucket = %+v, %v", got, err)
		}
	}
	s.SetTime(now.Add(250 * time.Millisecond))
	if got, err := first.Allow(context.Background()); err != nil || got.Allowed || got.RetryAfter != 250*time.Millisecond {
		t.Fatalf("partial refill = %+v, %v", got, err)
	}
	s.SetTime(now.Add(500 * time.Millisecond))
	if got, err := second.Allow(context.Background()); err != nil || !got.Allowed {
		t.Fatalf("refill = %+v, %v", got, err)
	}
	if ttl := s.TTL(cfg.Redis.Key); ttl != 1500*time.Millisecond {
		t.Fatalf("TTL=%v, want 1.5s", ttl)
	}
	s.FastForward(2 * time.Second)
	s.SetTime(now.Add(2500 * time.Millisecond))
	if s.Exists(cfg.Redis.Key) {
		t.Fatal("idle bucket did not expire")
	}
	for range cfg.Burst {
		if got, err := first.Allow(context.Background()); err != nil || !got.Allowed {
			t.Fatalf("expired bucket refill = %+v, %v", got, err)
		}
	}
}

func TestRedisConcurrentSharedBurst(t *testing.T) {
	s := miniredis.RunT(t)
	s.SetTime(time.Unix(1_700_000_000, 0))
	cfg := redisConfig(s.Addr())
	limiters := []bucket.Limiter{redisForTest(t, cfg), redisForTest(t, cfg)}
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := range 30 {
		wg.Go(func() {
			got, err := limiters[i%len(limiters)].Allow(context.Background())
			if err != nil {
				t.Errorf("Allow: %v", err)
			}
			if got.Allowed {
				allowed.Add(1)
			}
		})
	}
	wg.Wait()
	if got := allowed.Load(); got != int64(cfg.Burst) {
		t.Fatalf("allowed=%d, want %d", got, cfg.Burst)
	}
}

func TestRedisAuthenticationDatabaseAndKey(t *testing.T) {
	s := miniredis.RunT(t)
	s.RequireUserAuth("router", "test-password")
	cfg := redisConfig(s.Addr())
	cfg.Redis.Username = "router"
	cfg.Redis.Password = "test-password"
	cfg.Redis.DB = 2
	cfg.Redis.Key = "configured:key"
	l := redisForTest(t, cfg)
	if got, err := l.Allow(context.Background()); err != nil || !got.Allowed {
		t.Fatalf("configured Redis Allow = %+v, %v", got, err)
	}
	if s.Exists(cfg.Redis.Key) {
		t.Fatal("bucket was written to the default database")
	}
	s.Select(cfg.Redis.DB)
	if !s.Exists(cfg.Redis.Key) {
		t.Fatal("bucket missing from configured database and key")
	}
}

func TestRedisFailureCancellationAndClose(t *testing.T) {
	s := miniredis.RunT(t)
	l := redisForTest(t, redisConfig(s.Addr()))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := l.Allow(ctx); got.Allowed || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Allow = %+v, %v", got, err)
	}
	if s.Exists("test:bucket") {
		t.Fatal("canceled request modified bucket")
	}
	s.SetError("ERR credential=secret-value")
	if got, err := l.Allow(context.Background()); got.Allowed || !errors.Is(err, bucket.ErrUnavailable) || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("backend failure = %+v, %v", got, err)
	}
	s.SetError("")
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := l.Allow(context.Background()); got.Allowed || !errors.Is(err, bucket.ErrClosed) {
		t.Fatalf("closed Allow = %+v, %v", got, err)
	}
}

func TestRedisStartupFailureHidesBackendDetails(t *testing.T) {
	s := miniredis.RunT(t)
	s.SetError("ERR credential=secret-value")
	_, err := New(context.Background(), redisConfig(s.Addr()))
	if !errors.Is(err, bucket.ErrUnavailable) || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("startup failure: %v", err)
	}
}

func TestRedisStartupHonorsDeadline(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	}()
	// An accepted TCP connection without a Redis response exercises socket deadlines.
	done := make(chan struct{})
	defer close(done)
	var wg sync.WaitGroup
	wg.Go(func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		<-done
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(wg.Wait)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = New(ctx, redisConfig(listener.Addr().String()))
	if err == nil {
		t.Fatal("expected startup deadline error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("startup ignored caller deadline: %v", elapsed)
	}
}
