// Package backend constructs configured rate limiter implementations.
package backend

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"token-router/src/internal/bucket"
	"token-router/src/internal/bucket/distributed"
	"token-router/src/internal/bucket/memory"
	"token-router/src/internal/bucket/redis"
	"token-router/src/internal/config"
)

// New selects the configured backend or a disabled limiter. Redis connectivity
// is checked here; distributed election starts when the admin listener serves.
func New(ctx context.Context, cfg config.RateLimit) (bucket.Reconfigurable, error) {
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
		if cfg.Memory.Distributed.Enabled {
			return distributed.New(cfg)
		}
		return memory.New(cfg.RequestsPerSecond, cfg.Burst, cfg.TokensPerSecond, cfg.TokenBurst), nil
	case "redis":
		limiter, err := redis.New(ctx, cfg)
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

func (l *disabledLimiter) Allow(ctx context.Context) (bucket.Decision, error) {
	if err := ctx.Err(); err != nil {
		return bucket.Decision{}, err
	}
	if l.closed.Load() {
		return bucket.Decision{}, bucket.ErrClosed
	}
	return bucket.Decision{Allowed: true}, nil
}

func (l *disabledLimiter) RecordUsage(ctx context.Context, tokens int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if l.closed.Load() {
		return bucket.ErrClosed
	}
	if tokens < 0 {
		return bucket.ErrInvalidUsage
	}
	return nil
}

func (l *disabledLimiter) Close() error {
	l.closed.Store(true)
	return nil
}
