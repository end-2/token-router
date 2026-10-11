package backend

import (
	"context"
	"errors"
	"math"
	"testing"

	"token-router/src/internal/bucket"
	"token-router/src/internal/bucket/limittest"
)

func TestTokenUsageValidationCancellationAndClose(t *testing.T) {
	for _, backend := range []string{"disabled"} {
		t.Run(backend, func(t *testing.T) {
			l := &disabledLimiter{}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := l.RecordUsage(ctx, 1); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled usage: %v", err)
			}
			if err := l.RecordUsage(context.Background(), -1); !errors.Is(err, bucket.ErrInvalidUsage) {
				t.Fatalf("negative usage: %v", err)
			}
			limittest.RequireUsage(t, l, 0)
			limittest.RequireDecision(t, l, true, 0)
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}
			if err := l.RecordUsage(context.Background(), 1); !errors.Is(err, bucket.ErrClosed) {
				t.Fatalf("closed usage: %v", err)
			}
		})
	}
}

func TestDisabledTokenUsage(t *testing.T) {
	l := &disabledLimiter{}
	limittest.RequireUsage(t, l, math.MaxInt64)
	limittest.RequireDecision(t, l, true, 0)
}
