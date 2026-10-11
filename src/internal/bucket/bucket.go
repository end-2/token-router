// Package bucket provides interchangeable request and upstream token limits.
package bucket

import (
	"context"
	"errors"
	"net/http"
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

// AdminBackend exposes internal cluster routes on the separate admin listener.
type AdminBackend interface {
	RegisterAdmin(*http.ServeMux)
}
