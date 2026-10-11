package config

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTelemetryConfigurationLayers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, path, `telemetry:
  service_name: file-service
  otlp:
    enabled: true
    endpoint: http://collector:4318/gateway
  traces:
    sample_ratio: 0.25
  logs:
    enabled: false
`)
	cfg, err := Load(Options{
		Path: path, PathExplicit: true,
		LookupEnv: lookup(map[string]string{
			"TOKEN_ROUTER_TELEMETRY_SERVICE_NAME":            "env-service",
			"TOKEN_ROUTER_TELEMETRY_TRACES_SAMPLE_RATIO":     "0.5",
			"TOKEN_ROUTER_TELEMETRY_METRICS_EXPORT_INTERVAL": "2s",
		}),
		Overrides: map[string]string{
			"telemetry.service_name":        "cli-service",
			"telemetry.traces.sample_ratio": "0",
			"telemetry.metrics.enabled":     "false",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Telemetry.OTLP.Enabled || cfg.Telemetry.ServiceName != "cli-service" || cfg.Telemetry.OTLP.Endpoint != "http://collector:4318/gateway" {
		t.Fatal("telemetry configuration did not follow layer precedence")
	}
	if cfg.Telemetry.Traces.SampleRatio != 0 || cfg.Telemetry.Metrics.Enabled || cfg.Telemetry.Logs.Enabled || cfg.Telemetry.Metrics.ExportInterval != 2*time.Second {
		t.Fatal("telemetry signal options did not preserve explicit values")
	}
	if cfg.Telemetry.OTLP.Timeout != Defaults().Telemetry.OTLP.Timeout || !cfg.Telemetry.Metrics.Prometheus.Enabled {
		t.Fatal("partial telemetry settings changed unrelated defaults")
	}
}

func TestLocalTelemetryConfigurationLayers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		layer      int
		prometheus Prometheus
		logs       TelemetryLogs
	}{
		{"defaults", 0, Prometheus{Enabled: true, Address: "127.0.0.1:9090", Path: "/metrics"}, TelemetryLogs{Enabled: true, Level: "info", Format: "json"}},
		{"YAML", 1, Prometheus{Enabled: false, Address: "127.0.0.1:9191", Path: "/file"}, TelemetryLogs{Enabled: true, Level: "warn", Format: "text"}},
		{"environment", 2, Prometheus{Enabled: true, Address: "127.0.0.1:9192", Path: "/env"}, TelemetryLogs{Enabled: false, Level: "debug", Format: "json"}},
		{"CLI", 3, Prometheus{Enabled: false, Address: "127.0.0.1:9193", Path: "/cli"}, TelemetryLogs{Enabled: true, Level: "error", Format: "text"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			options := Options{Path: path, LookupEnv: lookup(nil)}
			if tc.layer >= 1 {
				writeConfig(t, path, `telemetry:
  metrics:
    prometheus:
      enabled: false
      address: 127.0.0.1:9191
      path: /file
  logs:
    enabled: true
    level: warn
    format: text
`)
			}
			if tc.layer >= 2 {
				options.LookupEnv = lookup(map[string]string{
					"TOKEN_ROUTER_TELEMETRY_METRICS_PROMETHEUS_ENABLED": "true",
					"TOKEN_ROUTER_TELEMETRY_METRICS_PROMETHEUS_ADDRESS": "127.0.0.1:9192",
					"TOKEN_ROUTER_TELEMETRY_METRICS_PROMETHEUS_PATH":    "/env",
					"TOKEN_ROUTER_TELEMETRY_LOGS_ENABLED":               "false",
					"TOKEN_ROUTER_TELEMETRY_LOGS_LEVEL":                 "debug",
					"TOKEN_ROUTER_TELEMETRY_LOGS_FORMAT":                "json",
				})
			}
			if tc.layer >= 3 {
				options.Overrides = map[string]string{
					"telemetry.metrics.prometheus.enabled": "false",
					"telemetry.metrics.prometheus.address": "127.0.0.1:9193",
					"telemetry.metrics.prometheus.path":    "/cli",
					"telemetry.logs.enabled":               "true",
					"telemetry.logs.level":                 "error",
					"telemetry.logs.format":                "text",
				}
			}
			cfg, err := Load(options)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Telemetry.OTLP.Enabled || !cfg.Telemetry.Metrics.Enabled {
				t.Fatal("OTLP must default to disabled while metrics default to enabled")
			}
			if cfg.Telemetry.Metrics.Prometheus != tc.prometheus || cfg.Telemetry.Logs != tc.logs {
				t.Fatalf("prometheus=%+v logs=%+v, want %+v %+v", cfg.Telemetry.Metrics.Prometheus, cfg.Telemetry.Logs, tc.prometheus, tc.logs)
			}
		})
	}
}

func TestTelemetryValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Telemetry)
	}{
		{"empty service", func(c *Telemetry) { c.ServiceName = " " }},
		{"invalid resource", func(c *Telemetry) { c.Environment = "secret-value\n" }},
		{"scheme", func(c *Telemetry) { c.OTLP.Endpoint = "grpc://collector:4317" }},
		{"relative endpoint", func(c *Telemetry) { c.OTLP.Endpoint = "collector:4318" }},
		{"credentials", func(c *Telemetry) { c.OTLP.Endpoint = "https://secret-value@collector:4318" }},
		{"query", func(c *Telemetry) { c.OTLP.Endpoint = "https://collector:4318?token=secret-value" }},
		{"fragment", func(c *Telemetry) { c.OTLP.Endpoint = "https://collector:4318#secret-value" }},
		{"export timeout", func(c *Telemetry) { c.OTLP.Timeout = 0 }},
		{"shutdown timeout", func(c *Telemetry) { c.ShutdownTimeout = 0 }},
		{"metric interval", func(c *Telemetry) { c.Metrics.ExportInterval = 0 }},
		{"negative ratio", func(c *Telemetry) { c.Traces.SampleRatio = -0.1 }},
		{"large ratio", func(c *Telemetry) { c.Traces.SampleRatio = 1.1 }},
		{"infinite ratio", func(c *Telemetry) { c.Traces.SampleRatio = math.Inf(1) }},
		{"nan ratio", func(c *Telemetry) { c.Traces.SampleRatio = math.NaN() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Telemetry.OTLP.Enabled = true
			test.change(&cfg.Telemetry)
			err := cfg.Validate()
			if err == nil || strings.Contains(err.Error(), "secret-value") {
				t.Fatalf("expected a sanitized telemetry validation error, got %v", err)
			}
		})
	}
	for _, ratio := range []float64{0, 0.5, 1} {
		cfg := Defaults()
		cfg.Telemetry.OTLP.Enabled = true
		cfg.Telemetry.Traces.SampleRatio = ratio
		if err := cfg.Validate(); err != nil {
			t.Fatalf("valid sample ratio %g rejected: %v", ratio, err)
		}
	}
}
