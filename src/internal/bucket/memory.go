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
	mu        sync.Mutex
	states    map[string]*memoryState
	limits    config.LimitValues
	nextSweep time.Time
	now       func() time.Time
	closed    bool
}

type memoryState struct {
	limiter      *rate.Limiter
	usageRate    float64
	usageBurst   float64
	usageTokens  float64
	usageUpdated time.Time
}

func newMemory(requestsPerSecond float64, burst int, tokensPerSecond float64, tokenBurst int) *memoryLimiter {
	return &memoryLimiter{
		states: make(map[string]*memoryState), now: time.Now,
		limits: config.LimitValues{RequestsPerSecond: requestsPerSecond, Burst: burst,
			TokensPerSecond: tokensPerSecond, TokenBurst: tokenBurst},
	}
}

func (l *memoryLimiter) state(ctx context.Context, now time.Time) *memoryState {
	if !now.Before(l.nextSweep) {
		for key, state := range l.states {
			state.refillUsage(now)
			if state.limiter.TokensAt(now) >= float64(state.limiter.Burst()) &&
				(state.usageRate == 0 || state.usageTokens >= state.usageBurst) {
				delete(l.states, key)
			}
		}
		l.nextSweep = now.Add(time.Minute)
	}
	key := accountKey(ctx)
	state := l.states[key]
	if state == nil {
		state = &memoryState{
			limiter:   rate.NewLimiter(rate.Limit(l.limits.RequestsPerSecond), l.limits.Burst),
			usageRate: l.limits.TokensPerSecond, usageBurst: float64(l.limits.TokenBurst),
			usageTokens: float64(l.limits.TokenBurst), usageUpdated: now,
		}
		l.states[key] = state
	}
	return state
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
	s := l.state(ctx, now)
	reservation := s.limiter.ReserveN(now, 1)
	if !reservation.OK() {
		return Decision{}, ErrUnavailable
	}
	wait := reservation.DelayFrom(now)
	if s.usageRate > 0 {
		s.refillUsage(now)
		if s.usageTokens < 1 {
			wait = max(wait, usageRetry((1-s.usageTokens)/s.usageRate))
		}
	}
	if wait > 0 {
		// Reservation and cancellation are atomic relative to other callers, so
		// a denied request never reserves capacity needed by a later request.
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
	if l.limits.TokensPerSecond == 0 || tokens == 0 {
		return nil
	}
	now := l.now()
	s := l.state(ctx, now)
	s.refillUsage(now)
	// Negative balances account for usage from already admitted requests.
	s.usageTokens -= float64(tokens)
	return nil
}

func (s *memoryState) refillUsage(now time.Time) {
	if now.After(s.usageUpdated) {
		s.usageTokens = math.Min(s.usageBurst, s.usageTokens+now.Sub(s.usageUpdated).Seconds()*s.usageRate)
		s.usageUpdated = now
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
	clear(l.states)
	return nil
}

func (l *memoryLimiter) Update(values config.LimitValues) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for _, s := range l.states {
		s.limiter.SetLimitAt(now, rate.Limit(values.RequestsPerSecond))
		s.limiter.SetBurstAt(now, values.Burst)
		if s.usageRate > 0 {
			s.refillUsage(now)
			s.usageTokens = math.Min(s.usageTokens, float64(values.TokenBurst))
		} else {
			s.usageTokens = float64(values.TokenBurst)
		}
		s.usageRate = values.TokensPerSecond
		s.usageBurst = float64(values.TokenBurst)
		s.usageUpdated = now
	}
	l.limits = values
}
