package app

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"token-router/src/internal/config"
)

// accountIdentifier reads identity only. Authentication and JWT signature,
// issuer, audience and expiration validation belong to the trusted auth layer.
func accountIdentifier(r *http.Request, cfg config.Account) (string, bool) {
	if len(r.Header.Values("Authorization")) != 1 || len(r.Header.Get("Authorization")) > 16<<10 {
		return "", false
	}
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 {
		return "", false
	}
	var identifier string
	switch {
	case strings.EqualFold(parts[0], "Basic"):
		decoded, err := base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return "", false
		}
		var ok bool
		identifier, _, ok = strings.Cut(string(decoded), ":")
		if !ok {
			return "", false
		}
	case strings.EqualFold(parts[0], "Bearer"):
		segments := strings.Split(parts[1], ".")
		if len(segments) != 3 || segments[0] == "" || segments[1] == "" {
			return "", false
		}
		payload, err := base64.RawURLEncoding.DecodeString(segments[1])
		if err != nil {
			return "", false
		}
		var claims map[string]json.RawMessage
		if err := json.Unmarshal(payload, &claims); err != nil {
			return "", false
		}
		if err := json.Unmarshal(claims[cfg.JWTClaim], &identifier); err != nil {
			return "", false
		}
	default:
		return "", false
	}
	return identifier, len(identifier) <= 1024 && strings.TrimSpace(identifier) != "" &&
		utf8.ValidString(identifier) && strings.IndexFunc(identifier, unicode.IsControl) < 0
}
