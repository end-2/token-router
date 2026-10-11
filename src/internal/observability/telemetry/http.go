package telemetry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/labstack/echo/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"token-router/src/internal/observability"
)

// Middleware records server spans, metrics and sanitized access logs without buffering.
func (t *Telemetry) Middleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		ctx := c.Request().Context()
		logRequest := t.logger.Enabled(ctx, slog.LevelInfo)
		if !t.tracing && t.metrics == nil && !logRequest {
			if err := next(c); err != nil {
				c.Echo().HTTPErrorHandler(c, err)
			}
			return nil
		}
		start := time.Now()
		method := observability.BoundedMethod(c.Request().Method)
		route := c.Path()
		if route == "" {
			route = "unmatched"
		}
		var attrs []attribute.KeyValue
		if t.tracing || t.metrics != nil {
			attrs = make([]attribute.KeyValue, 3)
			attrs[0] = attribute.String("http.request.method", method)
			attrs[1] = attribute.String("http.route", route)
		}
		var span trace.Span
		if t.tracing {
			ctx = t.propagator.Extract(ctx, propagation.HeaderCarrier(c.Request().Header))
			ctx, span = t.tracer.Start(ctx, method+" "+route, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attrs[:2]...))
			c.SetRequest(c.Request().WithContext(ctx))
		}
		if t.metrics != nil {
			t.inFlight.Add(ctx, 1)
		}
		defer func() {
			status := http.StatusOK
			if response, err := echo.UnwrapResponse(c.Response()); err == nil && response.Status > 0 {
				status = response.Status
			}
			elapsed := time.Since(start).Seconds()
			if t.metrics != nil {
				t.inFlight.Add(ctx, -1)
				t.duration.Record(ctx, elapsed, metric.WithAttributeSet(attribute.NewSet(attrs[:2]...)))
				attrs[2] = attribute.Int("http.response.status_code", status)
				t.requests.Add(ctx, 1, metric.WithAttributeSet(attribute.NewSet(attrs...)))
			}
			if span != nil && span.IsRecording() {
				span.SetAttributes(attribute.Int("http.response.status_code", status))
				if status >= 500 {
					span.SetStatus(codes.Error, "server_error")
				}
				if ctx.Err() != nil {
					markSpanError(span, ctx.Err())
				}
			}
			if logRequest {
				t.logger.LogAttrs(ctx, slog.LevelInfo, "HTTP request completed",
					slog.String("method", method), slog.String("route", route), slog.Int("status", status), slog.Float64("duration_seconds", elapsed))
			}
			if span != nil {
				span.End()
			}
		}()
		if err := next(c); err != nil {
			c.Echo().HTTPErrorHandler(c, err)
		}
		return nil
	}
}

// Transport creates upstream client spans that remain active until EOF or Close.
// Only fixed method and status attributes are recorded, never URLs or headers.
func (t *Telemetry) Transport(base http.RoundTripper) http.RoundTripper {
	if !t.tracing {
		return base
	}
	return tracedTransport{base: base, tracer: t.tracer, propagator: t.propagator}
}

type tracedTransport struct {
	base       http.RoundTripper
	tracer     trace.Tracer
	propagator propagation.TextMapPropagator
}

func (t tracedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	method := observability.BoundedMethod(request.Method)
	ctx, span := t.tracer.Start(request.Context(), method+" upstream", trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("http.request.method", method)))
	// Propagation changes only headers; the base transport must not mutate other request fields.
	request = request.WithContext(ctx)
	request.Header = request.Header.Clone()
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	request.Header.Del("traceparent")
	request.Header.Del("tracestate")
	t.propagator.Inject(ctx, propagation.HeaderCarrier(request.Header))
	response, err := t.base.RoundTrip(request)
	if !span.IsRecording() {
		span.End()
		return response, err
	}
	if err != nil {
		markSpanError(span, err)
		span.End()
		return response, err
	}
	span.SetAttributes(attribute.Int("http.response.status_code", response.StatusCode))
	if response.StatusCode >= 400 {
		span.SetStatus(codes.Error, "upstream_http_error")
	}
	if response.Body == nil || response.Body == http.NoBody {
		span.End()
	} else {
		response.Body = &spanBody{ReadCloser: response.Body, span: span, ctx: ctx}
	}
	return response, nil
}

type spanBody struct {
	io.ReadCloser
	span trace.Span
	ctx  context.Context
	once sync.Once
}

func (b *spanBody) Read(data []byte) (int, error) {
	n, err := b.ReadCloser.Read(data)
	if err != nil {
		b.finish(err)
	}
	return n, err
}

func (b *spanBody) Close() error {
	err := b.ReadCloser.Close()
	b.finish(err)
	return err
}

func (b *spanBody) finish(err error) {
	b.once.Do(func() {
		if (err == nil || errors.Is(err, io.EOF)) && b.ctx.Err() != nil {
			err = b.ctx.Err()
		}
		if err != nil && !errors.Is(err, io.EOF) {
			markSpanError(b.span, err)
		}
		b.span.End()
	})
}

func markSpanError(span trace.Span, err error) {
	category := "upstream_error"
	if errors.Is(err, context.Canceled) {
		category = "canceled"
	} else if errors.Is(err, context.DeadlineExceeded) {
		category = "timeout"
	}
	span.SetAttributes(attribute.String("error.type", category))
	span.SetStatus(codes.Error, category)
}
