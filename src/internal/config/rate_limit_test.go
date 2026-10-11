package config

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadTokenLimitPrecedence(t *testing.T) {
	tests := []struct {
		name      string
		yaml      string
		env       map[string]string
		overrides map[string]string
		wantRate  float64
		wantBurst int
	}{
		{name: "disabled by default"},
		{
			name: "yaml", yaml: "rate_limit:\n  tokens_per_second: 12.5\n  token_burst: 50\n",
			wantRate: 12.5, wantBurst: 50,
		},
		{
			name: "environment over yaml", yaml: "rate_limit:\n  tokens_per_second: 12.5\n  token_burst: 50\n",
			env:      map[string]string{"TOKEN_ROUTER_RATE_LIMIT_TOKENS_PER_SECOND": "25.5", "TOKEN_ROUTER_RATE_LIMIT_TOKEN_BURST": "100"},
			wantRate: 25.5, wantBurst: 100,
		},
		{
			name: "flags over environment", yaml: "rate_limit:\n  tokens_per_second: 12.5\n  token_burst: 50\n",
			env:       map[string]string{"TOKEN_ROUTER_RATE_LIMIT_TOKENS_PER_SECOND": "25.5", "TOKEN_ROUTER_RATE_LIMIT_TOKEN_BURST": "100"},
			overrides: map[string]string{"rate_limit.tokens_per_second": "50.5", "rate_limit.token_burst": "200"},
			wantRate:  50.5, wantBurst: 200,
		},
		{
			name: "environment disables token quota", yaml: "rate_limit:\n  tokens_per_second: 12.5\n  token_burst: 50\n",
			env: map[string]string{"TOKEN_ROUTER_RATE_LIMIT_TOKENS_PER_SECOND": "0", "TOKEN_ROUTER_RATE_LIMIT_TOKEN_BURST": "0"},
		},
		{
			name: "flags disable token quota", yaml: "rate_limit:\n  tokens_per_second: 12.5\n  token_burst: 50\n",
			overrides: map[string]string{"rate_limit.tokens_per_second": "0", "rate_limit.token_burst": "0"},
		},
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
			if cfg.RateLimit.TokensPerSecond != test.wantRate || cfg.RateLimit.TokenBurst != test.wantBurst {
				t.Errorf("token limit = (%v, %d), want (%v, %d)", cfg.RateLimit.TokensPerSecond, cfg.RateLimit.TokenBurst, test.wantRate, test.wantBurst)
			}
			if cfg.RateLimit.RequestsPerSecond != Defaults().RateLimit.RequestsPerSecond || cfg.RateLimit.Burst != Defaults().RateLimit.Burst {
				t.Error("token limit settings changed request limit defaults")
			}
		})
	}
}

func TestValidateTokenLimit(t *testing.T) {
	tests := []struct {
		name      string
		rate      float64
		burst     int
		disabled  bool
		wantError string
	}{
		{name: "zero disables token quota"},
		{name: "positive token quota", rate: 12.5, burst: 50},
		{name: "configured but limiter disabled", rate: 12.5, burst: 50, disabled: true},
		{name: "negative rate", rate: -1, burst: 50, wantError: "rate_limit.tokens_per_second"},
		{name: "nan rate", rate: math.NaN(), burst: 50, wantError: "rate_limit.tokens_per_second"},
		{name: "positive infinity", rate: math.Inf(1), burst: 50, wantError: "rate_limit.tokens_per_second"},
		{name: "negative infinity", rate: math.Inf(-1), burst: 50, wantError: "rate_limit.tokens_per_second"},
		{name: "negative burst", rate: 12.5, burst: -1, wantError: "rate_limit.token_burst"},
		{name: "missing rate", burst: 50, wantError: "must both be positive or both be zero"},
		{name: "missing burst", rate: 12.5, wantError: "must both be positive or both be zero"},
		{name: "unrepresentable refill duration", rate: 1e-100, burst: 50, wantError: "refill time exceeds"},
		{name: "disabled but invalid rate", rate: math.NaN(), burst: 50, disabled: true, wantError: "rate_limit.tokens_per_second"},
		{name: "disabled but partial quota", rate: 12.5, disabled: true, wantError: "must both be positive or both be zero"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.RateLimit.Enabled = !test.disabled
			cfg.RateLimit.TokensPerSecond = test.rate
			cfg.RateLimit.TokenBurst = test.burst
			err := cfg.Validate()
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("unexpected validation error: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("validation error = %v, want %q", err, test.wantError)
			}
		})
	}
}

func TestInvalidTokenLimitOverridesDoNotLeakValues(t *testing.T) {
	for _, options := range []Options{
		{LookupEnv: lookup(map[string]string{"TOKEN_ROUTER_RATE_LIMIT_TOKENS_PER_SECOND": "secret-value"})},
		{LookupEnv: lookup(map[string]string{"TOKEN_ROUTER_RATE_LIMIT_TOKEN_BURST": "secret-value"})},
		{LookupEnv: lookup(nil), Overrides: map[string]string{"rate_limit.tokens_per_second": "secret-value"}},
		{LookupEnv: lookup(nil), Overrides: map[string]string{"rate_limit.token_burst": "secret-value"}},
		{LookupEnv: lookup(nil), Overrides: map[string]string{"rate_limit.token_burst": "1.5"}},
	} {
		options.Path = filepath.Join(t.TempDir(), "absent.yaml")
		_, err := Load(options)
		if err == nil || strings.Contains(err.Error(), "secret-value") {
			t.Errorf("expected sanitized error, got %v", err)
		}
	}
}
