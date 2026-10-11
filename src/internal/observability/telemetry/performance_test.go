package telemetry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"token-router/src/internal/config"
)

func benchmarkTelemetry(b *testing.B, logs bool, level string, metrics bool) *Telemetry {
	b.Helper()
	cfg := config.Defaults().Telemetry
	cfg.Logs.Enabled, cfg.Logs.Level = logs, level
	telemetry, err := New(context.Background(), cfg, io.Discard)
	if err != nil {
		b.Fatal(err)
	}
	if metrics {
		telemetry.metrics = sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewManualReader()))
		if err := telemetry.createInstruments(telemetry.metrics.Meter(instrumentationName)); err != nil {
			b.Fatal(err)
		}
	}
	b.Cleanup(func() {
		if err := telemetry.Shutdown(context.Background()); err != nil {
			b.Error(err)
		}
	})
	return telemetry
}

func BenchmarkTelemetryMiddleware(b *testing.B) {
	for _, tc := range []struct {
		name    string
		logs    bool
		level   string
		metrics bool
	}{
		{name: "disabled", level: "info"},
		{name: "filtered_logs", logs: true, level: "warn"},
		{name: "local_logs", logs: true, level: "info"},
		{name: "metrics", level: "info", metrics: true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			telemetry := benchmarkTelemetry(b, tc.logs, tc.level, tc.metrics)
			e := echo.New()
			e.Use(telemetry.Middleware)
			e.GET("/v1/*", func(c *echo.Context) error { return c.NoContent(http.StatusOK) })
			request := httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/v1/models", nil)
			response := httptest.NewRecorder()
			b.ReportAllocs()
			for b.Loop() {
				e.ServeHTTP(response, request)
			}
		})
	}
}

func BenchmarkTelemetryRecordLimit(b *testing.B) {
	for _, metrics := range []bool{false, true} {
		name := "disabled"
		if metrics {
			name = "metrics"
		}
		b.Run(name, func(b *testing.B) {
			telemetry := benchmarkTelemetry(b, false, "info", metrics)
			ctx := context.Background()
			b.ReportAllocs()
			for b.Loop() {
				telemetry.RecordLimit(ctx, true, nil)
			}
		})
	}
}

func BenchmarkTracedTransport(b *testing.B) {
	for _, sampled := range []bool{false, true} {
		name, sampler := "unsampled", sdktrace.NeverSample()
		if sampled {
			name, sampler = "sampled", sdktrace.AlwaysSample()
		}
		b.Run(name, func(b *testing.B) {
			provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sampler))
			b.Cleanup(func() {
				if err := provider.Shutdown(context.Background()); err != nil {
					b.Error(err)
				}
			})
			transport := tracedTransport{
				tracer: provider.Tracer(instrumentationName), propagator: propagation.TraceContext{},
				base: roundTripperFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusOK, Body: benchmarkBody{}}, nil
				}),
			}
			request := httptest.NewRequestWithContext(b.Context(), http.MethodPost, "http://upstream/v1/chat/completions", nil)
			request.Header.Set("Authorization", "Bearer benchmark-token")
			b.ReportAllocs()
			for b.Loop() {
				response, err := transport.RoundTrip(request)
				if err != nil {
					b.Fatal(err)
				}
				if err := response.Body.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

type benchmarkBody struct{}

func (benchmarkBody) Read([]byte) (int, error) { return 0, io.EOF }
func (benchmarkBody) Close() error             { return nil }
