package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPolicyFileLoadAndPrecedence(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "config.yaml")
	file := filepath.Join(dir, "policy.yaml")
	writeConfig(t, main, "policy:\n  file: policy.yaml\nadmin:\n  enabled: true\nrate_limit:\n  backend: redis\n")
	writeConfig(t, file, "upstream:\n  base_urls: ['localhost:9000']\nrate_limit:\n  burst: 7\n")
	cfg, err := Load(Options{Path: main, LookupEnv: lookup(map[string]string{
		"TOKEN_ROUTER_RATE_LIMIT_BURST": "50", "TOKEN_ROUTER_UPSTREAM_API_KEY": "startup-secret",
	}), Overrides: map[string]string{"rate_limit.burst": "60"}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Policy.File != file || cfg.RateLimit.Burst != 7 || cfg.RateLimit.Backend != "redis" ||
		cfg.Upstream.BaseURLs[0] != "localhost:9000" || cfg.Upstream.APIKey != "startup-secret" || !cfg.Admin.Enabled {
		t.Fatal("policy file did not merge with startup settings")
	}
	if cfg.PolicyDefaults().RateLimit.Burst != 60 {
		t.Fatal("startup fallback was not preserved")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(Options{Path: main, LookupEnv: lookup(nil)}); err == nil {
		t.Fatal("missing policy file must fail startup")
	}
}

func TestPolicyDecodeRejectsStaticAndInvalidSettings(t *testing.T) {
	for _, data := range []string{
		"rate_limit: {backend: redis}", "rate_limit: {enabled: false}",
		"rate_limit: {strategy: memory}", "rate_limit: {redis: {address: 'localhost:6379'}}",
		"server: {address: ':9099'}", "admin: {enabled: true}", "policy: {file: other.yaml}",
		"upstream: {api_key: secret, typo: secret}", "upstream: {base_urls: []}",
		"upstream: {dial_timeout: 1s}", "upstream: {transport: {dial_timeout: 1s}}",
		"transport: {dial_timeout: 1s}", "transport: {}", "rate_limit: {burst: 0}",
		"rate_limit: {tokens_per_second: 2}", "rate_limit: {requests_per_second: .nan}",
		"upstream: null", "upstream: {routes: [null]}", "{}\n---\n{}", "", "[]",
		"rate_limit: {burst: 1, burst: 2}",
	} {
		t.Run(data, func(t *testing.T) {
			cfg := Defaults()
			next, err := DecodePolicy([]byte(data), cfg.Mutable())
			if err == nil {
				err = next.Apply(cfg).Validate()
			}
			if err == nil {
				t.Fatal("invalid update accepted")
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("error leaked input")
			}
		})
	}
}

func TestPolicyJSONRoundTripAndIndependentSnapshots(t *testing.T) {
	cfg := Defaults()
	cfg.Upstream.Routes = []UpstreamRoute{{Host: "api.example.com", BaseURLs: []string{"localhost:8000"}}}
	current := cfg.Mutable()
	data, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"request_timeout":"0s"`) {
		t.Fatal("JSON duration must use config units")
	}
	next, err := DecodePolicy(data, Defaults().Mutable())
	if err != nil || !reflect.DeepEqual(current, next) {
		t.Fatalf("round trip mismatch: %v", err)
	}
	next, err = DecodePolicy([]byte(`{"upstream":{"request_timeout":"2s","routes":[{"base_urls":["localhost:9000"]}]},"rate_limit":{"tokens_per_second":0,"token_burst":0}}`), current)
	if err != nil || next.Upstream.RequestTimeout != 2*time.Second || next.Upstream.Routes[0].Host != "" {
		t.Fatalf("array replacement or duration failed: %v", err)
	}
	next.Upstream.BaseURLs[0] = "localhost:9999"
	if current.Upstream.BaseURLs[0] != cfg.Upstream.BaseURLs[0] || current.Upstream.Routes[0].Host != "api.example.com" {
		t.Fatal("update mutated active snapshot")
	}
}

func TestAdminAndWatcherValidation(t *testing.T) {
	for _, modify := range []func(*Config){
		func(c *Config) { c.Policy.WatchInterval = 0 },
		func(c *Config) { c.Admin.Enabled = true; c.Admin.Address = "bad" },
		func(c *Config) { c.Admin.Enabled = true; c.Admin.Address = c.Server.Address },
		func(c *Config) { c.Admin.Enabled = true; c.Admin.Address = c.Telemetry.Metrics.Prometheus.Address },
	} {
		cfg := Defaults()
		modify(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Fatal("invalid listener or watcher accepted")
		}
	}
}
