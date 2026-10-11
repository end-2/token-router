package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"token-router/src/internal/config"
)

func TestServeCommands(t *testing.T) {
	for _, command := range []string{"root", "serve"} {
		t.Run(command, func(t *testing.T) {
			isolatedEnvironment(t)
			if err := os.WriteFile("config.yaml", []byte("server:\n  address: ':8101'\ntelemetry:\n  metrics:\n    prometheus:\n      enabled: true\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TOKEN_ROUTER_SERVER_ADDRESS", ":8102")
			t.Setenv("TOKEN_ROUTER_UPSTREAM_API_KEY", "environment-secret")

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			called := false
			root := NewCommand(func(gotContext context.Context, cfg config.Config) error {
				called = true
				if gotContext != ctx {
					t.Error("command did not propagate its context")
				}
				if cfg.Server.Address != ":8103" || cfg.Telemetry.Metrics.Prometheus.Enabled || cfg.Server.WriteTimeout != 0 || cfg.Upstream.APIKey != "" {
					t.Error("flags did not override YAML and environment settings")
				}
				return nil
			})

			args := []string{"--server-address=:8103", "--telemetry-metrics-prometheus-enabled=false", "--server-write-timeout=0s", "--upstream-api-key="}
			if command == "serve" {
				args = append([]string{"serve"}, args...)
			}
			root.SetArgs(args)
			if err := root.ExecuteContext(ctx); err != nil {
				t.Fatal(err)
			}

			if !called {
				t.Error("server callback was not called")
			}
		})
	}
}

func TestUnchangedFlagsDoNotOverrideEnvironment(t *testing.T) {
	isolatedEnvironment(t)
	t.Setenv("TOKEN_ROUTER_SERVER_ADDRESS", ":8123")
	t.Setenv("TOKEN_ROUTER_TELEMETRY_METRICS_PROMETHEUS_ENABLED", "false")

	root := NewCommand(func(_ context.Context, cfg config.Config) error {
		if cfg.Server.Address != ":8123" || cfg.Telemetry.Metrics.Prometheus.Enabled {
			t.Error("unchanged flags overrode environment")
		}
		return nil
	})

	root.SetArgs(nil)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestBooleanFlagWithoutValue(t *testing.T) {
	isolatedEnvironment(t)
	t.Setenv("TOKEN_ROUTER_TELEMETRY_METRICS_PROMETHEUS_ENABLED", "false")

	root := NewCommand(func(_ context.Context, cfg config.Config) error {
		if !cfg.Telemetry.Metrics.Prometheus.Enabled {
			t.Error("boolean flag without value did not enable metrics")
		}
		return nil
	})

	root.SetArgs([]string{"--telemetry-metrics-prometheus-enabled"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateCommand(t *testing.T) {
	isolatedEnvironment(t)
	var output bytes.Buffer
	root := NewCommand(func(context.Context, config.Config) error {
		t.Fatal("validate started the server")
		return nil
	})
	root.SetOut(&output)

	root.SetArgs([]string{"validate"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}

	if output.String() != "configuration is valid\n" {
		t.Errorf("unexpected validation output %q", output.String())
	}
}

func TestTransportFlags(t *testing.T) {
	isolatedEnvironment(t)
	t.Setenv("TOKEN_ROUTER_TRANSPORT_MAX_CONNS_PER_HOST", "20")

	root := NewCommand(func(_ context.Context, cfg config.Config) error {
		if cfg.Transport.MaxConnsPerHost != 30 || cfg.Transport.ForceAttemptHTTP2 ||
			!cfg.Transport.DisableKeepAlives || cfg.Transport.MaxResponseHeaderBytes != 1<<33 {
			t.Error("transport flags did not apply typed overrides")
		}
		return nil
	})

	root.SetArgs([]string{"serve", "--transport-max-conns-per-host=30", "--transport-force-attempt-http2=false",
		"--transport-disable-keep-alives", "--transport-max-response-header-bytes=8589934592"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigFlagTakesPriorityOverEnvironment(t *testing.T) {
	isolatedEnvironment(t)
	path := filepath.Join(t.TempDir(), "selected.yaml")
	if err := os.WriteFile(path, []byte("server:\n  address: ':8123'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOKEN_ROUTER_CONFIG", "missing-environment.yaml")

	root := NewCommand(func(_ context.Context, cfg config.Config) error {
		if cfg.Server.Address != ":8123" {
			t.Error("explicit config path was not selected")
		}
		return nil
	})

	root.SetArgs([]string{"--config", path, "serve"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestErrorsDoNotPrintConfigurationValues(t *testing.T) {
	tests := [][]string{
		{"--server-shutdown-timeout=secret-value"},
		{"--telemetry-metrics-prometheus-enabled=secret-value"},
		{"--rate-limit-burst=secret-value"},
		{"--unknown=secret-value"},
		{"--config=secret-value"},
	}
	for _, args := range tests {
		t.Run(args[0], func(t *testing.T) {
			isolatedEnvironment(t)
			var output bytes.Buffer
			root := NewCommand(func(context.Context, config.Config) error {
				t.Fatal("invalid settings started the server")
				return nil
			})
			root.SetOut(&output)
			root.SetErr(&output)

			root.SetArgs(args)
			err := root.Execute()

			if err == nil {
				t.Fatal("expected error")
			}
			if strings.Contains(err.Error()+output.String(), "secret-value") {
				t.Error("CLI error disclosed an input value")
			}
		})
	}
}

func TestServerErrorsAreReturned(t *testing.T) {
	isolatedEnvironment(t)
	want := errors.New("server stopped")
	root := NewCommand(func(context.Context, config.Config) error { return want })

	root.SetArgs(nil)
	if err := root.Execute(); !errors.Is(err, want) {
		t.Errorf("Execute returned %v, want server error", err)
	}
}

func isolatedEnvironment(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "TOKEN_ROUTER_") {
			t.Setenv(name, "")
			if err := os.Unsetenv(name); err != nil {
				t.Fatal(err)
			}
		}
	}
}
