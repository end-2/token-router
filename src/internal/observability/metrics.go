// Package observability provides application-scoped metrics, logs and traces.
package observability

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics records bounded HTTP labels and rate limiter outcomes.
type Metrics struct {
	registry      *prometheus.Registry
	requests      *prometheus.CounterVec
	duration      *prometheus.HistogramVec
	inFlight      prometheus.Gauge
	limits        *prometheus.CounterVec
	limitCounters [3]func() prometheus.Counter
}

// New creates collectors without changing the process-wide default registry.
func New() (*Metrics, error) {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "token_router_http_requests_total", Help: "Completed HTTP requests by method, route and status.",
		}, []string{"method", "route", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "token_router_http_request_duration_seconds", Help: "HTTP request duration including streamed responses.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "token_router_http_requests_in_flight", Help: "HTTP requests currently being served.",
		}),
		limits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "token_router_rate_limit_decisions_total", Help: "Rate limiter outcomes: allowed, denied or error.",
		}, []string{"result"}),
	}
	for _, collector := range []prometheus.Collector{
		m.requests, m.duration, m.inFlight, m.limits, collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	} {
		if err := m.registry.Register(collector); err != nil {
			return nil, err
		}
	}
	// Create each series on its first observation and reuse the counter afterwards.
	for i, outcome := range limitOutcomes {
		m.limitCounters[i] = sync.OnceValue(func() prometheus.Counter {
			return m.limits.WithLabelValues(outcome)
		})
	}
	return m, nil
}

// Handler exposes this instance's registry in Prometheus format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Middleware measures final responses without buffering bodies or SSE streams.
func (m *Metrics) Middleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		start := time.Now()
		m.inFlight.Inc()
		defer m.inFlight.Dec()
		defer func() {
			method := boundedMethod(c.Request().Method)
			route := c.Path()
			if route == "" {
				route = "unmatched"
			}
			status := http.StatusOK
			if response, err := echo.UnwrapResponse(c.Response()); err == nil {
				status = response.Status
			}
			m.requests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
			m.duration.WithLabelValues(method, route).Observe(time.Since(start).Seconds())
		}()
		if err := next(c); err != nil {
			c.Echo().HTTPErrorHandler(c, err)
		}
		return nil
	}
}

// RecordLimit records one of the fixed rate limiter outcome labels.
func (m *Metrics) RecordLimit(allowed bool, err error) {
	m.limitCounters[limitOutcome(allowed, err)]().Inc()
}

func boundedMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace, http.MethodPatch:
		return method
	default:
		return "OTHER"
	}
}
