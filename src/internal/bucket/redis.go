package bucket

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"token-router/src/internal/config"
)

// Redis TIME keeps replicas on one clock. A key may expire only after an idle
// bucket could refill completely, so expiry never grants capacity early.
var tokenScript = redis.NewScript(`
local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + tonumber(clock[2]) / 1000
local rate = tonumber(ARGV[1])
local capacity = tonumber(ARGV[2])
local ttl = tonumber(ARGV[3])
local state = redis.call('HMGET', KEYS[1], 'tokens', 'updated_at')
local tokens = tonumber(state[1]) or capacity
local updated = tonumber(state[2]) or now
now = math.max(now, updated)
tokens = math.min(capacity, tokens + (now - updated) * rate / 1000)
local allowed = 0
local retry = 0
if tokens >= 1 then
    tokens = tokens - 1
    allowed = 1
else
    retry = math.max(1, math.ceil((1 - tokens) / rate * 1000))
end
redis.call('HSET', KEYS[1], 'tokens', string.format('%.17g', tokens), 'updated_at', string.format('%.17g', now))
redis.call('PEXPIRE', KEYS[1], ttl)
return {allowed, retry}
`)

type redisLimiter struct {
	client   *redis.Client
	key      string
	rate     float64
	burst    int
	ttl      int64
	timeout  time.Duration
	closed   atomic.Bool
	once     sync.Once
	closeErr error
}

func newRedis(ctx context.Context, cfg config.RateLimit) (*redisLimiter, error) {
	rc := cfg.Redis
	if rc.Address == "" || rc.Key == "" || rc.DB < 0 || rc.DialTimeout <= 0 || rc.ReadTimeout <= 0 || rc.WriteTimeout <= 0 {
		return nil, errors.New("redis rate limit requires address, key, nonnegative DB and positive timeouts")
	}
	opts := &redis.Options{
		Addr:                  rc.Address,
		Username:              rc.Username,
		Password:              rc.Password,
		DB:                    rc.DB,
		DialTimeout:           rc.DialTimeout,
		ReadTimeout:           rc.ReadTimeout,
		WriteTimeout:          rc.WriteTimeout,
		ContextTimeoutEnabled: true,
		// Replaying a mutation after an ambiguous network error can consume twice.
		MaxRetries: -1,
	}
	if rc.TLS {
		opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	l := &redisLimiter{
		client:  redis.NewClient(opts),
		key:     rc.Key,
		rate:    cfg.RequestsPerSecond,
		burst:   cfg.Burst,
		ttl:     int64(math.Max(1, math.Ceil(float64(cfg.Burst)/cfg.RequestsPerSecond*1000))),
		timeout: max(rc.DialTimeout, rc.ReadTimeout, rc.WriteTimeout),
	}
	pingCtx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	if err := l.client.Ping(pingCtx).Err(); err != nil {
		connectErr := backendError(pingCtx, "connect")
		if closeErr := l.Close(); closeErr != nil {
			return nil, errors.Join(connectErr, closeErr)
		}
		return nil, connectErr
	}
	return l, nil
}

func (l *redisLimiter) Allow(ctx context.Context) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	if l.closed.Load() {
		return Decision{}, ErrClosed
	}
	commandCtx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	values, err := tokenScript.Run(commandCtx, l.client, []string{l.key}, l.rate, l.burst, l.ttl).Int64Slice()
	if err != nil {
		return Decision{}, backendError(commandCtx, "allow")
	}
	if len(values) != 2 || (values[0] != 0 && values[0] != 1) || values[1] < 0 || values[1] > math.MaxInt64/int64(time.Millisecond) {
		return Decision{}, ErrUnavailable
	}
	return Decision{Allowed: values[0] == 1, RetryAfter: time.Duration(values[1]) * time.Millisecond}, nil
}

func (l *redisLimiter) Close() error {
	l.once.Do(func() {
		l.closed.Store(true)
		if err := l.client.Close(); err != nil {
			l.closeErr = fmt.Errorf("close redis rate limiter: %w", ErrUnavailable)
		}
	})
	return l.closeErr
}

func backendError(ctx context.Context, operation string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("redis rate limiter %s: %w", operation, err)
	}
	// Backend error strings can contain credentials or remote-controlled text.
	return fmt.Errorf("redis rate limiter %s: %w", operation, ErrUnavailable)
}
