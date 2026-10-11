package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"token-router/src/internal/bucket"
	"token-router/src/internal/bucket/backend"
	"token-router/src/internal/config"
)

func TestUpstreamUsageLimitsSubsequentRequests(t *testing.T) {
	for _, backendName := range []string{"memory", "redis"} {
		t.Run(backendName, func(t *testing.T) {
			for _, test := range []struct {
				name, contentType, body string
				status                  int
				denied                  bool
			}{
				{name: "chat JSON", body: `{"choices":[],"usage":{"prompt_tokens":8,"completion_tokens":2,"total_tokens":10}}`, denied: true},
				{name: "responses JSON fallback", body: `{"output":[],"usage":{"input_tokens":8,"output_tokens":2}}`, denied: true},
				{name: "usage exceeds capacity", body: `{"usage":{"total_tokens":30}}`, denied: true},
				{name: "chat SSE", contentType: "text/event-stream", body: "data: {\"usage\":null}\n\ndata: {\"usage\":{\"total_tokens\":10}}\n\ndata: [DONE]\n\n", denied: true},
				{name: "responses SSE", contentType: "text/event-stream", body: "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":8,\"output_tokens\":2,\"total_tokens\":10}}}\n\n", denied: true},
				{name: "duplicate cumulative usage", contentType: "text/event-stream", body: "data: {\"usage\":{\"total_tokens\":6}}\n\ndata: {\"usage\":{\"total_tokens\":6}}\n\n"},
				{name: "missing usage", body: `{"choices":[]}`},
				{name: "invalid usage", body: `{"usage":{"total_tokens":-10}}`},
				{name: "upstream failure", body: `{"usage":{"total_tokens":10},"error":{"message":"failed"}}`, status: http.StatusBadRequest},
			} {
				t.Run(test.name, func(t *testing.T) {
					cfg := config.Defaults()
					cfg.RateLimit.Backend = backendName
					cfg.RateLimit.RequestsPerSecond = 0.001
					cfg.RateLimit.Burst = 20
					cfg.RateLimit.TokensPerSecond = 0.001
					cfg.RateLimit.TokenBurst = 10
					if backendName == "redis" {
						cfg.RateLimit.Redis.Address = miniredis.RunT(t).Addr()
					}
					limiter, err := backend.New(t.Context(), cfg.RateLimit)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if err := limiter.Close(); err != nil {
							t.Error(err)
						}
					})
					var forwarded atomic.Int64
					status := test.status
					if status == 0 {
						status = http.StatusOK
					}
					a := testApplication(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						forwarded.Add(1)
						contentType := test.contentType
						if contentType == "" {
							contentType = "application/json"
						}
						w.Header().Set("Content-Type", contentType)
						w.WriteHeader(status)
						if _, err := io.WriteString(w, test.body); err != nil {
							t.Error(err)
						}
					}), limiter, func(c *config.Config) { c.RateLimit = cfg.RateLimit })
					handler := a.handler()
					first := httptest.NewRecorder()
					handler.ServeHTTP(first, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", nil))
					if first.Code != status || first.Body.String() != test.body {
						t.Fatalf("first response changed: status=%d body=%q", first.Code, first.Body.String())
					}
					second := httptest.NewRecorder()
					handler.ServeHTTP(second, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", nil))
					if test.denied {
						if second.Code != http.StatusTooManyRequests || forwarded.Load() != 1 {
							t.Fatalf("token quota not enforced: status=%d forwarded=%d", second.Code, forwarded.Load())
						}
						if retry, err := strconv.Atoi(second.Header().Get("Retry-After")); err != nil || retry <= 0 {
							t.Fatalf("invalid Retry-After: %q", second.Header().Get("Retry-After"))
						}
					} else if second.Code != status || forwarded.Load() != 2 {
						t.Fatalf("unexpected rejection: status=%d forwarded=%d", second.Code, forwarded.Load())
					}
				})
			}
		})
	}
}

type usageRecorder struct {
	fixedLimiter
	record func(context.Context, int64) error
}

func (l *usageRecorder) RecordUsage(ctx context.Context, tokens int64) error {
	return l.record(ctx, tokens)
}

func TestUsageObservationDisabled(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			limiter := &usageRecorder{
				fixedLimiter: fixedLimiter{decision: bucket.Decision{Allowed: true}},
				record: func(context.Context, int64) error {
					t.Error("disabled token quota recorded usage")
					return nil
				},
			}
			a := testApplication(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if _, err := io.WriteString(w, `{"usage":{"total_tokens":10}}`); err != nil {
					t.Error(err)
				}
			}), limiter, func(cfg *config.Config) {
				cfg.RateLimit.Enabled = enabled
				if !enabled {
					cfg.RateLimit.TokensPerSecond = 1
					cfg.RateLimit.TokenBurst = 10
				}
			})
			response := httptest.NewRecorder()
			a.handler().ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", nil))
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d", response.Code)
			}
		})
	}
}

func TestRecordUsageSurvivesCancellationAndSanitizesFailure(t *testing.T) {
	var logs bytes.Buffer
	var recorded int64
	a := &application{
		logger: slog.New(slog.NewTextHandler(&logs, nil)),
		limiter: &usageRecorder{record: func(ctx context.Context, tokens int64) error {
			if err := ctx.Err(); err != nil {
				t.Errorf("recording received canceled context: %v", err)
			}
			recorded += tokens
			return errors.New("sensitive-backend-secret")
		}},
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	a.recordUsage(ctx, 12)
	if recorded != 12 || !strings.Contains(logs.String(), "token usage could not be recorded") {
		t.Fatalf("usage=%d log=%q", recorded, logs.String())
	}
	if strings.Contains(logs.String(), "sensitive-backend-secret") {
		t.Fatal("usage failure leaked backend details")
	}
}
