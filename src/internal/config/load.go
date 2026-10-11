package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

// Options supplies the optional configuration file, environment, and CLI values.
type Options struct {
	Path         string
	PathExplicit bool
	LookupEnv    func(string) (string, bool)
	// Overrides maps dotted YAML keys to explicitly supplied flag values.
	Overrides map[string]string
}

// Load applies defaults, YAML, environment variables, and explicit overrides in order.
func Load(options Options) (Config, error) {
	cfg := Defaults()
	lookup := options.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	path := options.Path
	explicit := options.PathExplicit
	if !explicit {
		if environmentPath, ok := lookup("TOKEN_ROUTER_CONFIG"); ok {
			path, explicit = environmentPath, true
		}
	}
	if path == "" {
		if explicit {
			return Config{}, errors.New("configuration file path must not be empty")
		}
		path = "config.yaml"
	}
	data, err := os.ReadFile(path)
	if err != nil && (explicit || !errors.Is(err, os.ErrNotExist)) {
		return Config{}, errors.New("cannot read configuration file")
	}
	if err == nil {
		if err := decodeYAML(data, &cfg); err != nil {
			return Config{}, err
		}
	}
	settings := fields(&cfg)
	known := make(map[string]field, len(settings))
	for _, setting := range settings {
		known[setting.key] = setting
		if value, ok := lookup(setting.envName()); ok {
			if err := setting.set(value); err != nil {
				return Config{}, fmt.Errorf("environment %s: %w", setting.envName(), err)
			}
		}
	}
	for key, value := range options.Overrides {
		setting, ok := known[key]
		if !ok {
			return Config{}, errors.New("unknown configuration override")
		}
		if err := setting.set(value); err != nil {
			return Config{}, fmt.Errorf("command-line override: %w", err)
		}
	}
	base := cfg.Mutable()
	if cfg.Policy.File != "" {
		cfg.policyBase = &base
		if !filepath.IsAbs(cfg.Policy.File) {
			cfg.Policy.File = filepath.Join(filepath.Dir(path), cfg.Policy.File)
		}
		cfg.Policy.File, err = filepath.Abs(cfg.Policy.File)
		if err != nil {
			return Config{}, errors.New("cannot resolve policy configuration file")
		}
		data, err := ReadPolicyFile(cfg.Policy.File)
		if err != nil {
			return Config{}, err
		}
		mutable, err := DecodePolicy(data, base)
		if err != nil {
			return Config{}, err
		}
		cfg = mutable.Apply(cfg)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

func decodeYAML(data []byte, cfg *Config) error {
	return decodeYAMLValue(data, cfg, yaml.MappingNode)
}

func decodeYAMLValue(data []byte, target any, kind yaml.Kind) error {
	var document yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&document); err != nil {
		return errors.New("invalid YAML configuration")
	}
	if len(document.Content) != 1 || document.Content[0].Kind != kind {
		return errors.New("configuration has an unexpected YAML value type")
	}
	if containsNull(&document, make(map[*yaml.Node]bool)) {
		return errors.New("configuration must not contain null values")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("configuration must contain exactly one YAML document")
	}
	strict := yaml.NewDecoder(bytes.NewReader(data))
	strict.KnownFields(true)
	if err := strict.Decode(target); err != nil {
		// YAML decoding errors can include API keys and other input values.
		return errors.New("invalid YAML configuration: check field names, value types, and duration units")
	}
	return nil
}

func containsNull(node *yaml.Node, seen map[*yaml.Node]bool) bool {
	if node == nil || seen[node] {
		return false
	}
	seen[node] = true
	if node.Tag == "!!null" {
		return true
	}
	if containsNull(node.Alias, seen) {
		return true
	}
	for _, child := range node.Content {
		if containsNull(child, seen) {
			return true
		}
	}
	return false
}
