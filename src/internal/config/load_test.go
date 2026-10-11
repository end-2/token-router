package config

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func TestLoadPrecedence(t *testing.T) {
	tests := []struct {
		name      string
		yaml      string
		env       map[string]string
		overrides map[string]string
		want      string
	}{
		{name: "defaults", want: "127.0.0.1:8080"},
		{name: "file", yaml: "server:\n  address: ':8101'\n", want: ":8101"},
		{name: "environment over file", yaml: "server:\n  address: ':8101'\n", env: map[string]string{"TOKEN_ROUTER_SERVER_ADDRESS": ":8102"}, want: ":8102"},
		{name: "flag over environment", yaml: "server:\n  address: ':8101'\n", env: map[string]string{"TOKEN_ROUTER_SERVER_ADDRESS": ":8102"}, overrides: map[string]string{"server.address": ":8103"}, want: ":8103"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if test.yaml != "" {
				writeConfig(t, path, test.yaml)
			}
			cfg, err := Load(Options{Path: path, LookupEnv: lookup(test.env), Overrides: test.overrides})
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Server.Address != test.want {
				t.Errorf("server.address = %q, want %q", cfg.Server.Address, test.want)
			}
			if cfg.Server.ShutdownTimeout != Defaults().Server.ShutdownTimeout {
				t.Error("partial configuration erased a default")
			}
		})
	}
}

func TestLoadExplicitZeroValues(t *testing.T) {
	for _, layer := range []string{"yaml", "environment", "flags"} {
		t.Run(layer, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			body := "server:\n  write_timeout: 10s\nupstream:\n  api_key: configured-key\n"
			options := Options{Path: path, LookupEnv: lookup(nil)}
			switch layer {
			case "yaml":
				body = "metrics:\n  enabled: false\nrate_limit:\n  enabled: false\n  requests_per_second: 0\n  burst: 0\nserver:\n  write_timeout: 0s\nupstream:\n  api_key: ''\n"
			case "environment":
				options.LookupEnv = lookup(map[string]string{
					"TOKEN_ROUTER_METRICS_ENABLED": "false", "TOKEN_ROUTER_RATE_LIMIT_ENABLED": "false",
					"TOKEN_ROUTER_RATE_LIMIT_REQUESTS_PER_SECOND": "0", "TOKEN_ROUTER_RATE_LIMIT_BURST": "0",
					"TOKEN_ROUTER_SERVER_WRITE_TIMEOUT": "0s", "TOKEN_ROUTER_UPSTREAM_API_KEY": "",
				})
			case "flags":
				options.Overrides = map[string]string{
					"metrics.enabled": "false", "rate_limit.enabled": "false", "rate_limit.requests_per_second": "0",
					"rate_limit.burst": "0", "server.write_timeout": "0s", "upstream.api_key": "",
				}
			}
			writeConfig(t, path, body)
			cfg, err := Load(options)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Metrics.Enabled || cfg.RateLimit.Enabled || cfg.RateLimit.RequestsPerSecond != 0 || cfg.RateLimit.Burst != 0 || cfg.Server.WriteTimeout != 0 || cfg.Upstream.APIKey != "" {
				t.Error("explicit false, zero, or empty value was overwritten")
			}
		})
	}
}

func TestLoadFileSelection(t *testing.T) {
	directory := t.TempDir()
	first := filepath.Join(directory, "first.yaml")
	second := filepath.Join(directory, "second.yaml")
	missing := filepath.Join(directory, "missing.yaml")
	writeConfig(t, first, "server:\n  address: ':8101'\n")
	writeConfig(t, second, "server:\n  address: ':8102'\n")
	tests := []struct {
		name    string
		options Options
		want    string
		wantErr bool
	}{
		{name: "optional missing file", options: Options{Path: missing}, want: "127.0.0.1:8080"},
		{name: "explicit missing file", options: Options{Path: missing, PathExplicit: true}, wantErr: true},
		{name: "explicit empty path", options: Options{PathExplicit: true}, wantErr: true},
		{name: "environment missing file", options: Options{Path: first, LookupEnv: lookup(map[string]string{"TOKEN_ROUTER_CONFIG": missing})}, wantErr: true},
		{name: "environment empty path", options: Options{Path: first, LookupEnv: lookup(map[string]string{"TOKEN_ROUTER_CONFIG": ""})}, wantErr: true},
		{name: "environment path", options: Options{Path: first, LookupEnv: lookup(map[string]string{"TOKEN_ROUTER_CONFIG": second})}, want: ":8102"},
		{name: "flag path over environment", options: Options{Path: first, PathExplicit: true, LookupEnv: lookup(map[string]string{"TOKEN_ROUTER_CONFIG": second})}, want: ":8101"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.options.LookupEnv == nil {
				test.options.LookupEnv = lookup(nil)
			}
			cfg, err := Load(test.options)
			if (err != nil) != test.wantErr {
				t.Fatalf("Load error = %v, want error %v", err, test.wantErr)
			}
			if !test.wantErr && cfg.Server.Address != test.want {
				t.Errorf("server.address = %q, want %q", cfg.Server.Address, test.want)
			}
		})
	}
}

func TestRejectInvalidYAML(t *testing.T) {
	tests := []struct{ name, body string }{
		{"empty", ""}, {"malformed", "server: ["}, {"scalar", "secret-value"},
		{"root null", "null"}, {"nested null", "server: null"}, {"leaf null", "upstream:\n  api_key: null"},
		{"unknown field", "server:\n  adress: ':8080'"}, {"unknown section", "unknown: {}"},
		{"duplicate field", "log:\n  level: info\n  level: error"},
		{"second document", "server: {}\n---\nmetrics: {}"},
		{"empty second document", "server: {}\n---\n"},
		{"invalid duration", "server:\n  shutdown_timeout: secret-value"},
		{"duration without units", "server:\n  shutdown_timeout: 30"},
		{"invalid boolean", "metrics:\n  enabled: secret-value"},
		{"secret with wrong type", "upstream:\n  api_key: [secret-value]"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			writeConfig(t, path, test.body)
			_, err := Load(Options{Path: path, PathExplicit: true, LookupEnv: lookup(nil)})
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), "secret-value") {
				t.Error("configuration error leaked an input value")
			}
		})
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Config)
	}{
		{"server address", func(c *Config) { c.Server.Address = "missing-port" }},
		{"server port", func(c *Config) { c.Server.Address = ":65536" }},
		{"negative timeout", func(c *Config) { c.Server.ReadTimeout = -time.Second }},
		{"shutdown timeout", func(c *Config) { c.Server.ShutdownTimeout = 0 }},
		{"header size", func(c *Config) { c.Server.MaxHeaderBytes = 0 }},
		{"upstream scheme", func(c *Config) { c.Upstream.BaseURL = "ftp://example.com" }},
		{"upstream host", func(c *Config) { c.Upstream.BaseURL = "https:///v1" }},
		{"upstream userinfo", func(c *Config) { c.Upstream.BaseURL = "https://secret-value@example.com" }},
		{"upstream query", func(c *Config) { c.Upstream.BaseURL = "https://example.com?api_key=secret-value" }},
		{"upstream fragment", func(c *Config) { c.Upstream.BaseURL = "https://example.com#secret-value" }},
		{"header injection", func(c *Config) { c.Upstream.APIKey = "secret-value\r\n" }},
		{"pool size", func(c *Config) { c.Upstream.MaxIdleConns = 0 }},
		{"backend", func(c *Config) { c.RateLimit.Backend = "secret-value" }},
		{"zero rate", func(c *Config) { c.RateLimit.RequestsPerSecond = 0 }},
		{"infinite rate", func(c *Config) { c.RateLimit.RequestsPerSecond = math.Inf(1) }},
		{"nan rate", func(c *Config) { c.RateLimit.RequestsPerSecond = math.NaN() }},
		{"unrepresentable refill duration", func(c *Config) { c.RateLimit.RequestsPerSecond = 1e-100 }},
		{"zero burst", func(c *Config) { c.RateLimit.Burst = 0 }},
		{"redis address", func(c *Config) { c.RateLimit.Backend = "redis"; c.RateLimit.Redis.Address = "invalid" }},
		{"redis key", func(c *Config) { c.RateLimit.Backend = "redis"; c.RateLimit.Redis.Key = "" }},
		{"redis db", func(c *Config) { c.RateLimit.Redis.DB = -1 }},
		{"metrics address", func(c *Config) { c.Metrics.Address = "invalid" }},
		{"shared address", func(c *Config) { c.Metrics.Address = c.Server.Address }},
		{"metrics path", func(c *Config) { c.Metrics.Path = "metrics" }},
		{"metrics query", func(c *Config) { c.Metrics.Path = "/metrics?secret-value" }},
		{"metrics pattern", func(c *Config) { c.Metrics.Path = "/metrics/{rest...}" }},
		{"log level", func(c *Config) { c.Log.Level = "secret-value" }},
		{"log format", func(c *Config) { c.Log.Format = "secret-value" }},
	}
	if err := Defaults().Validate(); err != nil {
		t.Fatalf("defaults are invalid: %v", err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := Defaults()
			test.change(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected validation error")
			}
			if strings.Contains(err.Error(), "secret-value") {
				t.Error("validation error leaked an input value")
			}
		})
	}
}

func TestEveryLeafHasEnvironmentAndFlagBindings(t *testing.T) {
	encoded, err := yaml.Marshal(Defaults())
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(encoded, &values); err != nil {
		t.Fatal(err)
	}
	leaves := make(map[string]bool)
	var walk func(map[string]any, string)
	walk = func(values map[string]any, prefix string) {
		for key, value := range values {
			if nested, ok := value.(map[string]any); ok {
				walk(nested, prefix+key+".")
			} else {
				leaves[prefix+key] = true
			}
		}
	}
	walk(values, "")
	flags := Flags()
	if len(leaves) != len(flags) {
		t.Fatalf("%d YAML leaves have %d flag bindings", len(leaves), len(flags))
	}
	environment := make(map[string]string)
	overrides := make(map[string]string)
	for _, flag := range flags {
		if !leaves[flag.Key] {
			t.Fatalf("duplicate or unknown flag key %q", flag.Key)
		}
		delete(leaves, flag.Key)
		environment["TOKEN_ROUTER_"+strings.ToUpper(strings.ReplaceAll(flag.Key, ".", "_"))] = flag.Default
		overrides[flag.Key] = flag.Default
	}
	for _, options := range []Options{
		{LookupEnv: lookup(environment)}, {LookupEnv: lookup(nil), Overrides: overrides},
	} {
		options.Path = filepath.Join(t.TempDir(), "absent.yaml")
		cfg, err := Load(options)
		if err != nil {
			t.Fatal(err)
		}
		if cfg != Defaults() {
			t.Error("default flag or environment values do not round-trip")
		}
	}
}

func TestExampleConfigurationMatchesDefaults(t *testing.T) {
	cfg, err := Load(Options{Path: "../../../config.yaml", PathExplicit: true, LookupEnv: lookup(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if cfg != Defaults() {
		t.Error("checked-in config.yaml differs from built-in defaults")
	}
}

func TestInvalidOverridesDoNotLeakValues(t *testing.T) {
	for _, options := range []Options{
		{LookupEnv: lookup(map[string]string{"TOKEN_ROUTER_RATE_LIMIT_BURST": "secret-value"})},
		{LookupEnv: lookup(map[string]string{"TOKEN_ROUTER_METRICS_ENABLED": "secret-value"})},
		{LookupEnv: lookup(nil), Overrides: map[string]string{"server.read_timeout": "secret-value"}},
		{LookupEnv: lookup(nil), Overrides: map[string]string{"rate_limit.requests_per_second": "secret-value"}},
		{LookupEnv: lookup(nil), Overrides: map[string]string{"secret-value": "secret-value"}},
	} {
		options.Path = filepath.Join(t.TempDir(), "absent.yaml")
		_, err := Load(options)
		if err == nil || strings.Contains(err.Error(), "secret-value") {
			t.Errorf("expected sanitized error, got %v", err)
		}
	}
}

func lookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) { value, ok := values[name]; return value, ok }
}

func writeConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
