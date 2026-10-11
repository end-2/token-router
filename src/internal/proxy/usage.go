package proxy

import (
	"context"

	"token-router/src/internal/proxy/usage"
)

// WithUsageRecorder observes OpenAI usage in successful generation POST responses.
// The callback receives positive increases in reported total tokens and the upstream
// request context. Request bodies and response bytes pass through unchanged.
func WithUsageRecorder(record func(context.Context, int64)) Option {
	return usage.Recorder(record)
}
