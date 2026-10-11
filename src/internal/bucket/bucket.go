// Package bucket provides interchangeable request and upstream token limits.
package bucket

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"token-router/src/internal/config"
)

// ErrClosed indicates that a limiter has released its resources.
var ErrClosed = errors.New("rate limiter is closed")

// ErrUnavailable indicates that a backend could not make a rate limit decision.
var ErrUnavailable = errors.New("rate limiter backend unavailable")

// ErrInvalidUsage indicates that a reported token count is negative.
var ErrInvalidUsage = errors.New("rate limiter token usage must not be negative")

// Decision describes whether a request can proceed and when a denied request can retry.
type Decision struct {
	Allowed    bool
	RetryAfter time.Duration
}

// Limiter consumes one request unit per successful Allow call and charges actual
// upstream token counts through RecordUsage. Implementations are safe for
// concurrent use and fail closed when their backend is unavailable.
type Limiter interface {
	Allow(context.Context) (Decision, error)
	RecordUsage(context.Context, int64) error
	Close() error
}

// Reconfigurable preserves backend resources and balances when validated limits change.
type Reconfigurable interface {
	Limiter
	Update(config.LimitValues)
}

// New selects a process-local memory bucket, a shared Redis bucket, or a disabled
// limiter according to cfg. Redis connectivity is checked before returning.
func New(ctx context.Context, cfg config.RateLimit) (Reconfigurable, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return &disabledLimiter{}, nil
	}
	if math.IsNaN(cfg.RequestsPerSecond) || math.IsInf(cfg.RequestsPerSecond, 0) || cfg.RequestsPerSecond <= 0 || cfg.Burst < 1 {
		return nil, errors.New("rate limit requires a finite positive rate and positive burst")
	}
	if float64(cfg.Burst)/cfg.RequestsPerSecond >= float64(math.MaxInt64)/float64(time.Second) {
		return nil, errors.New("rate limit refill duration exceeds supported range")
	}
	if cfg.TokensPerSecond != 0 || cfg.TokenBurst != 0 {
		if math.IsNaN(cfg.TokensPerSecond) || math.IsInf(cfg.TokensPerSecond, 0) || cfg.TokensPerSecond <= 0 || cfg.TokenBurst < 1 {
			return nil, errors.New("token rate limit requires a finite positive rate and positive burst")
		}
		if float64(cfg.TokenBurst)/cfg.TokensPerSecond >= float64(math.MaxInt64)/float64(time.Second) {
			return nil, errors.New("token rate limit refill duration exceeds supported range")
		}
	}
	switch cfg.Backend {
	case "memory":
		return newMemory(cfg.RequestsPerSecond, cfg.Burst, cfg.TokensPerSecond, cfg.TokenBurst), nil
	case "redis":
		limiter, err := newRedis(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return limiter, nil
	default:
		return nil, fmt.Errorf("unsupported rate limit backend %q", cfg.Backend)
	}
}

type disabledLimiter struct {
	closed atomic.Bool
}

func (*disabledLimiter) Update(config.LimitValues) {}

func (l *disabledLimiter) Allow(ctx context.Context) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	if l.closed.Load() {
		return Decision{}, ErrClosed
	}
	return Decision{Allowed: true}, nil
}

func (l *disabledLimiter) RecordUsage(ctx context.Context, tokens int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if l.closed.Load() {
		return ErrClosed
	}
	if tokens < 0 {
		return ErrInvalidUsage
	}
	return nil
}

func (l *disabledLimiter) Close() error {
	l.closed.Store(true)
	return nil
}
