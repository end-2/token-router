package config

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Flag describes the CLI representation of one leaf configuration setting.
type Flag struct {
	Key     string
	Name    string
	Default string
	Usage   string
	Boolean bool
}

// Flags returns all settings using consistent YAML, environment, and CLI names.
func Flags() []Flag {
	cfg := Defaults()
	settings := fields(&cfg)
	flags := make([]Flag, 0, len(settings))
	for _, field := range settings {
		_, boolean := field.value.(*bool)
		flags = append(flags, Flag{
			Key: field.key, Name: strings.NewReplacer(".", "-", "_", "-").Replace(field.key),
			Default: field.string(), Usage: field.usage + " (" + field.envName() + ")", Boolean: boolean,
		})
	}
	return flags
}

type field struct {
	key   string
	usage string
	value any
}

func fields(c *Config) []field {
	return append([]field{
		{"admin.enabled", "Enable the separate configuration API listener", &c.Admin.Enabled},
		{"admin.address", "Admin API listen address", &c.Admin.Address},
		{"policy.file", "Policy file relative to the main configuration file", &c.Policy.File},
		{"policy.watch_interval", "Interval for checking policy file contents", &c.Policy.WatchInterval},
		{"server.address", "Proxy listen address", &c.Server.Address},
		{"server.read_header_timeout", "Maximum time to read request headers", &c.Server.ReadHeaderTimeout},
		{"server.read_timeout", "Maximum time to read a request; 0 disables the timeout", &c.Server.ReadTimeout},
		{"server.write_timeout", "Maximum response write time; 0 supports long streams", &c.Server.WriteTimeout},
		{"server.idle_timeout", "Keep-alive connection idle timeout", &c.Server.IdleTimeout},
		{"server.shutdown_timeout", "Maximum graceful shutdown time", &c.Server.ShutdownTimeout},
		{"server.max_header_bytes", "Maximum request header size in bytes", &c.Server.MaxHeaderBytes},
		{"upstream.routes", "Host and model routing rules as a JSON or YAML array", &c.Upstream.Routes},
		{"upstream.max_routing_body_bytes", "Maximum JSON request body size inspected for model routing", &c.Upstream.MaxRoutingBodyBytes},
		{"upstream.api_key", "Upstream API key; empty forwards incoming Authorization", &c.Upstream.APIKey},
		{"upstream.organization", "Upstream OpenAI organization header", &c.Upstream.Organization},
		{"upstream.project", "Upstream OpenAI project header", &c.Upstream.Project},
		{"upstream.request_timeout", "Total upstream request timeout; 0 supports long streams", &c.Upstream.RequestTimeout},
		{"rate_limit.enabled", "Enable request and upstream token usage limits", &c.RateLimit.Enabled},
		{"rate_limit.backend", "Token bucket backend: memory or redis", &c.RateLimit.Backend},
		{"rate_limit.requests_per_second", "Request token refill rate per bucket", &c.RateLimit.RequestsPerSecond},
		{"rate_limit.burst", "Request token capacity per bucket", &c.RateLimit.Burst},
		{"rate_limit.tokens_per_second", "Upstream token usage refill rate; 0 with token_burst 0 disables token usage limiting", &c.RateLimit.TokensPerSecond},
		{"rate_limit.token_burst", "Upstream token usage bucket capacity; configure with tokens_per_second", &c.RateLimit.TokenBurst},
		{"rate_limit.account.enabled", "Limit each Authorization account independently", &c.RateLimit.Account.Enabled},
		{"rate_limit.account.jwt_claim", "Top-level JWT string claim used as the account identifier", &c.RateLimit.Account.JWTClaim},
		{"rate_limit.redis.address", "Redis host and port", &c.RateLimit.Redis.Address},
		{"rate_limit.redis.username", "Redis ACL username", &c.RateLimit.Redis.Username},
		{"rate_limit.redis.password", "Redis password", &c.RateLimit.Redis.Password},
		{"rate_limit.redis.key", "Redis key shared by rate limit instances", &c.RateLimit.Redis.Key},
		{"rate_limit.redis.db", "Redis logical database index", &c.RateLimit.Redis.DB},
		{"rate_limit.redis.dial_timeout", "Redis connection dial timeout", &c.RateLimit.Redis.DialTimeout},
		{"rate_limit.redis.read_timeout", "Redis command read timeout", &c.RateLimit.Redis.ReadTimeout},
		{"rate_limit.redis.write_timeout", "Redis command write timeout", &c.RateLimit.Redis.WriteTimeout},
		{"rate_limit.redis.tls", "Enable TLS for Redis", &c.RateLimit.Redis.TLS},
		{"rate_limit.memory.distributed.enabled", "Share memory buckets through Raft on the admin port", &c.RateLimit.Memory.Distributed.Enabled},
		{"rate_limit.memory.distributed.address", "This member's advertised IP:admin-port", &c.RateLimit.Memory.Distributed.Address},
		{"rate_limit.memory.distributed.peers", "All cluster IP:admin-port addresses as a JSON or YAML array, including this member", &c.RateLimit.Memory.Distributed.Peers},
		{"rate_limit.memory.distributed.data_dir", "Persistent Raft directory relative to the main configuration file", &c.RateLimit.Memory.Distributed.DataDir},
		{"rate_limit.memory.distributed.token", "Shared secret for internal cluster requests", &c.RateLimit.Memory.Distributed.Token},
		{"rate_limit.memory.distributed.request_timeout", "Maximum time for a distributed limiter operation", &c.RateLimit.Memory.Distributed.RequestTimeout},
		{"telemetry.service_name", "OpenTelemetry service name", &c.Telemetry.ServiceName},
		{"telemetry.service_instance_id", "OpenTelemetry service instance ID; empty generates a unique ID", &c.Telemetry.ServiceInstanceID},
		{"telemetry.service_version", "OpenTelemetry service version", &c.Telemetry.ServiceVersion},
		{"telemetry.environment", "OpenTelemetry deployment environment", &c.Telemetry.Environment},
		{"telemetry.shutdown_timeout", "Maximum telemetry flush and shutdown time after HTTP draining", &c.Telemetry.ShutdownTimeout},
		{"telemetry.otlp.enabled", "Enable OTLP export independently of local metrics and logs", &c.Telemetry.OTLP.Enabled},
		{"telemetry.otlp.endpoint", "OTLP HTTP base URL; signal paths are appended", &c.Telemetry.OTLP.Endpoint},
		{"telemetry.otlp.timeout", "Timeout for each OTLP export", &c.Telemetry.OTLP.Timeout},
		{"telemetry.traces.enabled", "Enable OTLP trace export", &c.Telemetry.Traces.Enabled},
		{"telemetry.traces.sample_ratio", "Root trace sampling ratio between 0 and 1", &c.Telemetry.Traces.SampleRatio},
		{"telemetry.metrics.enabled", "Enable metrics for Prometheus and optional OTLP export", &c.Telemetry.Metrics.Enabled},
		{"telemetry.metrics.export_interval", "Interval between OTLP metric exports", &c.Telemetry.Metrics.ExportInterval},
		{"telemetry.metrics.prometheus.enabled", "Enable the separate Prometheus listener independently of OTLP export", &c.Telemetry.Metrics.Prometheus.Enabled},
		{"telemetry.metrics.prometheus.address", "Prometheus listen address", &c.Telemetry.Metrics.Prometheus.Address},
		{"telemetry.metrics.prometheus.path", "Prometheus scrape path", &c.Telemetry.Metrics.Prometheus.Path},
		{"telemetry.logs.enabled", "Enable local application logs and optional OTLP log export", &c.Telemetry.Logs.Enabled},
		{"telemetry.logs.level", "Log level: debug, info, warn, or error", &c.Telemetry.Logs.Level},
		{"telemetry.logs.format", "Local log format: json or text", &c.Telemetry.Logs.Format},
	}, transportFields(&c.Transport)...)
}

func (f field) envName() string {
	return "TOKEN_ROUTER_" + strings.ToUpper(strings.ReplaceAll(f.key, ".", "_"))
}

func (f field) string() string {
	switch value := f.value.(type) {
	case *string:
		return *value
	case *bool:
		return strconv.FormatBool(*value)
	case *int:
		return strconv.Itoa(*value)
	case *int64:
		return strconv.FormatInt(*value, 10)
	case *float64:
		return strconv.FormatFloat(*value, 'g', -1, 64)
	case *time.Duration:
		return value.String()
	case *[]string, *[]UpstreamRoute:
		encoded, err := json.Marshal(value)
		if err != nil {
			return ""
		}
		return string(encoded)
	default:
		return ""
	}
}

func (f field) set(raw string) error {
	var err error
	switch value := f.value.(type) {
	case *string:
		*value = raw
	case *bool:
		*value, err = strconv.ParseBool(raw)
	case *int:
		*value, err = strconv.Atoi(raw)
	case *int64:
		*value, err = strconv.ParseInt(raw, 10, 64)
	case *float64:
		*value, err = strconv.ParseFloat(raw, 64)
	case *time.Duration:
		*value, err = time.ParseDuration(raw)
	case *[]string, *[]UpstreamRoute:
		err = decodeYAMLValue([]byte(raw), value, yaml.SequenceNode)
	default:
		return fmt.Errorf("unsupported configuration field type")
	}
	if err != nil {
		return fmt.Errorf("invalid value for %s", f.key)
	}
	return nil
}
