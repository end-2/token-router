// Package app assembles the server and owns the lifetime of its dependencies.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"

	"token-router/src/internal/bucket"
	"token-router/src/internal/bucket/backend"
	"token-router/src/internal/config"
	"token-router/src/internal/observability/prometheus"
	"token-router/src/internal/observability/telemetry"
	"token-router/src/internal/proxy"
)

type application struct {
	cfg          config.Config
	logger       *slog.Logger
	limiter      bucket.Reconfigurable
	proxy        *proxyGeneration
	policyMu     sync.RWMutex
	policyClosed bool
	updateMu     sync.Mutex
	current      config.PolicyConfig
	fileDigest   [32]byte
	fileSeen     bool
	metrics      *prometheus.Metrics
	telemetry    *telemetry.Telemetry
	ready        atomic.Bool
}

// Run validates configuration, serves until cancellation and drains active requests.
func Run(ctx context.Context, cfg config.Config) (err error) {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate configuration: %w", err)
	}
	telemetry, err := telemetry.New(ctx, cfg.Telemetry, os.Stderr)
	if err != nil {
		return fmt.Errorf("initialize telemetry: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.Telemetry.ShutdownTimeout)
		defer cancel()
		if shutdownErr := telemetry.Shutdown(shutdownCtx); shutdownErr != nil {
			err = errors.Join(err, fmt.Errorf("telemetry shutdown: %w", shutdownErr))
		}
	}()
	logger := telemetry.Logger()
	limiter, err := backend.New(ctx, cfg.RateLimit)
	if err != nil {
		return fmt.Errorf("initialize rate limiter: %w", err)
	}
	defer func() { err = errors.Join(err, limiter.Close()) }()
	a := &application{cfg: cfg, logger: logger, limiter: limiter, telemetry: telemetry, current: cfg.Mutable()}
	upstream, err := a.newProxy()
	if err != nil {
		return fmt.Errorf("initialize upstream: %w", err)
	}
	a.proxy = &proxyGeneration{handler: upstream}
	defer func() {
		a.policyMu.Lock()
		defer a.policyMu.Unlock()
		a.policyClosed = true
		a.proxy.retire()
	}()
	if cfg.Telemetry.Metrics.Enabled && cfg.Telemetry.Metrics.Prometheus.Enabled {
		a.metrics, err = prometheus.New()
		if err != nil {
			return fmt.Errorf("initialize metrics: %w", err)
		}
	}
	return a.serve(ctx)
}

func (a *application) newProxy() (*proxy.Handler, error) {
	return a.newProxyFor(a.cfg.Mutable())
}

func (a *application) newProxyFor(cfg config.PolicyConfig) (*proxy.Handler, error) {
	var options []proxy.Option
	if a.telemetry != nil {
		options = append(options, proxy.WithTransport(a.telemetry.Transport))
	}
	if a.cfg.RateLimit.Enabled && cfg.RateLimit.TokensPerSecond > 0 {
		options = append(options, proxy.WithUsageRecorder(a.recordUsage))
	}
	return proxy.New(cfg.Upstream, a.logger, options...)
}

func (a *application) recordUsage(ctx context.Context, tokens int64) {
	// Confirmed upstream usage still counts after a client disconnect or timeout.
	// Shared backends bound this detached operation with their command timeout.
	if err := a.limiter.RecordUsage(context.WithoutCancel(ctx), tokens); err != nil {
		a.logger.WarnContext(ctx, "token usage could not be recorded")
	}
}

func (a *application) handler() http.Handler {
	e := echo.New()
	e.Logger = a.logger
	if a.telemetry != nil {
		e.Use(a.telemetry.Middleware)
	}
	if a.metrics != nil {
		e.Use(a.metrics.Middleware)
	}
	e.Use(middleware.Recover())
	e.GET("/healthz", func(c *echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})
	e.GET("/readyz", func(c *echo.Context) error {
		if !a.ready.Load() {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
		}
		return c.JSON(http.StatusOK, map[string]string{"status": "ready"})
	})
	e.Any("/v1", a.forward)
	e.Any("/v1/*", a.forward)
	return e
}

func (a *application) forward(c *echo.Context) error {
	if !a.ready.Load() {
		return apiError(c, http.StatusServiceUnavailable, "server_unavailable", "Server is shutting down")
	}
	// Admission and proxy selection observe the same update. Streams release this
	// lock before forwarding so that an open response cannot block configuration.
	a.policyMu.RLock()
	if a.cfg.RateLimit.Enabled && a.current.RateLimit.Account.Enabled {
		identifier, ok := accountIdentifier(c.Request(), a.current.RateLimit.Account)
		if !ok {
			a.policyMu.RUnlock()
			return apiError(c, http.StatusUnauthorized, "rate_limit_account_required", "A valid account identifier is required")
		}
		c.SetRequest(c.Request().WithContext(bucket.WithAccount(c.Request().Context(), identifier)))
	}
	p := a.proxy
	p.active.Add(1)
	decision, err := a.limiter.Allow(c.Request().Context())
	a.policyMu.RUnlock()
	defer p.release()
	if a.metrics != nil {
		a.metrics.RecordLimit(decision.Allowed, err)
	}
	if a.telemetry != nil {
		a.telemetry.RecordLimit(c.Request().Context(), decision.Allowed, err)
	}
	if err != nil {
		a.logger.WarnContext(c.Request().Context(), "rate limiter unavailable")
		return apiError(c, http.StatusServiceUnavailable, "rate_limiter_unavailable", "Rate limiter is unavailable")
	}
	if !decision.Allowed {
		retry := max(1, int(math.Ceil(decision.RetryAfter.Seconds())))
		c.Response().Header().Set("Retry-After", strconv.Itoa(retry))
		return apiError(c, http.StatusTooManyRequests, "rate_limit_exceeded", "Rate limit exceeded")
	}
	p.handler.ServeHTTP(proxyResponseWriter{ResponseWriter: c.Response()}, c.Request())
	return nil
}

func apiError(c *echo.Context, status int, code, message string) error {
	return c.JSON(status, map[string]any{"error": map[string]string{
		"type": code, "code": code, "message": message,
	}})
}

func (a *application) serve(ctx context.Context) error {
	// Request contexts survive the termination signal so that HTTP Shutdown can drain them.
	requestCtx, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRequests()
	servers := []*http.Server{{
		Addr: a.cfg.Server.Address, Handler: a.handler(),
		ReadHeaderTimeout: a.cfg.Server.ReadHeaderTimeout, ReadTimeout: a.cfg.Server.ReadTimeout,
		WriteTimeout: a.cfg.Server.WriteTimeout, IdleTimeout: a.cfg.Server.IdleTimeout,
		MaxHeaderBytes: a.cfg.Server.MaxHeaderBytes,
		BaseContext:    func(net.Listener) context.Context { return requestCtx },
		ErrorLog:       slog.NewLogLogger(a.logger.Handler(), slog.LevelError),
	}}
	if a.cfg.Admin.Enabled {
		servers = append(servers, &http.Server{
			Addr: a.cfg.Admin.Address, Handler: a.adminHandler(),
			ReadHeaderTimeout: a.cfg.Server.ReadHeaderTimeout,
			ReadTimeout:       10 * time.Second, WriteTimeout: 10 * time.Second,
			IdleTimeout: a.cfg.Server.IdleTimeout, MaxHeaderBytes: a.cfg.Server.MaxHeaderBytes,
			BaseContext: func(net.Listener) context.Context { return requestCtx },
			ErrorLog:    slog.NewLogLogger(a.logger.Handler(), slog.LevelError),
		})
	}
	if a.metrics != nil {
		mux := http.NewServeMux()
		mux.Handle(a.cfg.Telemetry.Metrics.Prometheus.Path, a.metrics.Handler())
		servers = append(servers, &http.Server{
			Addr: a.cfg.Telemetry.Metrics.Prometheus.Address, Handler: mux,
			ReadHeaderTimeout: a.cfg.Server.ReadHeaderTimeout,
			ReadTimeout:       10 * time.Second, WriteTimeout: 10 * time.Second,
			IdleTimeout: a.cfg.Server.IdleTimeout, MaxHeaderBytes: a.cfg.Server.MaxHeaderBytes,
			BaseContext: func(net.Listener) context.Context { return requestCtx },
			ErrorLog:    slog.NewLogLogger(a.logger.Handler(), slog.LevelError),
		})
	}
	listeners := make([]net.Listener, 0, len(servers))
	for _, server := range servers {
		listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", server.Addr)
		if err != nil {
			errs := []error{fmt.Errorf("listen on %s: %w", server.Addr, err)}
			for _, opened := range listeners {
				errs = append(errs, opened.Close())
			}
			return errors.Join(errs...)
		}
		listeners = append(listeners, listener)
	}
	watchCtx, stopWatch := context.WithCancel(ctx)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		a.watchPolicy(watchCtx)
	}()
	defer func() {
		stopWatch()
		<-watchDone
	}()
	return a.serveListeners(ctx, servers, listeners, cancelRequests)
}

func (a *application) serveListeners(ctx context.Context, servers []*http.Server, listeners []net.Listener, cancelRequests context.CancelFunc) error {
	results := make(chan error, len(servers))
	a.ready.Store(true)
	for i, server := range servers {
		go func() {
			results <- server.Serve(listeners[i])
		}()
		a.logger.Info("HTTP listener started", "address", listeners[i].Addr().String())
	}
	pending := len(servers)
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-results:
		pending--
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
	}
	a.ready.Store(false)
	a.policyMu.Lock()
	a.policyClosed = true
	a.policyMu.Unlock()
	a.logger.Info("shutting down HTTP listeners", "timeout", a.cfg.Server.ShutdownTimeout)
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.cfg.Server.ShutdownTimeout)
	defer cancel()
	shutdownErrors := make([]error, len(servers))
	var wg sync.WaitGroup
	for i, server := range servers {
		wg.Go(func() {
			if err := server.Shutdown(shutdownCtx); err != nil {
				cancelRequests()
				shutdownErrors[i] = errors.Join(fmt.Errorf("HTTP shutdown: %w", err), server.Close())
			}
		})
	}
	wg.Wait()
	cancelRequests()
	for range pending {
		if err := <-results; !errors.Is(err, http.ErrServerClosed) {
			serveErr = errors.Join(serveErr, err)
		}
	}
	return errors.Join(serveErr, errors.Join(shutdownErrors...))
}
