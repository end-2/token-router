package config

import (
	"fmt"
	"time"
)

// Transport configures outbound HTTP connections shared by all routing destinations.
type Transport struct {
	DialTimeout            time.Duration `yaml:"dial_timeout"`
	TCPKeepAlive           time.Duration `yaml:"tcp_keep_alive"`
	TLSHandshakeTimeout    time.Duration `yaml:"tls_handshake_timeout"`
	ResponseHeaderTimeout  time.Duration `yaml:"response_header_timeout"`
	ExpectContinueTimeout  time.Duration `yaml:"expect_continue_timeout"`
	IdleConnTimeout        time.Duration `yaml:"idle_conn_timeout"`
	MaxIdleConns           int           `yaml:"max_idle_conns"`
	MaxIdleConnsPerHost    int           `yaml:"max_idle_conns_per_host"`
	MaxConnsPerHost        int           `yaml:"max_conns_per_host"`
	ReadBufferSize         int           `yaml:"read_buffer_size"`
	WriteBufferSize        int           `yaml:"write_buffer_size"`
	MaxResponseHeaderBytes int64         `yaml:"max_response_header_bytes"`
	DisableKeepAlives      bool          `yaml:"disable_keep_alives"`
	ForceAttemptHTTP2      bool          `yaml:"force_attempt_http2"`
}

func defaultTransport() Transport {
	return Transport{
		DialTimeout: 10 * time.Second, TCPKeepAlive: 30 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: time.Second, IdleConnTimeout: 90 * time.Second,
		MaxIdleConns: 100, MaxIdleConnsPerHost: 100, ForceAttemptHTTP2: true,
	}
}

// Validate rejects negative durations, connection limits, and buffer sizes.
func (c Transport) Validate() error {
	for _, setting := range transportFields(&c) {
		negative := false
		switch value := setting.value.(type) {
		case *time.Duration:
			negative = *value < 0
		case *int:
			negative = *value < 0
		case *int64:
			negative = *value < 0
		}

		if negative {
			return fmt.Errorf("%s must not be negative", setting.key)
		}
	}
	return nil
}

func transportFields(c *Transport) []field {
	return []field{
		{"transport.dial_timeout", "Upstream TCP connection timeout; 0 disables the timeout", &c.DialTimeout},
		{"transport.tcp_keep_alive", "Upstream TCP keep-alive probe interval; 0 disables probes", &c.TCPKeepAlive},
		{"transport.tls_handshake_timeout", "Upstream TLS handshake timeout; 0 disables the timeout", &c.TLSHandshakeTimeout},
		{"transport.response_header_timeout", "Wait for upstream response headers after sending the request; 0 disables the timeout", &c.ResponseHeaderTimeout},
		{"transport.expect_continue_timeout", "Wait for upstream 100-continue; 0 sends the body immediately", &c.ExpectContinueTimeout},
		{"transport.idle_conn_timeout", "Upstream idle connection timeout; 0 disables expiry", &c.IdleConnTimeout},
		{"transport.max_idle_conns", "Maximum idle upstream connections across all hosts; 0 is unlimited", &c.MaxIdleConns},
		{"transport.max_idle_conns_per_host", "Maximum idle upstream connections per host; 0 uses the Go default of 2", &c.MaxIdleConnsPerHost},
		{"transport.max_conns_per_host", "Maximum dialing, active, and idle upstream connections per host; 0 is unlimited", &c.MaxConnsPerHost},
		{"transport.read_buffer_size", "Upstream read buffer size in bytes; 0 uses the Go default", &c.ReadBufferSize},
		{"transport.write_buffer_size", "Upstream write buffer size in bytes; 0 uses the Go default", &c.WriteBufferSize},
		{"transport.max_response_header_bytes", "Maximum upstream response header bytes; 0 uses the Go default", &c.MaxResponseHeaderBytes},
		{"transport.disable_keep_alives", "Disable upstream HTTP connection reuse", &c.DisableKeepAlives},
		{"transport.force_attempt_http2", "Attempt HTTP/2 for HTTPS upstreams", &c.ForceAttemptHTTP2},
	}
}
