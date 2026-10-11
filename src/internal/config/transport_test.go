package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func TestTransportLoadPrecedence(t *testing.T) {
	for _, test := range []struct {
		name      string
		env       map[string]string
		overrides map[string]string
		want      int
	}{
		{name: "YAML", want: 11},
		{name: "environment", env: map[string]string{"TOKEN_ROUTER_TRANSPORT_MAX_CONNS_PER_HOST": "12"}, want: 12},
		{name: "CLI", env: map[string]string{"TOKEN_ROUTER_TRANSPORT_MAX_CONNS_PER_HOST": "12"}, overrides: map[string]string{"transport.max_conns_per_host": "13"}, want: 13},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			writeConfig(t, path, "policy:\n  file: policy.yaml\ntransport:\n  max_conns_per_host: 11\n  tls_handshake_timeout: 3s\n")
			writeConfig(t, filepath.Join(filepath.Dir(path), "policy.yaml"), "upstream:\n  request_timeout: 2s\n")
			cfg, err := Load(Options{Path: path, LookupEnv: lookup(test.env), Overrides: test.overrides})
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Transport.MaxConnsPerHost != test.want || cfg.Transport.TLSHandshakeTimeout != 3*time.Second ||
				cfg.Transport.DialTimeout != 10*time.Second || cfg.Upstream.RequestTimeout != 2*time.Second {
				t.Fatal("transport settings did not retain precedence or independent timeout defaults")
			}
			reset := cfg.PolicyDefaults().Apply(cfg)
			if reset.Transport != cfg.Transport {
				t.Fatal("policy replacement changed startup transport settings")
			}
		})
	}
}

func TestTransportExplicitZeroAndFalse(t *testing.T) {
	data, err := yaml.Marshal(struct {
		Transport Transport `yaml:"transport"`
	}{})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, path, string(data))
	cfg, err := Load(Options{Path: path, LookupEnv: lookup(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Transport != (Transport{}) {
		t.Fatal("explicit zero and false transport values were replaced by defaults")
	}
}

func TestTransportRejectsNegativeValues(t *testing.T) {
	for _, test := range []struct{ key, value string }{
		{"dial_timeout", "-1s"}, {"tcp_keep_alive", "-1s"}, {"tls_handshake_timeout", "-1s"},
		{"response_header_timeout", "-1s"}, {"expect_continue_timeout", "-1s"}, {"idle_conn_timeout", "-1s"},
		{"max_idle_conns", "-1"}, {"max_idle_conns_per_host", "-1"}, {"max_conns_per_host", "-1"},
		{"read_buffer_size", "-1"}, {"write_buffer_size", "-1"}, {"max_response_header_bytes", "-1"},
	} {
		t.Run(test.key, func(t *testing.T) {
			key := "transport." + test.key
			_, err := Load(Options{Path: filepath.Join(t.TempDir(), "absent.yaml"), LookupEnv: lookup(nil), Overrides: map[string]string{key: test.value}})
			if err == nil || !strings.Contains(err.Error(), key+" must not be negative") {
				t.Fatalf("expected field-specific validation error, got %v", err)
			}
		})
	}
}

func TestTransportTypedOverrides(t *testing.T) {
	for _, layer := range []string{"environment", "CLI"} {
		t.Run(layer, func(t *testing.T) {
			options := Options{Path: filepath.Join(t.TempDir(), "absent.yaml"), LookupEnv: lookup(nil)}
			if layer == "environment" {
				options.LookupEnv = lookup(map[string]string{
					"TOKEN_ROUTER_TRANSPORT_MAX_RESPONSE_HEADER_BYTES": "8589934592",
					"TOKEN_ROUTER_TRANSPORT_FORCE_ATTEMPT_HTTP2":       "false",
					"TOKEN_ROUTER_TRANSPORT_MAX_IDLE_CONNS":            "0",
				})
			} else {
				options.Overrides = map[string]string{
					"transport.max_response_header_bytes": "8589934592",
					"transport.force_attempt_http2":       "false",
					"transport.max_idle_conns":            "0",
				}
			}
			cfg, err := Load(options)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Transport.MaxResponseHeaderBytes != 1<<33 || cfg.Transport.ForceAttemptHTTP2 || cfg.Transport.MaxIdleConns != 0 {
				t.Fatal("transport override lost a 64-bit integer, explicit false, or zero")
			}
		})
	}
}
