// Package redis implements shared atomic request and token limits in Redis.
package redis

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

	"token-router/src/internal/bucket"
	"token-router/src/internal/config"
)

// Redis TIME keeps replicas on one clock. Both limits share one atomic mutation.
// A key expires only after all balances, including token debt, can refill fully.
var tokenScript = redis.NewScript(`
local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + tonumber(clock[2]) / 1000
local rate = tonumber(ARGV[1])
local capacity = tonumber(ARGV[2])
local ttl = tonumber(ARGV[3])
local usage_rate = tonumber(ARGV[4])
local usage_capacity = tonumber(ARGV[5])
local operation = ARGV[6]
local consumed = tonumber(ARGV[7])
local max_interval = tonumber(ARGV[8])
local state = redis.call('HMGET', KEYS[1], 'tokens', 'updated_at', 'usage_tokens', 'usage_updated_at')
local tokens = tonumber(state[1]) or capacity
local updated = tonumber(state[2]) or now
now = math.max(now, updated)
local usage_tokens = tonumber(state[3]) or usage_capacity
local usage_updated = tonumber(state[4]) or now
if usage_rate > 0 then
    now = math.max(now, usage_updated)
    usage_tokens = math.min(usage_capacity, usage_tokens + (now - usage_updated) * usage_rate / 1000)
end
tokens = math.min(capacity, tokens + (now - updated) * rate / 1000)
local allowed = 0
local retry = 0
if operation == 'allow' then
    if tokens >= 1 and (usage_rate == 0 or usage_tokens >= 1) then
        tokens = tokens - 1
        allowed = 1
    else
        if tokens < 1 then
            retry = math.max(1, math.ceil((1 - tokens) / rate * 1000))
        end
        if usage_rate > 0 and usage_tokens < 1 then
            retry = math.max(retry, math.ceil((1 - usage_tokens) / usage_rate * 1000))
        end
        retry = math.min(max_interval, retry)
    end
elseif usage_rate > 0 then
    usage_tokens = usage_tokens - consumed
end
redis.call('HSET', KEYS[1], 'tokens', string.format('%.17g', tokens), 'updated_at', string.format('%.17g', now))
if usage_rate > 0 then
    redis.call('HSET', KEYS[1], 'usage_tokens', string.format('%.17g', usage_tokens), 'usage_updated_at', string.format('%.17g', now))
    ttl = math.max(ttl, math.ceil((usage_capacity - usage_tokens) / usage_rate * 1000))
end
if ttl <= max_interval then
    redis.call('PEXPIRE', KEYS[1], ttl)
else
    redis.call('PERSIST', KEYS[1])
end
return {allowed, retry}
`)

// Limiter maintains shared atomic request and token limits in Redis.
type Limiter struct {
	mu         sync.RWMutex
	client     *redis.Client
	key        string
	rate       float64
	burst      int
	ttl        int64
	usageRate  float64
	usageBurst int
	timeout    time.Duration
	closed     atomic.Bool
	once       sync.Once
	closeErr   error
}

// New constructs a limiter from validated rate limit values.
func New(ctx context.Context, cfg config.RateLimit) (*Limiter, error) {
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

	l := &Limiter{
		client:     redis.NewClient(opts),
		key:        rc.Key,
		rate:       cfg.RequestsPerSecond,
		burst:      cfg.Burst,
		ttl:        int64(math.Max(1, math.Ceil(float64(cfg.Burst)/cfg.RequestsPerSecond*1000))),
		usageRate:  cfg.TokensPerSecond,
		usageBurst: cfg.TokenBurst,
		timeout:    max(rc.DialTimeout, rc.ReadTimeout, rc.WriteTimeout),
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

// Allow atomically checks both balances and consumes one request unit on admission.
func (l *Limiter) Allow(ctx context.Context) (bucket.Decision, error) {
	if err := ctx.Err(); err != nil {
		return bucket.Decision{}, err
	}
	if l.closed.Load() {
		return bucket.Decision{}, bucket.ErrClosed
	}

	commandCtx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	values, err := l.run(commandCtx, "allow", 0)
	if err != nil {
		return bucket.Decision{}, backendError(commandCtx, "allow")
	}

	if len(values) != 2 || (values[0] != 0 && values[0] != 1) || values[1] < 0 || values[1] > math.MaxInt64/int64(time.Millisecond) {
		return bucket.Decision{}, bucket.ErrUnavailable
	}
	return bucket.Decision{Allowed: values[0] == 1, RetryAfter: time.Duration(values[1]) * time.Millisecond}, nil
}

// RecordUsage atomically charges observed tokens to the shared account balance.
func (l *Limiter) RecordUsage(ctx context.Context, tokens int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if l.closed.Load() {
		return bucket.ErrClosed
	}
	if tokens < 0 {
		return bucket.ErrInvalidUsage
	}

	l.mu.RLock()
	usageRate := l.usageRate
	l.mu.RUnlock()
	if usageRate == 0 || tokens == 0 {
		return nil
	}

	commandCtx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	if _, err := l.run(commandCtx, "usage", tokens); err != nil {
		return backendError(commandCtx, "record usage")
	}
	return nil
}

func (l *Limiter) run(ctx context.Context, operation string, tokens int64) ([]int64, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	key := l.key
	if account := bucket.AccountKey(ctx); account != "" {
		key += ":account:" + account
	}

	return tokenScript.Run(ctx, l.client, []string{key}, l.rate, l.burst, l.ttl,
		l.usageRate, l.usageBurst, operation, tokens, math.MaxInt64/int64(time.Millisecond)).Int64Slice()
}

// Update changes validated limits for subsequent Redis operations.
func (l *Limiter) Update(values config.LimitValues) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rate, l.burst = values.RequestsPerSecond, values.Burst
	l.usageRate, l.usageBurst = values.TokensPerSecond, values.TokenBurst
	l.ttl = int64(math.Max(1, math.Ceil(float64(values.Burst)/values.RequestsPerSecond*1000)))
}

// Close rejects future operations and releases the Redis client once.
func (l *Limiter) Close() error {
	l.once.Do(func() {
		l.closed.Store(true)
		if err := l.client.Close(); err != nil {
			l.closeErr = fmt.Errorf("close redis rate limiter: %w", bucket.ErrUnavailable)
		}
	})
	return l.closeErr
}

func backendError(ctx context.Context, operation string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("redis rate limiter %s: %w", operation, err)
	}
	// Backend error strings can contain credentials or remote-controlled text.
	return fmt.Errorf("redis rate limiter %s: %w", operation, bucket.ErrUnavailable)
}
