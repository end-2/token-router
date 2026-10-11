package observability

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	logscollector "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricscollector "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracecollector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"token-router/src/internal/config"
)

type telemetryCapture struct {
	mu       sync.Mutex
	traces   []*tracecollector.ExportTraceServiceRequest
	metrics  []*metricscollector.ExportMetricsServiceRequest
	logs     []*logscollector.ExportLogsServiceRequest
	requests []string
}

func newCapture(t *testing.T) (*telemetryCapture, *httptest.Server) {
	t.Helper()
	capture := &telemetryCapture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read export: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		capture.mu.Lock()
		defer capture.mu.Unlock()
		capture.requests = append(capture.requests, r.URL.Path)
		var message proto.Message
		switch r.URL.Path {
		case "/otlp/v1/traces":
			request := &tracecollector.ExportTraceServiceRequest{}
			capture.traces = append(capture.traces, request)
			message = request
		case "/otlp/v1/metrics":
			request := &metricscollector.ExportMetricsServiceRequest{}
			capture.metrics = append(capture.metrics, request)
			message = request
		case "/otlp/v1/logs":
			request := &logscollector.ExportLogsServiceRequest{}
			capture.logs = append(capture.logs, request)
			message = request
		default:
			t.Errorf("unexpected exporter path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := proto.Unmarshal(data, message); err != nil {
			t.Errorf("decode export: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	return capture, server
}

func telemetryConfig(endpoint string) config.Telemetry {
	cfg := config.Defaults().Telemetry
	cfg.OTLP.Enabled = true
	cfg.ServiceName = "test-router"
	cfg.ServiceVersion = "test-version"
	cfg.Environment = "test"
	cfg.OTLP.Endpoint = endpoint + "/otlp/"
	cfg.OTLP.Timeout = time.Second
	cfg.Metrics.ExportInterval = time.Hour
	cfg.Traces.SampleRatio = 1
	return cfg
}

func shutdownTelemetry(t *testing.T, telemetry *Telemetry) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := telemetry.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown telemetry: %v", err)
	}
}

func TestTelemetryExportsCorrelatedSanitizedSignalsOnShutdown(t *testing.T) {
	capture, receiver := newCapture(t)
	var output bytes.Buffer
	telemetry, err := NewTelemetry(context.Background(), telemetryConfig(receiver.URL), &output)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { shutdownTelemetry(t, telemetry) })
	const incomingTrace = "0af7651916cd43dd8448eb211c80319c"
	const incomingParent = "b7ad6b7169203331"
	upstreamParents := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamParents <- r.Header.Get("traceparent")
		w.WriteHeader(http.StatusCreated)
		if _, err := io.WriteString(w, "private-response"); err != nil {
			t.Errorf("write upstream: %v", err)
		}
	}))
	defer upstream.Close()
	client := &http.Client{Transport: telemetry.Transport(http.DefaultTransport), Timeout: time.Second}
	e := echo.New()
	e.Use(telemetry.Middleware)
	e.Any("/v1/*", func(c *echo.Context) error {
		telemetry.RecordLimit(c.Request().Context(), true, nil)
		request, err := http.NewRequestWithContext(c.Request().Context(), c.Request().Method, upstream.URL+"/private-path?token=private-query", strings.NewReader("private-body"))
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer private-auth")
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		defer func() {
			if err := response.Body.Close(); err != nil {
				t.Errorf("close response: %v", err)
			}
		}()
		c.Response().WriteHeader(response.StatusCode)
		_, err = io.Copy(c.Response(), response.Body)
		return err
	})
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/private-path?token=private-query", strings.NewReader("private-body"))
	request.Header.Set("traceparent", "00-"+incomingTrace+"-"+incomingParent+"-01")
	response := httptest.NewRecorder()
	e.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || response.Body.String() != "private-response" {
		t.Fatalf("proxy response: %d %s", response.Code, response.Body.String())
	}
	telemetry.Logger().Debug("filtered log")
	shutdownTelemetry(t, telemetry)
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if len(capture.traces) == 0 || len(capture.metrics) == 0 || len(capture.logs) == 0 {
		t.Fatalf("missing signals: traces=%d metrics=%d logs=%d", len(capture.traces), len(capture.metrics), len(capture.logs))
	}
	spans := capture.spans()
	if len(spans) != 2 {
		t.Fatalf("span count=%d", len(spans))
	}
	var serverSpan, clientSpan *tracepb.Span
	for _, span := range spans {
		switch span.Kind {
		case tracepb.Span_SPAN_KIND_SERVER:
			serverSpan = span
		case tracepb.Span_SPAN_KIND_CLIENT:
			clientSpan = span
		}
	}
	if serverSpan == nil || clientSpan == nil {
		t.Fatalf("missing server/client span: %v", spans)
	}
	if serverSpan.Name != "POST /v1/*" || clientSpan.Name != "POST upstream" ||
		hex.EncodeToString(serverSpan.TraceId) != incomingTrace || hex.EncodeToString(clientSpan.TraceId) != incomingTrace ||
		hex.EncodeToString(serverSpan.ParentSpanId) != incomingParent || !bytes.Equal(clientSpan.ParentSpanId, serverSpan.SpanId) {
		t.Fatalf("incorrect span tree: server=%v client=%v", serverSpan, clientSpan)
	}
	if upstreamParent := <-upstreamParents; upstreamParent != "00-"+incomingTrace+"-"+hex.EncodeToString(clientSpan.SpanId)+"-01" {
		t.Fatalf("upstream traceparent=%s", upstreamParent)
	}
	var access map[string]any
	if err := json.Unmarshal(output.Bytes(), &access); err != nil {
		t.Fatalf("JSON log: %v: %s", err, output.String())
	}
	if access["msg"] != "HTTP request completed" || access["route"] != "/v1/*" ||
		access["trace_id"] != incomingTrace || access["span_id"] != hex.EncodeToString(serverSpan.SpanId) || access["status"] != float64(http.StatusCreated) {
		t.Fatalf("access log=%v", access)
	}
	logs := capture.logRecords()
	if len(logs) != 1 || logs[0].Body.GetStringValue() != "HTTP request completed" ||
		!bytes.Equal(logs[0].TraceId, serverSpan.TraceId) || !bytes.Equal(logs[0].SpanId, serverSpan.SpanId) {
		t.Fatalf("OTLP logs=%v", logs)
	}
	metrics := capture.metricRecords()
	for _, name := range []string{"token_router.http.requests", "token_router.http.request.duration", "token_router.http.requests.in_flight", "token_router.rate_limit.decisions"} {
		if metrics[name] == nil {
			t.Errorf("missing metric %s", name)
		}
	}
	if got := metrics["token_router.http.requests"].GetSum().DataPoints[0].GetAsInt(); got != 1 {
		t.Errorf("request counter=%d", got)
	}
	requestAttrs := metrics["token_router.http.requests"].GetSum().DataPoints[0].Attributes
	if len(requestAttrs) != 3 || stringAttr(requestAttrs, "http.request.method") != http.MethodPost || stringAttr(requestAttrs, "http.route") != "/v1/*" {
		t.Errorf("request attributes=%v", requestAttrs)
	}
	for _, attr := range requestAttrs {
		if attr.Key == "http.response.status_code" && attr.Value.GetIntValue() != http.StatusCreated {
			t.Errorf("request status attribute=%v", attr)
		}
	}
	durationAttrs := metrics["token_router.http.request.duration"].GetHistogram().DataPoints[0].Attributes
	if len(durationAttrs) != 2 || stringAttr(durationAttrs, "http.request.method") != http.MethodPost || stringAttr(durationAttrs, "http.route") != "/v1/*" {
		t.Errorf("duration attributes=%v", durationAttrs)
	}
	if got := metrics["token_router.http.requests.in_flight"].GetSum().DataPoints[0].GetAsInt(); got != 0 {
		t.Errorf("in flight=%d", got)
	}
	resourceAttrs := capture.traces[0].ResourceSpans[0].Resource.Attributes
	if stringAttr(resourceAttrs, "service.name") != "test-router" || stringAttr(resourceAttrs, "service.version") != "test-version" ||
		stringAttr(resourceAttrs, "deployment.environment.name") != "test" || stringAttr(resourceAttrs, "service.instance.id") == "" {
		t.Fatalf("resource=%v", resourceAttrs)
	}
	for _, serialized := range []string{output.String(), protojson.Format(capture.traces[0]), protojson.Format(capture.metrics[0]), protojson.Format(capture.logs[0])} {
		for _, secret := range []string{"private-path", "private-query", "private-body", "private-auth", "private-response", "filtered log"} {
			if strings.Contains(serialized, secret) {
				t.Errorf("telemetry includes %q", secret)
			}
		}
	}
}

func TestLocalLogConfigurationWithoutOTLP(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		t.Run(format, func(t *testing.T) {
			cfg := config.Defaults().Telemetry
			cfg.Logs.Level, cfg.Logs.Format = "warn", format
			var output bytes.Buffer
			telemetry, err := NewTelemetry(context.Background(), cfg, &output)
			if err != nil {
				t.Fatal(err)
			}
			telemetry.Logger().Info("filtered message")
			telemetry.Logger().Warn("visible message")
			shutdownTelemetry(t, telemetry)
			if strings.Contains(output.String(), "filtered message") || !strings.Contains(output.String(), "visible message") {
				t.Fatalf("log level was not applied: %s", output.String())
			}
			if format == "json" {
				var entry map[string]any
				if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
					t.Fatal(err)
				}
				if entry["level"] != "WARN" || entry["msg"] != "visible message" {
					t.Fatalf("unexpected JSON log: %v", entry)
				}
			} else if !strings.Contains(output.String(), `level=WARN msg="visible message"`) {
				t.Fatalf("unexpected text log: %s", output.String())
			}
		})
	}
}

func TestTelemetrySignalSelectionAndInstanceIsolation(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		enabled, traces, logs bool
		metrics               bool
	}{
		{name: "OTLP disabled", traces: true, metrics: true, logs: true},
		{name: "all disabled"},
		{name: "traces", enabled: true, traces: true},
		{name: "metrics", enabled: true, metrics: true},
		{name: "logs", enabled: true, logs: true},
		{name: "none", enabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture, receiver := newCapture(t)
			cfg := telemetryConfig(receiver.URL)
			cfg.OTLP.Enabled, cfg.Traces.Enabled, cfg.Metrics.Enabled, cfg.Logs.Enabled = tc.enabled, tc.traces, tc.metrics, tc.logs
			cfg.ServiceInstanceID = "explicit-instance"
			var output bytes.Buffer
			telemetry, err := NewTelemetry(context.Background(), cfg, &output)
			if err != nil {
				t.Fatal(err)
			}
			e := echo.New()
			e.Use(telemetry.Middleware)
			e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), "PRIVATE_METHOD", "/private-identifier", nil))
			shutdownTelemetry(t, telemetry)
			capture.mu.Lock()
			defer capture.mu.Unlock()
			if (len(capture.traces) > 0) != (tc.enabled && tc.traces) || (len(capture.metrics) > 0) != (tc.enabled && tc.metrics) || (len(capture.logs) > 0) != (tc.enabled && tc.logs) {
				t.Fatalf("signals: traces=%d metrics=%d logs=%d", len(capture.traces), len(capture.metrics), len(capture.logs))
			}
			if tc.logs {
				var access map[string]any
				if err := json.Unmarshal(output.Bytes(), &access); err != nil {
					t.Fatal(err)
				}
				if access["method"] != "OTHER" || access["route"] != "unmatched" || access["status"] != float64(http.StatusNotFound) {
					t.Fatalf("bounded access attrs=%v", access)
				}
				if (!tc.enabled || !tc.traces) && (access["trace_id"] != nil || access["span_id"] != nil) {
					t.Fatalf("disabled traces created correlation: %v", access)
				}
			} else if output.Len() != 0 {
				t.Fatalf("disabled logs produced output: %s", output.String())
			}
			if tc.enabled && tc.traces && stringAttr(capture.traces[0].ResourceSpans[0].Resource.Attributes, "service.instance.id") != "explicit-instance" {
				t.Fatal("configured instance identity was not exported")
			}
		})
	}
}

func TestDisabledRequestTelemetryPreservesErrorHandling(t *testing.T) {
	for _, logs := range []bool{false, true} {
		name := "disabled logs"
		if logs {
			name = "filtered logs"
		}
		t.Run(name, func(t *testing.T) {
			cfg := config.Defaults().Telemetry
			cfg.Logs.Enabled, cfg.Logs.Level = logs, "warn"
			var output bytes.Buffer
			telemetry, err := NewTelemetry(context.Background(), cfg, &output)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { shutdownTelemetry(t, telemetry) })
			e := echo.New()
			e.Use(telemetry.Middleware)
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/failure", nil)
			e.GET("/failure", func(c *echo.Context) error {
				if c.Request() != request {
					t.Error("disabled telemetry replaced the request")
				}
				return echo.NewHTTPError(http.StatusConflict, "conflict")
			})
			handleError := e.HTTPErrorHandler
			errorCalls := 0
			e.HTTPErrorHandler = func(c *echo.Context, err error) {
				errorCalls++
				handleError(c, err)
			}
			response := httptest.NewRecorder()
			e.ServeHTTP(response, request)
			if response.Code != http.StatusConflict || errorCalls != 1 || output.Len() != 0 {
				t.Fatalf("status=%d error calls=%d output=%s", response.Code, errorCalls, output.String())
			}
		})
	}
}

func TestConcurrentLimitMeasurements(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	telemetry := &Telemetry{metrics: provider}
	if err := telemetry.createInstruments(provider.Meter(instrumentationName)); err != nil {
		t.Fatal(err)
	}
	metrics, err := New()
	if err != nil {
		t.Fatal(err)
	}
	families, err := metrics.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "token_router_rate_limit_decisions_total" {
			t.Fatal("limit series appeared before a decision")
		}
	}
	const workers, decisions = 12, 20
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range decisions {
				for _, tc := range []struct {
					allowed bool
					err     error
				}{{allowed: true}, {}, {allowed: true, err: io.ErrUnexpectedEOF}} {
					telemetry.RecordLimit(context.Background(), tc.allowed, tc.err)
					metrics.RecordLimit(tc.allowed, tc.err)
				}
			}
		})
	}
	wg.Wait()
	want := map[string]int64{"allowed": workers * decisions, "denied": workers * decisions, "error": workers * decisions}
	var exported metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &exported); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int64{}
	for _, scope := range exported.ScopeMetrics {
		for _, measurement := range scope.Metrics {
			if measurement.Name != "token_router.rate_limit.decisions" {
				continue
			}
			for _, point := range measurement.Data.(metricdata.Sum[int64]).DataPoints {
				result, ok := point.Attributes.Value("result")
				if !ok {
					t.Fatal("missing result attribute")
				}
				seen[result.AsString()] = point.Value
			}
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("OTLP limit series=%v", seen)
	}
	for outcome, count := range want {
		if seen[outcome] != count {
			t.Errorf("OTLP result=%s count=%d want=%d", outcome, seen[outcome], count)
		}
	}
	families, err = metrics.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	seen = map[string]int64{}
	for _, family := range families {
		if family.GetName() != "token_router_rate_limit_decisions_total" {
			continue
		}
		for _, measurement := range family.Metric {
			for _, label := range measurement.Label {
				if label.GetName() == "result" {
					seen[label.GetValue()] = int64(measurement.GetCounter().GetValue())
				}
			}
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("Prometheus limit series=%v", seen)
	}
	for outcome, count := range want {
		if seen[outcome] != count {
			t.Errorf("Prometheus result=%s count=%d want=%d", outcome, seen[outcome], count)
		}
	}
}

func TestUnsampledTransportPreservesPropagationAndResponse(t *testing.T) {
	for _, failure := range []bool{false, true} {
		name := "response body"
		if failure {
			name = "transport error"
		}
		t.Run(name, func(t *testing.T) {
			provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample()))
			t.Cleanup(func() {
				if err := provider.Shutdown(context.Background()); err != nil {
					t.Error(err)
				}
			})
			body := io.NopCloser(strings.NewReader("stream"))
			response := &http.Response{StatusCode: http.StatusOK, Body: body}
			var transportErr error
			if failure {
				response, transportErr = nil, io.ErrUnexpectedEOF
			}
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://upstream/v1/chat/completions", nil)
			request.Header.Set("traceparent", "private-parent")
			request.Header.Set("tracestate", "private-state")
			transport := tracedTransport{
				tracer: provider.Tracer(instrumentationName), propagator: propagation.TraceContext{},
				base: roundTripperFunc(func(outgoing *http.Request) (*http.Response, error) {
					parent := outgoing.Header.Get("traceparent")
					if parent == "private-parent" || !strings.HasSuffix(parent, "-00") || outgoing.Header.Get("tracestate") != "" {
						t.Errorf("unsampled propagation: %v", outgoing.Header)
					}
					return response, transportErr
				}),
			}
			got, err := transport.RoundTrip(request)
			if got != response || !errors.Is(err, transportErr) {
				t.Fatalf("response=%v err=%v", got, err)
			}
			if !failure && got.Body != body {
				t.Error("nonrecording span wrapped the response body")
			}
			if request.Header.Get("traceparent") != "private-parent" || request.Header.Get("tracestate") != "private-state" {
				t.Errorf("original headers changed: %v", request.Header)
			}
			if got != nil {
				if err := got.Body.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestTransportSpanEndsWithResponseBodyAndSanitizesErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		closeOnly bool
		failure   bool
		canceled  bool
	}{
		{name: "EOF"},
		{name: "close", closeOnly: true},
		{name: "failure", failure: true},
		{name: "canceled close", closeOnly: true, canceled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exporter := tracetest.NewInMemoryExporter()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
			t.Cleanup(func() {
				if err := provider.Shutdown(context.Background()); err != nil {
					t.Error(err)
				}
			})
			transport := tracedTransport{tracer: provider.Tracer(instrumentationName), propagator: propagation.TraceContext{}, base: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				if request.Header.Get("traceparent") == "" {
					t.Error("missing propagation")
				}
				if tc.failure {
					return nil, errors.New("private transport error with credentials")
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("data: first\n\n"))}, nil
			})}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request := httptest.NewRequestWithContext(ctx, http.MethodPost, "http://upstream/private-query?token=secret", nil)
			response, err := transport.RoundTrip(request)
			if request.Header.Get("traceparent") != "" {
				t.Error("original request headers mutated")
			}
			if tc.failure {
				if err == nil || len(exporter.GetSpans()) != 1 {
					t.Fatal("failed transport did not end span")
				}
				if got := exporter.GetSpans()[0].Status.Description; got != "upstream_error" {
					t.Fatalf("unsafe span status=%s", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(exporter.GetSpans()) != 0 {
				t.Fatal("client span ended before stream consumption")
			}
			if !tc.closeOnly {
				if _, err := io.ReadAll(response.Body); err != nil {
					t.Fatal(err)
				}
			}
			if tc.canceled {
				cancel()
			}
			if err := response.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if len(exporter.GetSpans()) != 1 {
				t.Fatalf("span completion count=%d", len(exporter.GetSpans()))
			}
			if tc.canceled && exporter.GetSpans()[0].Status.Description != "canceled" {
				t.Fatal("canceled response body was recorded as successful")
			}
		})
	}
}

func TestTelemetryShutdownDeadlineCancelsOngoingExport(t *testing.T) {
	started := make(chan struct{}, 1)
	finished := make(chan struct{}, 1)
	receiver := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		if _, err := io.Copy(io.Discard, request.Body); err != nil {
			return
		}
		started <- struct{}{}
		select {
		case <-request.Context().Done():
		case <-time.After(2 * time.Second):
			t.Error("export request outlived shutdown deadline")
		}
		finished <- struct{}{}
	}))
	defer receiver.Close()
	cfg := telemetryConfig(receiver.URL)
	cfg.Metrics.Enabled, cfg.Logs.Enabled = false, false
	telemetry, err := NewTelemetry(context.Background(), cfg, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	_, span := telemetry.tracer.Start(context.Background(), "pending span")
	span.End()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := telemetry.Shutdown(ctx); err == nil {
		t.Fatal("expected shutdown timeout")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not attempt final export")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("export was not canceled when shutdown returned")
	}
}

func TestTelemetryExportFailureIsStructuredAndFailsShutdown(t *testing.T) {
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "private backend response", http.StatusBadRequest)
	}))
	defer receiver.Close()
	cfg := telemetryConfig(receiver.URL)
	var output synchronizedBuffer
	telemetry, err := NewTelemetry(context.Background(), cfg, &output)
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	e.Use(telemetry.Middleware)
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/secret", nil))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := telemetry.Shutdown(ctx); err == nil {
		t.Fatal("shutdown succeeded although no signal was delivered")
	}
	decoder := json.NewDecoder(strings.NewReader(output.String()))
	warnings := map[string]bool{}
	for decoder.More() {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatalf("non-JSON export failure: %v", err)
		}
		if record["msg"] == "OTLP export failed; batch was not delivered" {
			signal, ok := record["signal"].(string)
			if !ok {
				t.Fatal("missing signal")
			}
			warnings[signal] = true
		}
	}
	if len(warnings) != 3 || strings.Contains(output.String(), "private backend response") || strings.Contains(output.String(), receiver.URL) {
		t.Fatalf("unsafe or missing failures: %s", output.String())
	}
}

type synchronizedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *synchronizedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(data)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func (c *telemetryCapture) spans() []*tracepb.Span {
	var spans []*tracepb.Span
	for _, request := range c.traces {
		for _, resource := range request.ResourceSpans {
			for _, scope := range resource.ScopeSpans {
				spans = append(spans, scope.Spans...)
			}
		}
	}
	return spans
}

func (c *telemetryCapture) logRecords() []*logspb.LogRecord {
	var records []*logspb.LogRecord
	for _, request := range c.logs {
		for _, resource := range request.ResourceLogs {
			for _, scope := range resource.ScopeLogs {
				records = append(records, scope.LogRecords...)
			}
		}
	}
	return records
}

func (c *telemetryCapture) metricRecords() map[string]*metricspb.Metric {
	metrics := make(map[string]*metricspb.Metric)
	for _, request := range c.metrics {
		for _, resource := range request.ResourceMetrics {
			for _, scope := range resource.ScopeMetrics {
				for _, metric := range scope.Metrics {
					metrics[metric.Name] = metric
				}
			}
		}
	}
	return metrics
}

func stringAttr(attrs []*commonpb.KeyValue, key string) string {
	for _, attr := range attrs {
		if attr.Key == key {
			return attr.Value.GetStringValue()
		}
	}
	return ""
}
