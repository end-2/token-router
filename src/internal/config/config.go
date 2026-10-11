// Package config loads and validates application configuration and policies.
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

// Config groups application settings by the component that consumes them.
type Config struct {
	Server     Server    `yaml:"server"`
	Admin      Admin     `yaml:"admin"`
	Policy     Policy    `yaml:"policy"`
	Upstream   Upstream  `yaml:"upstream"`
	Transport  Transport `yaml:"transport"`
	RateLimit  RateLimit `yaml:"rate_limit"`
	Telemetry  Telemetry `yaml:"telemetry"`
	policyBase *PolicyConfig
}

// Admin configures the separate configuration API listener.
type Admin struct {
	Enabled bool   `yaml:"enabled"`
	Address string `yaml:"address"`
}

// Policy selects the optional mutable configuration file and polling interval.
type Policy struct {
	File          string        `yaml:"file"`
	WatchInterval time.Duration `yaml:"watch_interval"`
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

// Upstream configures routing, authentication, and the overall request timeout.
type Upstream struct {
	Routes              []UpstreamRoute `yaml:"routes"`
	MaxRoutingBodyBytes int             `yaml:"max_routing_body_bytes"`
	APIKey              string          `yaml:"api_key"`
	Organization        string          `yaml:"organization"`
	Project             string          `yaml:"project"`
	RequestTimeout      time.Duration   `yaml:"request_timeout"`
}

// RateLimit configures shared or per-account request and upstream token limits.
type RateLimit struct {
	Enabled           bool    `yaml:"enabled"`
	Backend           string  `yaml:"backend"`
	RequestsPerSecond float64 `yaml:"requests_per_second"`
	Burst             int     `yaml:"burst"`
	TokensPerSecond   float64 `yaml:"tokens_per_second"`
	TokenBurst        int     `yaml:"token_burst"`
	Account           Account `yaml:"account"`
	Redis             Redis   `yaml:"redis"`
	Memory            Memory  `yaml:"memory"`
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

// Prometheus configures a separate Prometheus HTTP listener.
type Prometheus struct {
	Enabled bool   `yaml:"enabled"`
	Address string `yaml:"address"`
	Path    string `yaml:"path"`
}

// Telemetry configures application logging, Prometheus, and OpenTelemetry OTLP export.
type Telemetry struct {
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

// OTLP controls export and the shared HTTP endpoint for enabled telemetry signals.
type OTLP struct {
	Enabled  bool          `yaml:"enabled"`
	Endpoint string        `yaml:"endpoint"`
	Timeout  time.Duration `yaml:"timeout"`
}

// Traces controls trace export and parent-aware sampling.
type Traces struct {
	Enabled     bool    `yaml:"enabled"`
	SampleRatio float64 `yaml:"sample_ratio"`
}

// TelemetryMetrics configures periodic OTLP metric export and a separate Prometheus listener.
type TelemetryMetrics struct {
	Enabled        bool          `yaml:"enabled"`
	ExportInterval time.Duration `yaml:"export_interval"`
	Prometheus     Prometheus    `yaml:"prometheus"`
}

// TelemetryLogs configures local application logging and optional OTLP log export.
type TelemetryLogs struct {
	Enabled bool   `yaml:"enabled"`
	Level   string `yaml:"level"`
	Format  string `yaml:"format"`
}

// Defaults returns settings suitable for local development and streaming APIs.
func Defaults() Config {
	return Config{
		Admin:  Admin{Address: "127.0.0.1:9091"},
		Policy: Policy{WatchInterval: time.Second},
		Server: Server{
			Address: "127.0.0.1:8080", ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout: 60 * time.Second, ShutdownTimeout: 30 * time.Second,
			MaxHeaderBytes: 1 << 20,
		},
		Upstream: Upstream{
			Routes:              []UpstreamRoute{{Host: "*", Model: "*", BaseURLs: []string{"https://api.openai.com"}}},
			MaxRoutingBodyBytes: 8 << 20,
		},
		Transport: defaultTransport(),
		RateLimit: RateLimit{
			Enabled: true, Backend: "memory", RequestsPerSecond: 10, Burst: 20,
			Account: Account{JWTClaim: "sub"},
			Memory:  Memory{Distributed: Distributed{Peers: []string{}, DataDir: "data/rate-limit", RequestTimeout: 5 * time.Second}},
			Redis: Redis{
				Address: "127.0.0.1:6379", Key: "token-router:rate-limit:global",
				DialTimeout: 5 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second,
			},
		},
		Telemetry: Telemetry{
			ServiceName: "token-router", ServiceVersion: "dev", Environment: "development",
			ShutdownTimeout: 5 * time.Second,
			OTLP:            OTLP{Endpoint: "http://127.0.0.1:4318", Timeout: 5 * time.Second},
			Traces:          Traces{Enabled: true, SampleRatio: 1},
			Metrics: TelemetryMetrics{
				Enabled: true, ExportInterval: 15 * time.Second,
				Prometheus: Prometheus{Enabled: true, Address: "127.0.0.1:9090", Path: "/metrics"},
			},
			Logs: TelemetryLogs{Enabled: true, Level: "info", Format: "json"},
		},
	}
}

// Validate checks settings without including configured values in errors.
func (c Config) Validate() error {
	if c.Policy.WatchInterval <= 0 {
		return fmt.Errorf("policy.watch_interval must be positive")
	}
	if c.Admin.Enabled {
		if err := validateAddress("admin.address", c.Admin.Address); err != nil {
			return err
		}
		if !strings.HasSuffix(c.Admin.Address, ":0") && (c.Admin.Address == c.Server.Address ||
			(c.Telemetry.Metrics.Enabled && c.Telemetry.Metrics.Prometheus.Enabled && c.Admin.Address == c.Telemetry.Metrics.Prometheus.Address)) {
			return fmt.Errorf("admin.address must differ from other listener addresses")
		}
	}
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
	if err := c.Upstream.ValidateRouting(); err != nil {
		return err
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
	if err := c.Transport.Validate(); err != nil {
		return err
	}
	if c.RateLimit.Backend != "memory" && c.RateLimit.Backend != "redis" {
		return fmt.Errorf("rate_limit.backend must be memory or redis")
	}
	if c.RateLimit.Account.Enabled && (strings.TrimSpace(c.RateLimit.Account.JWTClaim) == "" || len(c.RateLimit.Account.JWTClaim) > 1024) {
		return fmt.Errorf("rate_limit.account.jwt_claim must contain 1 to 1024 bytes when account limits are enabled")
	}
	if c.RateLimit.Enabled && c.RateLimit.Backend == "memory" && c.RateLimit.Memory.Distributed.Enabled {
		if err := c.RateLimit.Memory.Distributed.Validate(); err != nil {
			return err
		}
		_, adminPort, err := net.SplitHostPort(c.Admin.Address)
		_, peerPort, _ := net.SplitHostPort(c.RateLimit.Memory.Distributed.Address)
		if !c.Admin.Enabled || err != nil || adminPort != peerPort {
			return fmt.Errorf("distributed rate limiting requires admin.enabled and an admin.address port matching rate_limit.memory.distributed.address")
		}
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
	prometheus := c.Telemetry.Metrics.Prometheus
	if c.Telemetry.Metrics.Enabled && prometheus.Enabled {
		if err := validateAddress("telemetry.metrics.prometheus.address", prometheus.Address); err != nil {
			return err
		}
		if prometheus.Address == c.Server.Address && !strings.HasSuffix(c.Server.Address, ":0") {
			return fmt.Errorf("telemetry.metrics.prometheus.address must differ from server.address")
		}
		p, err := url.ParseRequestURI(prometheus.Path)
		if err != nil || !strings.HasPrefix(prometheus.Path, "/") || p.Path != prometheus.Path || p.RawQuery != "" || strings.ContainsAny(prometheus.Path, "?#{}* \t\r\n") {
			return fmt.Errorf("telemetry.metrics.prometheus.path must be an absolute URL path without query, fragment, or patterns")
		}
	}
	if c.Telemetry.Logs.Level != "debug" && c.Telemetry.Logs.Level != "info" && c.Telemetry.Logs.Level != "warn" && c.Telemetry.Logs.Level != "error" {
		return fmt.Errorf("telemetry.logs.level must be debug, info, warn, or error")
	}
	if c.Telemetry.Logs.Format != "json" && c.Telemetry.Logs.Format != "text" {
		return fmt.Errorf("telemetry.logs.format must be json or text")
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
	if !c.OTLP.Enabled {
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
		return fmt.Errorf("telemetry.otlp.timeout and telemetry.shutdown_timeout must be positive when OTLP export is enabled")
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
