package config

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"

	"go.yaml.in/yaml/v3"
)

// MaxPolicyBytes bounds both file reads and admin request bodies.
const MaxPolicyBytes = 1 << 20

// LimitValues contains limits and account selection that can change without replacing the backend.
type LimitValues struct {
	RequestsPerSecond float64 `yaml:"requests_per_second"`
	Burst             int     `yaml:"burst"`
	TokensPerSecond   float64 `yaml:"tokens_per_second"`
	TokenBurst        int     `yaml:"token_burst"`
	Account           Account `yaml:"account"`
}

// PolicyConfig is the complete set of settings accepted during policy updates.
type PolicyConfig struct {
	Upstream  Upstream    `yaml:"upstream"`
	RateLimit LimitValues `yaml:"rate_limit"`
}

// Mutable returns an independent copy of the currently configured mutable fields.
func (c Config) Mutable() PolicyConfig {
	return PolicyConfig{
		Upstream: c.Upstream,
		RateLimit: LimitValues{RequestsPerSecond: c.RateLimit.RequestsPerSecond, Burst: c.RateLimit.Burst,
			TokensPerSecond: c.RateLimit.TokensPerSecond, TokenBurst: c.RateLimit.TokenBurst, Account: c.RateLimit.Account},
	}.Clone()
}

// PolicyDefaults returns the startup settings before the policy file is applied.
func (c Config) PolicyDefaults() PolicyConfig {
	if c.policyBase != nil {
		return c.policyBase.Clone()
	}
	return c.Mutable()
}

// Clone detaches route and destination slices from an existing snapshot.
func (c PolicyConfig) Clone() PolicyConfig {
	c.Upstream.Routes = slices.Clone(c.Upstream.Routes)
	for i := range c.Upstream.Routes {
		c.Upstream.Routes[i].BaseURLs = slices.Clone(c.Upstream.Routes[i].BaseURLs)
	}
	return c
}

// Apply overlays mutable settings while preserving startup-only settings.
func (c PolicyConfig) Apply(base Config) Config {
	base.Upstream = c.Clone().Upstream
	base.RateLimit.RequestsPerSecond = c.RateLimit.RequestsPerSecond
	base.RateLimit.Burst = c.RateLimit.Burst
	base.RateLimit.TokensPerSecond = c.RateLimit.TokensPerSecond
	base.RateLimit.TokenBurst = c.RateLimit.TokenBurst
	base.RateLimit.Account = c.RateLimit.Account
	return base
}

// DecodePolicy accepts a strict YAML or JSON mapping over the supplied defaults.
// Validation of the merged static and mutable settings is performed by Config.Validate.
func DecodePolicy(data []byte, base PolicyConfig) (PolicyConfig, error) {
	if len(data) > MaxPolicyBytes {
		return PolicyConfig{}, errors.New("policy configuration exceeds size limit")
	}

	c := base.Clone()
	if err := decodeYAMLValue(data, &c, yaml.MappingNode); err != nil {
		return PolicyConfig{}, err
	}
	return c, nil
}

// MarshalJSON uses YAML field names and duration strings for round-trip API edits.
func (c PolicyConfig) MarshalJSON() ([]byte, error) {
	data, err := yaml.Marshal(c)
	if err != nil {
		return nil, err
	}

	var value map[string]any
	if err := yaml.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

// ReadPolicyFile reads a bounded snapshot, following file and directory symlinks.
func ReadPolicyFile(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // The operator explicitly selects the configuration file path.
	if err != nil {
		return nil, errors.New("cannot read policy configuration file")
	}

	data, readErr := io.ReadAll(io.LimitReader(f, MaxPolicyBytes+1))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.New("cannot read policy configuration file")
	}

	if len(data) > MaxPolicyBytes {
		return nil, errors.New("policy configuration exceeds size limit")
	}
	return data, nil
}
