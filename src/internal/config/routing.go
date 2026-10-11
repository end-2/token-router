package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

// UpstreamRoute matches Host and the top-level JSON model, with empty conditions matching any value.
type UpstreamRoute struct {
	Host     string   `yaml:"host" json:"host"`
	Model    string   `yaml:"model" json:"model"`
	BaseURLs []string `yaml:"base_urls" json:"base_urls"`
}

// ParseUpstreamURL accepts an HTTP(S) base URL or a host:port address using HTTP.
// Errors omit input values because configuration may contain credentials.
func ParseUpstreamURL(raw string) (*url.URL, error) {
	invalid := errors.New("must be an HTTP or HTTPS URL or host:port without credentials, query, or fragment")
	if !strings.Contains(raw, "://") {
		host, port, err := net.SplitHostPort(raw)
		if err != nil || host == "" || !validPort(port) {
			return nil, invalid
		}
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") || u.Opaque != "" ||
		strings.ContainsAny(u.Host, "\\*") || strings.ContainsFunc(u.Host, unicode.IsSpace) ||
		strings.HasSuffix(u.Host, ":") || (u.Port() != "" && !validPort(u.Port())) {
		return nil, invalid
	}
	if (strings.Contains(u.Hostname(), ":") || strings.HasPrefix(u.Host, "[")) && (net.ParseIP(u.Hostname()) == nil || !strings.HasPrefix(u.Host, "[")) {
		return nil, invalid
	}
	return u, nil
}

// ValidateRouting checks route conditions, destination lists, and the body inspection limit.
func (c Upstream) ValidateRouting() error {
	if len(c.BaseURLs) == 0 && len(c.Routes) == 0 {
		return errors.New("upstream requires at least one destination")
	}
	if c.MaxRoutingBodyBytes <= 0 {
		return errors.New("upstream.max_routing_body_bytes must be positive")
	}
	if err := validateBaseURLs("upstream.base_urls", c.BaseURLs); err != nil {
		return err
	}
	for i, route := range c.Routes {
		key := fmt.Sprintf("upstream.routes[%d]", i)
		if route.Host != "" {
			u, err := ParseUpstreamURL("http://" + route.Host)
			if err != nil || u.Path != "" || strings.TrimSuffix(u.Hostname(), ".") == "" {
				return fmt.Errorf("%s.host must be a hostname or IP address with an optional port", key)
			}
		}
		if strings.ContainsFunc(route.Model, unicode.IsControl) {
			return fmt.Errorf("%s.model must not contain control characters", key)
		}
		if len(route.BaseURLs) == 0 {
			return fmt.Errorf("%s.base_urls must not be empty", key)
		}
		if err := validateBaseURLs(key+".base_urls", route.BaseURLs); err != nil {
			return err
		}
	}
	return nil
}

func validateBaseURLs(key string, values []string) error {
	for i, value := range values {
		if _, err := ParseUpstreamURL(value); err != nil {
			return fmt.Errorf("%s[%d] %w", key, i, err)
		}
	}
	return nil
}

func validPort(raw string) bool {
	port, err := strconv.Atoi(raw)
	return err == nil && port > 0 && port <= 65535
}
