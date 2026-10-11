package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func distributedConfig() Config {
	cfg := Defaults()
	cfg.Admin.Enabled = true
	cfg.Admin.Address = "0.0.0.0:9091"
	cfg.RateLimit.Memory.Distributed = Distributed{
		Enabled: true, Address: "10.0.0.1:9091", Peers: []string{"10.0.0.1:9091", "10.0.0.2:9091", "10.0.0.3:9091"},
		DataDir: "data/rate-limit", Token: "cluster-secret", RequestTimeout: time.Second,
	}
	return cfg
}

func TestDistributedConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(*Config)
		valid bool
	}{
		{"valid", func(*Config) {}, true},
		{"disabled", func(c *Config) { c.RateLimit.Enabled = false; c.RateLimit.Memory.Distributed.Token = "" }, true},
		{"local memory", func(c *Config) {
			c.RateLimit.Memory.Distributed.Enabled = false
			c.RateLimit.Memory.Distributed.Token = ""
		}, true},
		{"redis ignores memory", func(c *Config) { c.RateLimit.Backend = "redis"; c.RateLimit.Memory.Distributed.Token = "" }, true},
		{"no admin", func(c *Config) { c.Admin.Enabled = false }, false},
		{"wrong admin port", func(c *Config) { c.Admin.Address = "0.0.0.0:9999" }, false},
		{"ephemeral admin port", func(c *Config) { c.Admin.Address = "0.0.0.0:0" }, false},
		{"dns address", func(c *Config) { c.RateLimit.Memory.Distributed.Address = "localhost:9091" }, false},
		{"wildcard", func(c *Config) { c.RateLimit.Memory.Distributed.Address = "0.0.0.0:9091" }, false},
		{"mapped ipv4", func(c *Config) { c.RateLimit.Memory.Distributed.Address = "[::ffff:10.0.0.1]:9091" }, false},
		{"missing self", func(c *Config) { c.RateLimit.Memory.Distributed.Peers = []string{"10.0.0.2:9091"} }, false},
		{"invalid peer", func(c *Config) {
			c.RateLimit.Memory.Distributed.Peers = append(c.RateLimit.Memory.Distributed.Peers, "secret-value")
		}, false},
		{"duplicate peer", func(c *Config) {
			c.RateLimit.Memory.Distributed.Peers = append(c.RateLimit.Memory.Distributed.Peers, "10.0.0.1:9091")
		}, false},
		{"empty token", func(c *Config) { c.RateLimit.Memory.Distributed.Token = "" }, false},
		{"header injection", func(c *Config) { c.RateLimit.Memory.Distributed.Token = "secret-value\r\nInjected: value" }, false},
		{"empty directory", func(c *Config) { c.RateLimit.Memory.Distributed.DataDir = " " }, false},
		{"zero timeout", func(c *Config) { c.RateLimit.Memory.Distributed.RequestTimeout = 0 }, false},
		{"ipv6", func(c *Config) {
			c.Admin.Address = "[::]:9091"
			c.RateLimit.Memory.Distributed.Address = "[::1]:9091"
			c.RateLimit.Memory.Distributed.Peers = []string{"[::1]:9091"}
		}, true},
		{"missing JWT claim", func(c *Config) { c.RateLimit.Account = Account{Enabled: true} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := distributedConfig()
			tc.apply(&cfg)
			err := cfg.Validate()
			if (err == nil) != tc.valid || (err != nil && strings.Contains(err.Error(), "secret-value")) {
				t.Fatalf("Validate = %v, want valid=%v", err, tc.valid)
			}
		})
	}
}

func TestDistributedAndAccountConfigurationLayers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeConfig(t, path, `admin:
  enabled: true
  address: '0.0.0.0:9091'
policy:
  file: policy.yaml
rate_limit:
  memory:
    distributed:
      enabled: true
      address: '10.0.0.1:9091'
      peers: ['10.0.0.1:9091']
      data_dir: state
`)
	writeConfig(t, filepath.Join(dir, "policy.yaml"), `rate_limit:
  account:
    enabled: true
    jwt_claim: account_id
`)
	cfg, err := Load(Options{Path: path, PathExplicit: true, LookupEnv: lookup(map[string]string{
		"TOKEN_ROUTER_RATE_LIMIT_MEMORY_DISTRIBUTED_TOKEN": "env-secret",
		"TOKEN_ROUTER_RATE_LIMIT_MEMORY_DISTRIBUTED_PEERS": `["10.0.0.1:9091","10.0.0.2:9091"]`,
	}), Overrides: map[string]string{
		"rate_limit.memory.distributed.request_timeout": "3s",
		"rate_limit.account.jwt_claim":                  "sub",
	}})
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.RateLimit.Memory.Distributed
	if d.DataDir != filepath.Join(dir, "state") || d.Token != "env-secret" || len(d.Peers) != 2 || d.RequestTimeout != 3*time.Second {
		t.Fatal("distributed YAML, environment or CLI values did not load")
	}
	if !cfg.RateLimit.Account.Enabled || cfg.RateLimit.Account.JWTClaim != "account_id" {
		t.Fatalf("policy did not override account settings: %+v", cfg.RateLimit.Account)
	}
	if _, err := DecodePolicy([]byte("rate_limit: {memory: {distributed: {enabled: true}}}"), cfg.Mutable()); err == nil {
		t.Fatal("mutable policy accepted static cluster settings")
	}
}
