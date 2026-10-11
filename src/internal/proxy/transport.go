package proxy

import (
	"net"
	"net/http"

	"token-router/src/internal/config"
)

func newTransport(cfg config.Transport) (*http.Transport, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	keepAlive := cfg.TCPKeepAlive
	if keepAlive == 0 {
		// net.Dialer uses a negative interval to disable TCP keep-alive probes.
		keepAlive = -1
	}

	return &http.Transport{
		// Direct routing and transparent response bodies are proxy invariants.
		Proxy:                  nil,
		DisableCompression:     true,
		DialContext:            (&net.Dialer{Timeout: cfg.DialTimeout, KeepAlive: keepAlive}).DialContext,
		TLSHandshakeTimeout:    cfg.TLSHandshakeTimeout,
		ResponseHeaderTimeout:  cfg.ResponseHeaderTimeout,
		ExpectContinueTimeout:  cfg.ExpectContinueTimeout,
		IdleConnTimeout:        cfg.IdleConnTimeout,
		MaxIdleConns:           cfg.MaxIdleConns,
		MaxIdleConnsPerHost:    cfg.MaxIdleConnsPerHost,
		MaxConnsPerHost:        cfg.MaxConnsPerHost,
		ReadBufferSize:         cfg.ReadBufferSize,
		WriteBufferSize:        cfg.WriteBufferSize,
		MaxResponseHeaderBytes: cfg.MaxResponseHeaderBytes,
		DisableKeepAlives:      cfg.DisableKeepAlives,
		ForceAttemptHTTP2:      cfg.ForceAttemptHTTP2,
	}, nil
}
