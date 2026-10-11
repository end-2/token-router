// Package bucket provides interchangeable request token buckets.
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

// Decision describes whether a request can proceed and when a denied request can retry.
type Decision struct {
	Allowed    bool
	RetryAfter time.Duration
}

// Limiter consumes one request token per successful Allow call. Implementations
// are safe for concurrent use and fail closed when their backend is unavailable.
type Limiter interface {
	Allow(context.Context) (Decision, error)
	Close() error
}

// New selects a process-local memory bucket, a shared Redis bucket, or a disabled
// limiter according to cfg. Redis connectivity is checked before returning.
func New(ctx context.Context, cfg config.RateLimit) (Limiter, error) {
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
	switch cfg.Backend {
	case "memory":
		return newMemory(cfg.RequestsPerSecond, cfg.Burst), nil
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

func (l *disabledLimiter) Allow(ctx context.Context) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	if l.closed.Load() {
		return Decision{}, ErrClosed
	}
	return Decision{Allowed: true}, nil
}

func (l *disabledLimiter) Close() error {
	l.closed.Store(true)
	return nil
}
