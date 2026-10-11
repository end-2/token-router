package bucket

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"token-router/src/internal/config"
)

type memoryLimiter struct {
	shards    [16]memoryShard
	nextSweep atomic.Pointer[time.Time]
	now       func() time.Time
}

type memoryShard struct {
	mu     sync.Mutex
	states map[string]*memoryState
	limits config.LimitValues
	closed bool
}

type memoryState struct {
	limiter      *rate.Limiter
	usageRate    float64
	usageBurst   float64
	usageTokens  float64
	usageUpdated time.Time
}

func newMemory(requestsPerSecond float64, burst int, tokensPerSecond float64, tokenBurst int) *memoryLimiter {
	l := &memoryLimiter{now: time.Now}
	for i := range l.shards {
		l.shards[i].limits = config.LimitValues{RequestsPerSecond: requestsPerSecond, Burst: burst,
			TokensPerSecond: tokensPerSecond, TokenBurst: tokenBurst}
	}
	return l
}

func (l *memoryLimiter) shard(key string) *memoryShard {
	if key == "" {
		return &l.shards[0]
	}
	// Account keys are lowercase SHA-256 hex digests, so their first nibble
	// distributes independent accounts without another hash on the request path.
	index := key[0] - '0'
	if key[0] >= 'a' {
		index = key[0] - 'a' + 10
	}
	return &l.shards[index]
}

func (l *memoryLimiter) scheduleSweep(now time.Time) bool {
	next := l.nextSweep.Load()
	if next != nil && now.Before(*next) {
		return false
	}
	updated := now.Add(time.Minute)
	return l.nextSweep.CompareAndSwap(next, &updated)
}

func (l *memoryLimiter) sweep(now time.Time) {
	for i := range l.shards {
		shard := &l.shards[i]
		shard.mu.Lock()
		for key, state := range shard.states {
			state.refillUsage(now)
			if state.limiter.TokensAt(now) >= float64(state.limiter.Burst()) &&
				(state.usageRate == 0 || state.usageTokens >= state.usageBurst) {
				delete(shard.states, key)
			}
		}
		shard.mu.Unlock()
	}
}

func (s *memoryShard) state(key string, now time.Time) *memoryState {
	state := s.states[key]
	if state == nil {
		state = &memoryState{
			limiter:   rate.NewLimiter(rate.Limit(s.limits.RequestsPerSecond), s.limits.Burst),
			usageRate: s.limits.TokensPerSecond, usageBurst: float64(s.limits.TokenBurst),
			usageTokens: float64(s.limits.TokenBurst), usageUpdated: now,
		}
		if s.states == nil {
			s.states = make(map[string]*memoryState)
		}
		s.states[key] = state
	}
	return state
}

func (l *memoryLimiter) Allow(ctx context.Context) (Decision, error) {
	key := accountKey(ctx)
	shard := l.shard(key)
	shard.mu.Lock()
	var sweep bool
	var now time.Time
	defer func() {
		shard.mu.Unlock()
		if sweep {
			l.sweep(now)
		}
	}()
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	if shard.closed {
		return Decision{}, ErrClosed
	}

	now = l.now()
	sweep = l.scheduleSweep(now)
	s := shard.state(key, now)
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
	key := accountKey(ctx)
	shard := l.shard(key)
	shard.mu.Lock()
	var sweep bool
	var now time.Time
	defer func() {
		shard.mu.Unlock()
		if sweep {
			l.sweep(now)
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if shard.closed {
		return ErrClosed
	}
	if tokens < 0 {
		return ErrInvalidUsage
	}
	if shard.limits.TokensPerSecond == 0 || tokens == 0 {
		return nil
	}

	now = l.now()
	sweep = l.scheduleSweep(now)
	s := shard.state(key, now)
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
	l.lock()
	defer l.unlock()
	for i := range l.shards {
		l.shards[i].closed = true
		clear(l.shards[i].states)
	}
	return nil
}

func (l *memoryLimiter) Update(values config.LimitValues) {
	l.lock()
	defer l.unlock()

	now := l.now()
	for i := range l.shards {
		shard := &l.shards[i]
		for _, s := range shard.states {
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
		shard.limits = values
	}
}

// Lock every shard in a fixed order so updates and close remain atomic across
// all accounts without a shared lock on individual admissions and usage.
func (l *memoryLimiter) lock() {
	for i := range l.shards {
		l.shards[i].mu.Lock()
	}
}

func (l *memoryLimiter) unlock() {
	for i := len(l.shards) - 1; i >= 0; i-- {
		l.shards[i].mu.Unlock()
	}
}
