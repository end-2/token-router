// Package config loads and validates the runtime configuration.
package config

import (
	"fmt"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Config groups runtime settings by the component that consumes them.
type Config struct {
	Server    Server    `yaml:"server"`
	Upstream  Upstream  `yaml:"upstream"`
	RateLimit RateLimit `yaml:"rate_limit"`
	Metrics   Metrics   `yaml:"metrics"`
	Log       Log       `yaml:"log"`
	Telemetry Telemetry `yaml:"telemetry"`
}

// Server configures the public HTTP listener and graceful shutdown.
type Server struct {
	Address           string        `yaml:"address"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	ReadTimeout       time.Duration `yaml:"read_timeout"`
	WriteTimeout      time.Duration `yaml:"write_timeout"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`
	MaxHeaderBytes    int           `yaml:"max_header_bytes"`
}

// Upstream configures the destination and its HTTP connection pool.
type Upstream struct {
	BaseURL               string        `yaml:"base_url"`
	APIKey                string        `yaml:"api_key"`
	Organization          string        `yaml:"organization"`
	Project               string        `yaml:"project"`
	DialTimeout           time.Duration `yaml:"dial_timeout"`
	ResponseHeaderTimeout time.Duration `yaml:"response_header_timeout"`
	IdleConnTimeout       time.Duration `yaml:"idle_conn_timeout"`
	RequestTimeout        time.Duration `yaml:"request_timeout"`
	MaxIdleConns          int           `yaml:"max_idle_conns"`
	MaxIdleConnsPerHost   int           `yaml:"max_idle_conns_per_host"`
}

// RateLimit configures global request and upstream token usage limits.
type RateLimit struct {
	Enabled           bool    `yaml:"enabled"`
	Backend           string  `yaml:"backend"`
	RequestsPerSecond float64 `yaml:"requests_per_second"`
	Burst             int     `yaml:"burst"`
	TokensPerSecond   float64 `yaml:"tokens_per_second"`
	TokenBurst        int     `yaml:"token_burst"`
	Redis             Redis   `yaml:"redis"`
}

// Redis configures the shared rate limit storage.
type Redis struct {
	Address      string        `yaml:"address"`
	Username     string        `yaml:"username"`
	Password     string        `yaml:"password"`
	Key          string        `yaml:"key"`
	DB           int           `yaml:"db"`
	DialTimeout  time.Duration `yaml:"dial_timeout"`
	ReadTimeout  time.Duration `yaml:"read_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout"`
	TLS          bool          `yaml:"tls"`
}

// Metrics configures a separate Prometheus HTTP listener.
type Metrics struct {
	Enabled bool   `yaml:"enabled"`
	Address string `yaml:"address"`
	Path    string `yaml:"path"`
}

// Log configures structured application logging.
type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Telemetry configures application-scoped OpenTelemetry providers and OTLP export.
type Telemetry struct {
	Enabled           bool             `yaml:"enabled"`
	ServiceName       string           `yaml:"service_name"`
	ServiceInstanceID string           `yaml:"service_instance_id"`
	ServiceVersion    string           `yaml:"service_version"`
	Environment       string           `yaml:"environment"`
	ShutdownTimeout   time.Duration    `yaml:"shutdown_timeout"`
	OTLP              OTLP             `yaml:"otlp"`
	Traces            Traces           `yaml:"traces"`
	Metrics           TelemetryMetrics `yaml:"metrics"`
	Logs              TelemetryLogs    `yaml:"logs"`
}

// OTLP configures the shared HTTP endpoint for all enabled telemetry signals.
type OTLP struct {
	Endpoint string        `yaml:"endpoint"`
	Timeout  time.Duration `yaml:"timeout"`
}

// Traces controls trace export and parent-aware sampling.
type Traces struct {
	Enabled     bool    `yaml:"enabled"`
	SampleRatio float64 `yaml:"sample_ratio"`
}

// TelemetryMetrics configures periodic OTLP metric export independently of Prometheus.
type TelemetryMetrics struct {
	Enabled        bool          `yaml:"enabled"`
	ExportInterval time.Duration `yaml:"export_interval"`
}

// TelemetryLogs controls OTLP export of structured application logs.
type TelemetryLogs struct {
	Enabled bool `yaml:"enabled"`
}

// Defaults returns settings suitable for local development and streaming APIs.
func Defaults() Config {
	return Config{
		Server: Server{
			Address: "127.0.0.1:8080", ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout: 60 * time.Second, ShutdownTimeout: 30 * time.Second,
			MaxHeaderBytes: 1 << 20,
		},
		Upstream: Upstream{
			BaseURL: "https://api.openai.com", DialTimeout: 10 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second, IdleConnTimeout: 90 * time.Second,
			MaxIdleConns: 100, MaxIdleConnsPerHost: 100,
		},
		RateLimit: RateLimit{
			Enabled: true, Backend: "memory", RequestsPerSecond: 10, Burst: 20,
			Redis: Redis{
				Address: "127.0.0.1:6379", Key: "token-router:rate-limit:global",
				DialTimeout: 5 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second,
			},
		},
		Metrics: Metrics{Enabled: true, Address: "127.0.0.1:9090", Path: "/metrics"},
		Log:     Log{Level: "info", Format: "json"},
		Telemetry: Telemetry{
			ServiceName: "token-router", ServiceVersion: "dev", Environment: "development",
			ShutdownTimeout: 5 * time.Second,
			OTLP:            OTLP{Endpoint: "http://127.0.0.1:4318", Timeout: 5 * time.Second},
			Traces:          Traces{Enabled: true, SampleRatio: 1},
			Metrics:         TelemetryMetrics{Enabled: true, ExportInterval: 15 * time.Second},
			Logs:            TelemetryLogs{Enabled: true},
		},
	}
}

// Validate checks settings without including configured values in errors.
func (c Config) Validate() error {
	if err := validateAddress("server.address", c.Server.Address); err != nil {
		return err
	}
	for _, field := range fields(&c) {
		if value, ok := field.value.(*time.Duration); ok && *value < 0 {
			return fmt.Errorf("%s must not be negative", field.key)
		}
	}
	if c.Server.ShutdownTimeout <= 0 {
		return fmt.Errorf("server.shutdown_timeout must be positive")
	}
	if c.Server.MaxHeaderBytes <= 0 {
		return fmt.Errorf("server.max_header_bytes must be positive")
	}
	u, err := url.Parse(c.Upstream.BaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return fmt.Errorf("upstream.base_url must be an absolute HTTP or HTTPS URL without credentials, query, or fragment")
	}
	for _, field := range []struct{ name, value string }{
		{"upstream.api_key", c.Upstream.APIKey},
		{"upstream.organization", c.Upstream.Organization},
		{"upstream.project", c.Upstream.Project},
	} {
		if strings.ContainsFunc(field.value, func(r rune) bool { return r < 32 || r == 127 }) {
			return fmt.Errorf("%s must not contain control characters", field.name)
		}
	}
	if c.Upstream.MaxIdleConns <= 0 || c.Upstream.MaxIdleConnsPerHost <= 0 {
		return fmt.Errorf("upstream connection pool sizes must be positive")
	}
	if c.RateLimit.Backend != "memory" && c.RateLimit.Backend != "redis" {
		return fmt.Errorf("rate_limit.backend must be memory or redis")
	}
	if math.IsNaN(c.RateLimit.RequestsPerSecond) || math.IsInf(c.RateLimit.RequestsPerSecond, 0) || c.RateLimit.RequestsPerSecond < 0 || (c.RateLimit.Enabled && c.RateLimit.RequestsPerSecond == 0) {
		return fmt.Errorf("rate_limit.requests_per_second must be finite and positive when enabled")
	}
	if c.RateLimit.Burst < 0 || (c.RateLimit.Enabled && c.RateLimit.Burst == 0) {
		return fmt.Errorf("rate_limit.burst must be positive when enabled")
	}
	if c.RateLimit.Enabled && float64(c.RateLimit.Burst)/c.RateLimit.RequestsPerSecond >= float64(math.MaxInt64)/float64(time.Second) {
		return fmt.Errorf("rate_limit bucket refill time exceeds the supported duration")
	}
	if math.IsNaN(c.RateLimit.TokensPerSecond) || math.IsInf(c.RateLimit.TokensPerSecond, 0) || c.RateLimit.TokensPerSecond < 0 {
		return fmt.Errorf("rate_limit.tokens_per_second must be finite and nonnegative")
	}
	if c.RateLimit.TokenBurst < 0 {
		return fmt.Errorf("rate_limit.token_burst must not be negative")
	}
	if (c.RateLimit.TokensPerSecond == 0) != (c.RateLimit.TokenBurst == 0) {
		return fmt.Errorf("rate_limit.tokens_per_second and rate_limit.token_burst must both be positive or both be zero")
	}
	if c.RateLimit.Enabled && c.RateLimit.TokensPerSecond > 0 && float64(c.RateLimit.TokenBurst)/c.RateLimit.TokensPerSecond >= float64(math.MaxInt64)/float64(time.Second) {
		return fmt.Errorf("rate_limit token usage bucket refill time exceeds the supported duration")
	}
	if c.RateLimit.Redis.DB < 0 {
		return fmt.Errorf("rate_limit.redis.db must not be negative")
	}
	if c.RateLimit.Enabled && c.RateLimit.Backend == "redis" {
		if err := validateAddress("rate_limit.redis.address", c.RateLimit.Redis.Address); err != nil {
			return err
		}
		if c.RateLimit.Redis.Key == "" {
			return fmt.Errorf("rate_limit.redis.key must not be empty")
		}
		if c.RateLimit.Redis.DialTimeout <= 0 || c.RateLimit.Redis.ReadTimeout <= 0 || c.RateLimit.Redis.WriteTimeout <= 0 {
			return fmt.Errorf("rate_limit.redis timeouts must be positive when the Redis backend is enabled")
		}
	}
	if c.Metrics.Enabled {
		if err := validateAddress("metrics.address", c.Metrics.Address); err != nil {
			return err
		}
		if c.Metrics.Address == c.Server.Address && !strings.HasSuffix(c.Server.Address, ":0") {
			return fmt.Errorf("metrics.address must differ from server.address")
		}
		p, err := url.ParseRequestURI(c.Metrics.Path)
		if err != nil || !strings.HasPrefix(c.Metrics.Path, "/") || p.Path != c.Metrics.Path || p.RawQuery != "" || strings.ContainsAny(c.Metrics.Path, "?#{}* \t\r\n") {
			return fmt.Errorf("metrics.path must be an absolute URL path without query, fragment, or patterns")
		}
	}
	if c.Log.Level != "debug" && c.Log.Level != "info" && c.Log.Level != "warn" && c.Log.Level != "error" {
		return fmt.Errorf("log.level must be debug, info, warn, or error")
	}
	if c.Log.Format != "json" && c.Log.Format != "text" {
		return fmt.Errorf("log.format must be json or text")
	}
	if err := c.Telemetry.validate(); err != nil {
		return err
	}
	return nil
}

func (c Telemetry) validate() error {
	if math.IsNaN(c.Traces.SampleRatio) || math.IsInf(c.Traces.SampleRatio, 0) || c.Traces.SampleRatio < 0 || c.Traces.SampleRatio > 1 {
		return fmt.Errorf("telemetry.traces.sample_ratio must be finite and between 0 and 1")
	}
	if !c.Enabled {
		return nil
	}
	if strings.ContainsFunc(c.ServiceInstanceID, func(r rune) bool { return r < 32 || r == 127 }) {
		return fmt.Errorf("telemetry.service_instance_id must not contain control characters")
	}
	for _, field := range []struct{ name, value string }{
		{"telemetry.service_name", c.ServiceName},
		{"telemetry.service_version", c.ServiceVersion},
		{"telemetry.environment", c.Environment},
	} {
		if strings.TrimSpace(field.value) == "" || strings.ContainsFunc(field.value, func(r rune) bool { return r < 32 || r == 127 }) {
			return fmt.Errorf("%s must be nonempty and contain no control characters", field.name)
		}
	}
	u, err := url.Parse(c.OTLP.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return fmt.Errorf("telemetry.otlp.endpoint must be an absolute HTTP or HTTPS URL without credentials, query, or fragment")
	}
	if c.OTLP.Timeout <= 0 || c.ShutdownTimeout <= 0 {
		return fmt.Errorf("telemetry.otlp.timeout and telemetry.shutdown_timeout must be positive when telemetry is enabled")
	}
	if c.Metrics.Enabled && c.Metrics.ExportInterval <= 0 {
		return fmt.Errorf("telemetry.metrics.export_interval must be positive when telemetry metrics are enabled")
	}
	return nil
}

func validateAddress(name, address string) error {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%s must have host:port format", name)
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 0 || number > 65535 {
		return fmt.Errorf("%s must use a port between 0 and 65535", name)
	}
	return nil
}
