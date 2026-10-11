// Package proxy routes HTTP requests and streaming responses to configured upstream pools.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"token-router/src/internal/config"
	"token-router/src/internal/proxy/routing"
)

// Handler owns the upstream connection pool and selects configured routing destinations.
type Handler struct {
	proxy          *httputil.ReverseProxy
	transport      *http.Transport
	logger         *slog.Logger
	requestTimeout time.Duration
	router         *routing.Router
}

// Option customizes proxy construction without changing connection pool ownership.
type Option func(*httputil.ReverseProxy)

// WithTransport decorates the upstream transport, for example with tracing.
func WithTransport(wrap func(http.RoundTripper) http.RoundTripper) Option {
	return func(proxy *httputil.ReverseProxy) {
		proxy.Transport = wrap(proxy.Transport)
	}
}

// New constructs a handler without opening upstream connections.
func New(cfg config.Upstream, transportConfig config.Transport, logger *slog.Logger, options ...Option) (*Handler, error) {
	routing, err := routing.New(cfg)
	if err != nil {
		return nil, err
	}

	if logger == nil {
		logger = slog.Default()
	}

	transport, err := newTransport(transportConfig)
	if err != nil {
		return nil, err
	}

	handler := &Handler{transport: transport, logger: logger, requestTimeout: cfg.RequestTimeout, router: routing}
	authorization := "Bearer " + cfg.APIKey
	handler.proxy = &httputil.ReverseProxy{
		Transport:  transport,
		BufferPool: responseBuffers,
		ErrorLog:   log.New(safeErrorWriter{logger: logger}, "", 0),
		Rewrite: func(request *httputil.ProxyRequest) {
			target := request.In.Context().Value(upstreamTargetKey{}).(*url.URL)
			request.SetURL(target)
			request.Out.URL.User = nil
			request.Out.URL.Opaque = ""
			request.Out.URL.Fragment = ""
			request.Out.RequestURI = ""

			for key := range request.Out.Header {
				if strings.EqualFold(key, "Forwarded") || (len(key) >= len("X-Forwarded-") && strings.EqualFold(key[:len("X-Forwarded-")], "X-Forwarded-")) {
					delete(request.Out.Header, key)
				}
			}

			if cfg.APIKey != "" {
				request.Out.Header.Set("Authorization", authorization)
			}
			if cfg.Organization != "" {
				request.Out.Header.Set("OpenAI-Organization", cfg.Organization)
			}
			if cfg.Project != "" {
				request.Out.Header.Set("OpenAI-Project", cfg.Project)
			}
		},
		ModifyResponse: func(response *http.Response) error {
			if response.StatusCode == http.StatusSwitchingProtocols {
				return errors.New("upstream protocol upgrade is unsupported")
			}
			return nil
		},
		ErrorHandler: handler.handleError,
	}

	for _, option := range options {
		option(handler.proxy)
	}
	return handler, nil
}

const responseBufferSize = 32 << 10

var responseBuffers = &responseBufferPool{}

// Array pointers avoid allocating a slice header when returning buffers to the pool.
type responseBufferPool struct {
	pool sync.Pool
}

func (pool *responseBufferPool) Get() []byte {
	if buffer := pool.pool.Get(); buffer != nil {
		return buffer.(*[responseBufferSize]byte)[:]
	}
	return make([]byte, responseBufferSize)
}

func (pool *responseBufferPool) Put(buffer []byte) {
	if cap(buffer) == responseBufferSize {
		pool.pool.Put((*[responseBufferSize]byte)(buffer[:responseBufferSize]))
	}
}

// ServeHTTP forwards an HTTP request and propagates cancellation to the upstream.
func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Upgrade") != "" || hasHeaderToken(request.Header.Values("Connection"), "upgrade") {
		handler.writeError(writer, http.StatusBadRequest, "unsupported_upgrade", "Protocol upgrades are not supported.")
		return
	}

	// Keep response flushes from closing the body while the upstream transport still reads it.
	if err := http.NewResponseController(writer).EnableFullDuplex(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		handler.logger.Error("full duplex HTTP could not be enabled")
		handler.writeError(writer, http.StatusInternalServerError, "proxy_error", "The proxy could not prepare the request.")
		return
	}

	if handler.requestTimeout > 0 {
		ctx, cancel := context.WithTimeout(request.Context(), handler.requestTimeout)
		defer cancel()
		request = request.WithContext(ctx)
	}

	target, routeErr := handler.router.SelectTarget(writer, request)
	if routeErr != nil {
		if err := request.Context().Err(); err != nil {
			handler.handleError(writer, request, err)
			return
		}
		handler.writeError(writer, routeErr.Status, routeErr.Code, routeErr.Message)
		return
	}

	request = request.WithContext(context.WithValue(request.Context(), upstreamTargetKey{}, target))
	handler.proxy.ServeHTTP(writer, request)
}

type upstreamTargetKey struct{}

// CloseIdleConnections releases pooled connections after active requests finish.
func (handler *Handler) CloseIdleConnections() {
	handler.transport.CloseIdleConnections()
}

func (handler *Handler) handleError(writer http.ResponseWriter, request *http.Request, err error) {
	if errors.Is(request.Context().Err(), context.Canceled) {
		return
	}

	status := http.StatusBadGateway
	code := "upstream_error"
	message := "The upstream request failed."
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		status = http.StatusGatewayTimeout
		code = "upstream_timeout"
		message = "The upstream request timed out."
	}

	handler.logger.WarnContext(request.Context(), "upstream request failed", "status", status, "code", code)
	handler.writeError(writer, status, code, message)
}

func (handler *Handler) writeError(writer http.ResponseWriter, status int, code, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)

	response := struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}{}
	response.Error.Message = message
	response.Error.Type = "proxy_error"
	response.Error.Code = code

	if err := json.NewEncoder(writer).Encode(response); err != nil {
		handler.logger.Debug("proxy error response could not be written")
	}
}

func hasHeaderToken(values []string, token string) bool {
	for _, value := range values {
		for part := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// ReverseProxy errors can include URLs, so its diagnostic text is not forwarded to logs.
type safeErrorWriter struct {
	logger *slog.Logger
}

func (writer safeErrorWriter) Write(message []byte) (int, error) {
	writer.logger.Debug("upstream response transfer interrupted")
	return len(message), nil
}
