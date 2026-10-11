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
	"token-router/src/internal/config"
	"token-router/src/internal/observability"
	"token-router/src/internal/proxy"
)

type application struct {
	cfg       config.Config
	logger    *slog.Logger
	limiter   bucket.Limiter
	proxy     *proxy.Handler
	metrics   *observability.Metrics
	telemetry *observability.Telemetry
	ready     atomic.Bool
}

// Run validates configuration, serves until cancellation and drains active requests.
func Run(ctx context.Context, cfg config.Config) (err error) {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate configuration: %w", err)
	}
	telemetry, err := observability.NewTelemetry(ctx, cfg.Telemetry, cfg.Log, os.Stderr)
	if err != nil {
		return fmt.Errorf("initialize telemetry: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Telemetry.ShutdownTimeout)
		defer cancel()
		if shutdownErr := telemetry.Shutdown(shutdownCtx); shutdownErr != nil {
			err = errors.Join(err, fmt.Errorf("telemetry shutdown: %w", shutdownErr))
		}
	}()
	logger := telemetry.Logger()
	limiter, err := bucket.New(ctx, cfg.RateLimit)
	if err != nil {
		return fmt.Errorf("initialize rate limiter: %w", err)
	}
	defer func() { err = errors.Join(err, limiter.Close()) }()
	upstream, err := proxy.New(cfg.Upstream, logger, proxy.WithTransport(telemetry.Transport))
	if err != nil {
		return fmt.Errorf("initialize upstream: %w", err)
	}
	defer upstream.CloseIdleConnections()
	a := &application{cfg: cfg, logger: logger, limiter: limiter, proxy: upstream, telemetry: telemetry}
	if cfg.Metrics.Enabled {
		a.metrics, err = observability.New()
		if err != nil {
			return fmt.Errorf("initialize metrics: %w", err)
		}
	}
	return a.serve(ctx)
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
	decision, err := a.limiter.Allow(c.Request().Context())
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
		return apiError(c, http.StatusTooManyRequests, "rate_limit_exceeded", "Request rate limit exceeded")
	}
	a.proxy.ServeHTTP(proxyResponseWriter{ResponseWriter: c.Response()}, c.Request())
	return nil
}

func apiError(c *echo.Context, status int, code, message string) error {
	return c.JSON(status, map[string]any{"error": map[string]string{
		"type": code, "code": code, "message": message,
	}})
}

func (a *application) serve(ctx context.Context) error {
	// Request contexts survive the termination signal so that HTTP Shutdown can drain them.
	requestCtx, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	servers := []*http.Server{{
		Addr: a.cfg.Server.Address, Handler: a.handler(),
		ReadHeaderTimeout: a.cfg.Server.ReadHeaderTimeout, ReadTimeout: a.cfg.Server.ReadTimeout,
		WriteTimeout: a.cfg.Server.WriteTimeout, IdleTimeout: a.cfg.Server.IdleTimeout,
		MaxHeaderBytes: a.cfg.Server.MaxHeaderBytes,
		BaseContext:    func(net.Listener) context.Context { return requestCtx },
		ErrorLog:       slog.NewLogLogger(a.logger.Handler(), slog.LevelError),
	}}
	if a.metrics != nil {
		mux := http.NewServeMux()
		mux.Handle(a.cfg.Metrics.Path, a.metrics.Handler())
		servers = append(servers, &http.Server{
			Addr: a.cfg.Metrics.Address, Handler: mux,
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
	a.logger.Info("shutting down HTTP listeners", "timeout", a.cfg.Server.ShutdownTimeout)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), a.cfg.Server.ShutdownTimeout)
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
