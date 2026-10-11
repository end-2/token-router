package telemetry

import (
	"context"
	"log/slog"
	"sync/atomic"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type exportStatus struct {
	signal string
	logger *slog.Logger
	ctx    context.Context
	failed atomic.Bool
}

func (t *Telemetry) exportStatus(signal string) *exportStatus {
	status := &exportStatus{signal: signal, logger: t.logger, ctx: t.exportContext}
	t.exports = append(t.exports, status)
	return status
}

func (s *exportStatus) record(err error) {
	s.failed.Store(err != nil)
	if err != nil {
		// Exporters already retry. Keep final failures local to avoid recursive log export
		// and the SDK's process-wide error handler exposing endpoints or response bodies.
		s.logger.Warn("OTLP export failed; batch was not delivered", "signal", s.signal)
	}
}

func (s *exportStatus) context(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

type traceExporter struct {
	sdktrace.SpanExporter
	status *exportStatus
}

func (e traceExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	ctx, cancel := e.status.context(ctx)
	defer cancel()
	e.status.record(e.SpanExporter.ExportSpans(ctx, spans))
	return nil
}

type metricExporter struct {
	sdkmetric.Exporter
	status *exportStatus
}

func (e metricExporter) Export(ctx context.Context, data *metricdata.ResourceMetrics) error {
	ctx, cancel := e.status.context(ctx)
	defer cancel()
	e.status.record(e.Exporter.Export(ctx, data))
	return nil
}

type logExporter struct {
	sdklog.Exporter
	status *exportStatus
}

func (e logExporter) Export(ctx context.Context, records []sdklog.Record) error {
	ctx, cancel := e.status.context(ctx)
	defer cancel()
	e.status.record(e.Exporter.Export(ctx, records))
	return nil
}
