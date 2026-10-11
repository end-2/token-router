package observability

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"token-router/src/internal/config"
)

const instrumentationName = "token-router"

var limitOutcomes = [...]string{"allowed", "denied", "error"}

var limitOptions = func() [3][]metric.AddOption {
	var options [3][]metric.AddOption
	for i, outcome := range limitOutcomes {
		options[i] = []metric.AddOption{metric.WithAttributes(attribute.String("result", outcome))}
	}
	return options
}()

// Telemetry owns isolated OpenTelemetry providers and the application logger.
type Telemetry struct {
	logger        *slog.Logger
	tracer        trace.Tracer
	propagator    propagation.TextMapPropagator
	tracing       bool
	traces        *sdktrace.TracerProvider
	metrics       *sdkmetric.MeterProvider
	logs          *sdklog.LoggerProvider
	requests      metric.Int64Counter
	duration      metric.Float64Histogram
	inFlight      metric.Int64UpDownCounter
	limits        metric.Int64Counter
	exports       []*exportStatus
	exportContext context.Context
	cancelExports context.CancelFunc
	shutdownOnce  sync.Once
	shutdownError error
}

// NewTelemetry constructs providers without changing global OpenTelemetry state.
// OTLP export and local application logging follow their separate enable settings.
func NewTelemetry(ctx context.Context, cfg config.Telemetry, output io.Writer) (_ *Telemetry, err error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.Logs.Level)); err != nil {
		return nil, errors.New("invalid log level")
	}
	opts := &slog.HandlerOptions{Level: level}
	var local slog.Handler = slog.NewJSONHandler(output, opts)
	if cfg.Logs.Format == "text" {
		local = slog.NewTextHandler(output, opts)
	}
	if !cfg.Logs.Enabled {
		local = slog.DiscardHandler
	}
	t := &Telemetry{
		logger: slog.New(local), tracer: tracenoop.NewTracerProvider().Tracer(instrumentationName),
		propagator: propagation.TraceContext{}, tracing: cfg.OTLP.Enabled && cfg.Traces.Enabled,
	}
	t.exportContext, t.cancelExports = context.WithCancel(context.WithoutCancel(ctx))
	defer func() {
		if err != nil {
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
			defer cancel()
			err = errors.Join(err, t.Shutdown(shutdownCtx))
		}
	}()
	instanceID := cfg.ServiceInstanceID
	if instanceID == "" {
		instanceID = rand.Text()
	}
	res := resource.NewSchemaless(
		attribute.String("service.name", cfg.ServiceName),
		attribute.String("service.instance.id", instanceID),
		attribute.String("service.version", cfg.ServiceVersion),
		attribute.String("deployment.environment.name", cfg.Environment),
	)
	endpoint := strings.TrimRight(cfg.OTLP.Endpoint, "/")
	if t.tracing {
		exporter, exportErr := otlptracehttp.New(ctx,
			otlptracehttp.WithEndpointURL(endpoint+"/v1/traces"), otlptracehttp.WithTimeout(cfg.OTLP.Timeout),
		)
		if exportErr != nil {
			return nil, errors.New("initialize OTLP trace exporter")
		}
		status := t.exportStatus("traces")
		t.traces = sdktrace.NewTracerProvider(
			sdktrace.WithResource(res),
			sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.Traces.SampleRatio))),
			sdktrace.WithBatcher(traceExporter{SpanExporter: exporter, status: status},
				sdktrace.WithBatchTimeout(time.Second), sdktrace.WithExportTimeout(cfg.OTLP.Timeout)),
		)
		t.tracer = t.traces.Tracer(instrumentationName)
	}
	meter := metricnoop.NewMeterProvider().Meter(instrumentationName)
	if cfg.OTLP.Enabled && cfg.Metrics.Enabled {
		exporter, exportErr := otlpmetrichttp.New(ctx,
			otlpmetrichttp.WithEndpointURL(endpoint+"/v1/metrics"), otlpmetrichttp.WithTimeout(cfg.OTLP.Timeout),
		)
		if exportErr != nil {
			return nil, errors.New("initialize OTLP metric exporter")
		}
		status := t.exportStatus("metrics")
		reader := sdkmetric.NewPeriodicReader(metricExporter{Exporter: exporter, status: status},
			sdkmetric.WithInterval(cfg.Metrics.ExportInterval), sdkmetric.WithTimeout(cfg.OTLP.Timeout))
		t.metrics = sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(reader))
		meter = t.metrics.Meter(instrumentationName)
	}
	if err := t.createInstruments(meter); err != nil {
		return nil, fmt.Errorf("initialize telemetry instruments: %w", err)
	}
	handler := local
	if cfg.OTLP.Enabled && cfg.Logs.Enabled {
		exporter, exportErr := otlploghttp.New(ctx,
			otlploghttp.WithEndpointURL(endpoint+"/v1/logs"), otlploghttp.WithTimeout(cfg.OTLP.Timeout),
		)
		if exportErr != nil {
			return nil, errors.New("initialize OTLP log exporter")
		}
		status := t.exportStatus("logs")
		processor := sdklog.NewBatchProcessor(logExporter{Exporter: exporter, status: status},
			sdklog.WithExportInterval(time.Second), sdklog.WithExportTimeout(cfg.OTLP.Timeout))
		t.logs = sdklog.NewLoggerProvider(sdklog.WithResource(res), sdklog.WithProcessor(processor))
		handler = slog.NewMultiHandler(local, otelslog.NewHandler(instrumentationName, otelslog.WithLoggerProvider(t.logs)))
	}
	t.logger = slog.New(contextHandler{Handler: handler, level: level})
	return t, nil
}

func (t *Telemetry) createInstruments(meter metric.Meter) error {
	var requestErr, durationErr, inFlightErr, limitErr error
	t.requests, requestErr = meter.Int64Counter("token_router.http.requests", metric.WithDescription("Completed HTTP requests"))
	t.duration, durationErr = meter.Float64Histogram("token_router.http.request.duration",
		metric.WithDescription("HTTP request duration including streamed responses"), metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60))
	t.inFlight, inFlightErr = meter.Int64UpDownCounter("token_router.http.requests.in_flight", metric.WithDescription("Active HTTP requests"))
	t.limits, limitErr = meter.Int64Counter("token_router.rate_limit.decisions", metric.WithDescription("Rate limiter outcomes"))
	return errors.Join(requestErr, durationErr, inFlightErr, limitErr)
}

// Logger returns the configured local logger and optional OTLP log bridge.
func (t *Telemetry) Logger() *slog.Logger { return t.logger }

// Shutdown flushes traces and metrics concurrently, then logs, using one deadline.
// Repeated calls return the result of the first shutdown.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	t.shutdownOnce.Do(func() {
		defer t.cancelExports()
		var tracesErr, metricsErr, logsErr error
		var wg sync.WaitGroup
		if t.traces != nil {
			wg.Go(func() { tracesErr = t.traces.Shutdown(ctx) })
		}
		if t.metrics != nil {
			wg.Go(func() { metricsErr = t.metrics.Shutdown(ctx) })
		}
		wg.Wait()
		if t.logs != nil {
			logsErr = t.logs.Shutdown(ctx)
		}
		if tracesErr != nil || metricsErr != nil || logsErr != nil {
			t.shutdownError = errors.New("telemetry shutdown failed")
		}
		for _, status := range t.exports {
			if status.failed.Load() {
				t.shutdownError = errors.Join(t.shutdownError, fmt.Errorf("OTLP %s export failed", status.signal))
			}
		}
	})
	return t.shutdownError
}

// RecordLimit records a fixed outcome without including storage errors or keys.
func (t *Telemetry) RecordLimit(ctx context.Context, allowed bool, err error) {
	if t.metrics == nil {
		return
	}
	t.limits.Add(ctx, 1, limitOptions[limitOutcome(allowed, err)]...)
}

func limitOutcome(allowed bool, err error) int {
	if err != nil {
		return 2
	}
	if !allowed {
		return 1
	}
	return 0
}
