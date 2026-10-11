package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"token-router/src/internal/bucket"
	"token-router/src/internal/config"
	"token-router/src/internal/observability"
)

type fixedLimiter struct {
	decision bucket.Decision
	err      error
	calls    int
}

func (l *fixedLimiter) Allow(context.Context) (bucket.Decision, error) {
	l.calls++
	return l.decision, l.err
}

func (*fixedLimiter) Close() error { return nil }

func (*fixedLimiter) RecordUsage(context.Context, int64) error { return nil }

func (*fixedLimiter) Update(config.LimitValues) {}

func testApplication(t *testing.T, upstream http.Handler, limiter bucket.Reconfigurable, configure ...func(*config.Config)) *application {
	t.Helper()
	u := httptest.NewServer(upstream)
	t.Cleanup(u.Close)
	cfg := config.Defaults()
	for _, apply := range configure {
		apply(&cfg)
	}
	cfg.Upstream.BaseURLs = []string{u.URL}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &application{cfg: cfg, logger: logger, limiter: limiter}
	p, err := a.newProxy()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.CloseIdleConnections)
	m, err := observability.New()
	if err != nil {
		t.Fatal(err)
	}
	a.proxy, a.metrics = &proxyGeneration{handler: p}, m
	a.current = cfg.Mutable()
	a.ready.Store(true)
	return a
}

func TestRoutesAndRateLimit(t *testing.T) {
	for _, tc := range []struct {
		name, path, metric string
		decision           bucket.Decision
		err                error
		status, calls      int
	}{
		{name: "proxy", path: "/v1/models", decision: bucket.Decision{Allowed: true}, status: 201, calls: 1, metric: `result="allowed"`},
		{name: "denied", path: "/v1/models", decision: bucket.Decision{RetryAfter: 1500 * time.Millisecond}, status: 429, calls: 1, metric: `result="denied"`},
		{name: "backend failure", path: "/v1/models", err: errors.New("sensitive backend details"), status: 503, calls: 1, metric: `result="error"`},
		{name: "health bypass", path: "/healthz", status: 200},
		{name: "readiness bypass", path: "/readyz", status: 200},
		{name: "unknown", path: "/secret/random/path", status: 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limiter := &fixedLimiter{decision: tc.decision, err: tc.err}
			upstreamCalls := 0
			a := testApplication(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls++
				w.WriteHeader(http.StatusCreated)
			}), limiter)
			r := httptest.NewRecorder()
			a.handler().ServeHTTP(r, httptest.NewRequestWithContext(t.Context(), http.MethodGet, tc.path, nil))
			if r.Code != tc.status || limiter.calls != tc.calls {
				t.Fatalf("status=%d calls=%d, want status=%d calls=%d", r.Code, limiter.calls, tc.status, tc.calls)
			}
			if tc.status != 201 && upstreamCalls != 0 {
				t.Fatal("blocked or local route reached upstream")
			}
			if tc.status == 429 && r.Header().Get("Retry-After") != "2" {
				t.Fatalf("Retry-After=%q", r.Header().Get("Retry-After"))
			}
			if strings.Contains(r.Body.String(), "sensitive") {
				t.Fatal("backend details exposed")
			}
			metrics := httptest.NewRecorder()
			a.metrics.Handler().ServeHTTP(metrics, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
			if !strings.Contains(metrics.Body.String(), `status="`+httpStatus(tc.status)+`"`) {
				t.Fatal("final response status missing from metrics")
			}
			if tc.metric != "" && !strings.Contains(metrics.Body.String(), tc.metric) {
				t.Fatal("rate limiter outcome missing from metrics")
			}
			if strings.Contains(metrics.Body.String(), "/secret/random/path") {
				t.Fatal("raw path used as metric label")
			}
		})
	}
}

func httpStatus(code int) string {
	return fmt.Sprint(code)
}

func TestNotReadyRejectsNewRequests(t *testing.T) {
	limiter := &fixedLimiter{decision: bucket.Decision{Allowed: true}}
	a := testApplication(t, http.NotFoundHandler(), limiter)
	a.ready.Store(false)
	for _, path := range []string{"/readyz", "/v1/models"} {
		r := httptest.NewRecorder()
		a.handler().ServeHTTP(r, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if r.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s returned %d", path, r.Code)
		}
	}
	if limiter.calls != 0 {
		t.Fatal("shutdown request consumed bucket capacity")
	}
}

func TestShutdownDrainsAndEnforcesDeadline(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%t", force), func(t *testing.T) {
			entered, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
			requestCtx, cancelRequests := context.WithCancel(context.Background())
			defer cancelRequests()
			listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			server := &http.Server{
				ReadHeaderTimeout: time.Second,
				BaseContext:       func(net.Listener) context.Context { return requestCtx },
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					close(entered)
					defer close(exited)
					select {
					case <-release:
						w.WriteHeader(http.StatusNoContent)
					case <-r.Context().Done():
					}
				}),
			}
			cfg := config.Defaults()
			cfg.Server.ShutdownTimeout = 150 * time.Millisecond
			if !force {
				cfg.Server.ShutdownTimeout = 2 * time.Second
			}
			a := &application{cfg: cfg, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- a.serveListeners(ctx, []*http.Server{server}, []net.Listener{listener}, cancelRequests)
			}()
			response := make(chan error, 1)
			go func() {
				client := &http.Client{Timeout: 3 * time.Second}
				request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+listener.Addr().String(), nil)
				if err != nil {
					response <- err
					return
				}
				r, err := client.Do(request)
				if err == nil {
					err = r.Body.Close()
					if r.StatusCode != http.StatusNoContent {
						err = fmt.Errorf("response status %d", r.StatusCode)
					}
				}
				response <- err
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("request did not enter handler")
			}
			cancel()
			if !force {
				select {
				case <-exited:
					t.Fatal("request was canceled before drain finished")
				case <-time.After(30 * time.Millisecond):
				}
				close(release)
			}
			select {
			case err := <-done:
				if force != errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("force=%t shutdown error=%v", force, err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("shutdown did not finish")
			}
			if a.ready.Load() {
				t.Fatal("readiness remained enabled")
			}
			select {
			case <-exited:
			case <-time.After(time.Second):
				t.Fatal("handler did not exit")
			}
			if err := <-response; !force && err != nil {
				t.Fatalf("drained request failed: %v", err)
			}
		})
	}
}

func TestPrometheusListenerSelectionWithoutOTLP(t *testing.T) {
	for _, tc := range []struct {
		name                string
		metrics, prometheus bool
	}{
		{"enabled", true, true},
		{"metrics disabled", false, true},
		{"Prometheus disabled", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			occupied, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := occupied.Close(); err != nil {
					t.Error(err)
				}
			})
			cfg := config.Defaults()
			cfg.Server.Address = "127.0.0.1:0"
			cfg.Telemetry.Logs.Enabled = false
			cfg.Telemetry.Metrics.Enabled = tc.metrics
			cfg.Telemetry.Metrics.Prometheus.Enabled = tc.prometheus
			cfg.Telemetry.Metrics.Prometheus.Address = occupied.Addr().String()
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			err = Run(ctx, cfg)
			if tc.metrics && tc.prometheus {
				if err == nil || !strings.Contains(err.Error(), "listen on "+occupied.Addr().String()) {
					t.Fatalf("enabled Prometheus must attempt to bind its address: %v", err)
				}
			} else if err != nil {
				t.Fatalf("disabled Prometheus must not bind its address: %v", err)
			}
		})
	}
}

func TestMetricsListenFailureClosesAPIListener(t *testing.T) {
	occupied, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := occupied.Close(); err != nil {
			t.Error(err)
		}
	})
	available, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := available.Addr().String()
	if err := available.Close(); err != nil {
		t.Fatal(err)
	}
	a := testApplication(t, http.NotFoundHandler(), &fixedLimiter{})
	a.cfg.Server.Address = address
	a.cfg.Telemetry.Metrics.Prometheus.Address = occupied.Addr().String()
	if err := a.serve(context.Background()); err == nil {
		t.Fatal("expected metrics bind failure")
	}
	reopened, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", address)
	if err != nil {
		t.Fatalf("API listener leaked: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}
