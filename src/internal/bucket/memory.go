package bucket

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type memoryLimiter struct {
	mu      sync.Mutex
	limiter *rate.Limiter
	now     func() time.Time
	closed  bool
}

func newMemory(requestsPerSecond float64, burst int) *memoryLimiter {
	return &memoryLimiter{
		limiter: rate.NewLimiter(rate.Limit(requestsPerSecond), burst),
		now:     time.Now,
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
	if wait > 0 {
		// Reservation and cancellation must be atomic relative to other callers,
		// so denied requests never reserve capacity needed by a later request.
		reservation.CancelAt(now)
		return Decision{RetryAfter: wait}, nil
	}
	return Decision{Allowed: true}, nil
}

func (l *memoryLimiter) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	return nil
}
