package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseUpstreamURL(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"https://example.com/gateway", "https://example.com/gateway"},
		{"http://example.com:8080/a%2Fb", "http://example.com:8080/a%2Fb"},
		{"127.0.0.1:9000", "http://127.0.0.1:9000"},
		{"inference:8000", "http://inference:8000"},
		{"[::1]:9000", "http://[::1]:9000"},
		{"https://[::1]/gateway", "https://[::1]/gateway"},
	} {
		t.Run(test.input, func(t *testing.T) {
			got, err := ParseUpstreamURL(test.input)
			if err != nil || got.String() != test.want {
				t.Fatalf("ParseUpstreamURL = %v, %v; want %s", got, err, test.want)
			}
		})
	}
	for _, input := range []string{
		"", "example.com", ":9000", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:abc",
		"127.0.0.1:9000/path", "//example.com", "ftp://example.com", "https://example.com:",
		"http://::1:9000", "http://example.com:65536", "http://[invalid]:9000", "http://bad host",
		"http://secret-value@example.com", "http://example.com?secret-value", "http://example.com?",
		"http://example.com#secret-value", "http://example.com#", "http://*.example.com", "http://example.com\n",
	} {
		t.Run(input, func(t *testing.T) {
			_, err := ParseUpstreamURL(input)
			if err == nil || strings.Contains(err.Error(), "secret-value") {
				t.Fatalf("expected sanitized validation error, got %v", err)
			}
		})
	}
}

func TestLoadRoutingPrecedence(t *testing.T) {
	for _, layer := range []string{"yaml", "environment", "flags"} {
		t.Run(layer, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			body := "upstream:\n  base_urls: [old:8000]\n  routes: [{model: old, base_urls: [old:8000]}]\n"
			options := Options{Path: path, LookupEnv: lookup(nil)}
			targets := `["127.0.0.1:9000","https://example.com/gateway"]`
			routes := `[{"host":"API.example.com","model":"chat","base_urls":["127.0.0.1:9001","https://example.com"]}]`
			switch layer {
			case "yaml":
				body = "upstream:\n  base_urls: " + targets + "\n  routes: " + routes + "\n"
			case "environment":
				options.LookupEnv = lookup(map[string]string{"TOKEN_ROUTER_UPSTREAM_BASE_URLS": targets, "TOKEN_ROUTER_UPSTREAM_ROUTES": routes})
			case "flags":
				options.LookupEnv = lookup(map[string]string{"TOKEN_ROUTER_UPSTREAM_BASE_URLS": `["env:8000"]`, "TOKEN_ROUTER_UPSTREAM_ROUTES": "[]"}) //nolint:gosec // These routing environment values are test fixtures, not credentials.
				options.Overrides = map[string]string{"upstream.base_urls": targets, "upstream.routes": routes}
			}
			writeConfig(t, path, body)
			cfg, err := Load(options)
			if err != nil {
				t.Fatal(err)
			}
			wantRoutes := []UpstreamRoute{{Host: "API.example.com", Model: "chat", BaseURLs: []string{"127.0.0.1:9001", "https://example.com"}}}
			if !reflect.DeepEqual(cfg.Upstream.Routes, wantRoutes) || !reflect.DeepEqual(cfg.Upstream.BaseURLs, []string{"127.0.0.1:9000", "https://example.com/gateway"}) {
				t.Fatalf("routing arrays were not replaced: %+v", cfg.Upstream)
			}
		})
	}
}

func TestRejectInvalidRoutingConfiguration(t *testing.T) {
	for _, test := range []struct{ name, body string }{
		{"empty route pool", "routes: [{model: chat, base_urls: []}]"},
		{"missing route pool", "routes: [{host: example.com}]"},
		{"unknown route field", "routes: [{models: secret-value, base_urls: [localhost:9000]}]"},
		{"invalid default target", "base_urls: [secret-value]"},
		{"invalid route target", "routes: [{base_urls: ['ftp://secret-value']}]"},
		{"host is URL", "routes: [{host: 'http://secret-value', base_urls: [localhost:9000]}]"},
		{"host is path", "routes: [{host: 'secret-value/path', base_urls: [localhost:9000]}]"},
		{"empty normalized host", "routes: [{host: '.', base_urls: [localhost:9000]}]"},
		{"wildcard host", "routes: [{host: '*.example.com', base_urls: [localhost:9000]}]"},
		{"host port out of range", "routes: [{host: 'example.com:65536', base_urls: [localhost:9000]}]"},
		{"model control", `routes: [{model: "secret-value\n", base_urls: [localhost:9000]}]`},
		{"null array", "routes: null"},
		{"null target", "base_urls: [null]"},
		{"zero body limit", "max_routing_body_bytes: 0"},
		{"negative body limit", "max_routing_body_bytes: -1"},
		{"no destinations", "base_urls: []"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			writeConfig(t, path, "upstream:\n  "+test.body+"\n")
			_, err := Load(Options{Path: path, LookupEnv: lookup(nil)})
			if err == nil || strings.Contains(err.Error(), "secret-value") {
				t.Fatalf("expected sanitized routing error, got %v", err)
			}
		})
	}
	for _, raw := range []string{"null", "secret-value", "[null]", "[{model: chat, base_urls: [null]}]", "[]\n---\n[]", "[{unknown: secret-value}]", "[{model: a, model: b}]"} {
		t.Run(raw, func(t *testing.T) {
			_, err := Load(Options{Path: filepath.Join(t.TempDir(), "absent.yaml"), LookupEnv: lookup(nil), Overrides: map[string]string{"upstream.routes": raw}})
			if err == nil || strings.Contains(err.Error(), "secret-value") {
				t.Fatalf("expected sanitized array error, got %v", err)
			}
		})
	}
}

func TestRoutingWithSingleDefaultOrNoFallback(t *testing.T) {
	for _, body := range []string{"base_urls: [localhost:9000]", "base_urls: []\n  routes: [{model: chat, base_urls: [localhost:9000]}]"} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		writeConfig(t, path, "upstream:\n  "+body+"\n")
		if _, err := Load(Options{Path: path, LookupEnv: lookup(nil)}); err != nil {
			t.Fatal(err)
		}
	}
}
