// Package proxy forwards HTTP requests and streaming responses to one configured upstream.
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
	"time"

	"token-router/src/internal/config"
)

// Handler owns the upstream connection pool and forwards requests to its fixed destination.
type Handler struct {
	proxy          *httputil.ReverseProxy
	transport      *http.Transport
	logger         *slog.Logger
	requestTimeout time.Duration
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
func New(cfg config.Upstream, logger *slog.Logger, options ...Option) (*Handler, error) {
	target, err := url.Parse(cfg.BaseURL)
	if err != nil || target.Hostname() == "" || (target.Scheme != "http" && target.Scheme != "https") ||
		target.User != nil || target.RawQuery != "" || target.ForceQuery || target.Fragment != "" || target.Opaque != "" {
		return nil, errors.New("upstream base URL must be an HTTP or HTTPS URL without credentials, query, or fragment")
	}
	if logger == nil {
		logger = slog.Default()
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = (&net.Dialer{Timeout: cfg.DialTimeout, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = cfg.DialTimeout
	transport.ResponseHeaderTimeout = cfg.ResponseHeaderTimeout
	transport.IdleConnTimeout = cfg.IdleConnTimeout
	transport.MaxIdleConns = cfg.MaxIdleConns
	transport.MaxIdleConnsPerHost = cfg.MaxIdleConnsPerHost
	transport.DisableCompression = true

	handler := &Handler{transport: transport, logger: logger, requestTimeout: cfg.RequestTimeout}
	handler.proxy = &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		ErrorLog:      log.New(safeErrorWriter{logger: logger}, "", 0),
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(target)
			request.Out.URL.User = nil
			request.Out.URL.Opaque = ""
			request.Out.URL.Fragment = ""
			request.Out.RequestURI = ""
			for key := range request.Out.Header {
				if strings.EqualFold(key, "Forwarded") || strings.HasPrefix(strings.ToLower(key), "x-forwarded-") {
					delete(request.Out.Header, key)
				}
			}
			if cfg.APIKey != "" {
				request.Out.Header.Set("Authorization", "Bearer "+cfg.APIKey)
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
	handler.proxy.ServeHTTP(writer, request)
}

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
