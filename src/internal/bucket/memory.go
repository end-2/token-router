package bucket

import (
	"context"
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"token-router/src/internal/config"
)

type memoryLimiter struct {
	mu           sync.Mutex
	limiter      *rate.Limiter
	usageRate    float64
	usageBurst   float64
	usageTokens  float64
	usageUpdated time.Time
	now          func() time.Time
	closed       bool
}

func newMemory(requestsPerSecond float64, burst int, tokensPerSecond float64, tokenBurst int) *memoryLimiter {
	return &memoryLimiter{
		limiter:     rate.NewLimiter(rate.Limit(requestsPerSecond), burst),
		usageRate:   tokensPerSecond,
		usageBurst:  float64(tokenBurst),
		usageTokens: float64(tokenBurst),
		now:         time.Now,
	}
}

func (l *memoryLimiter) Allow(ctx context.Context) (Decision, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	if l.closed {
		return Decision{}, ErrClosed
	}
	now := l.now()
	reservation := l.limiter.ReserveN(now, 1)
	if !reservation.OK() {
		return Decision{}, ErrUnavailable
	}
	wait := reservation.DelayFrom(now)
	if l.usageRate > 0 {
		l.refillUsage(now)
		if l.usageTokens < 1 {
			wait = max(wait, usageRetry((1-l.usageTokens)/l.usageRate))
		}
	}
	if wait > 0 {
		// Reservation and cancellation must be atomic relative to other callers,
		// so denied requests never reserve capacity needed by a later request.
		reservation.CancelAt(now)
		return Decision{RetryAfter: wait}, nil
	}
	return Decision{Allowed: true}, nil
}

func (l *memoryLimiter) RecordUsage(ctx context.Context, tokens int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if l.closed {
		return ErrClosed
	}
	if tokens < 0 {
		return ErrInvalidUsage
	}
	if l.usageRate == 0 || tokens == 0 {
		return nil
	}
	l.refillUsage(l.now())
	// Usage arrives after admission and may exceed the configured burst. Keeping
	// a negative balance accounts for all completed concurrent requests.
	l.usageTokens -= float64(tokens)
	return nil
}

func (l *memoryLimiter) refillUsage(now time.Time) {
	if l.usageUpdated.IsZero() {
		l.usageUpdated = now
	}
	if now.After(l.usageUpdated) {
		l.usageTokens = math.Min(l.usageBurst, l.usageTokens+now.Sub(l.usageUpdated).Seconds()*l.usageRate)
		l.usageUpdated = now
	}
}

func usageRetry(seconds float64) time.Duration {
	if seconds >= float64(math.MaxInt64)/float64(time.Second) {
		return time.Duration(math.MaxInt64)
	}
	return max(time.Nanosecond, time.Duration(math.Ceil(seconds*float64(time.Second))))
}

func (l *memoryLimiter) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	return nil
}

func (l *memoryLimiter) Update(values config.LimitValues) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.limiter.SetLimitAt(now, rate.Limit(values.RequestsPerSecond))
	l.limiter.SetBurstAt(now, values.Burst)
	if l.usageRate > 0 {
		l.refillUsage(now)
		l.usageTokens = math.Min(l.usageTokens, float64(values.TokenBurst))
	} else {
		l.usageTokens = float64(values.TokenBurst)
	}
	l.usageRate = values.TokensPerSecond
	l.usageBurst = float64(values.TokenBurst)
	l.usageUpdated = now
}
