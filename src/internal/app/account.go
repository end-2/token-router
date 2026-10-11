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
	values := r.Header.Values("Authorization")
	if len(values) != 1 || len(values[0]) > 16<<10 {
		return "", false
	}

	var parts [2]string
	count := 0
	for part := range strings.FieldsSeq(values[0]) {
		if count == len(parts) {
			return "", false
		}
		parts[count] = part
		count++
	}
	if count != len(parts) {
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
		header, remainder, ok := strings.Cut(parts[1], ".")
		if !ok || header == "" {
			return "", false
		}
		encoded, signature, ok := strings.Cut(remainder, ".")
		if !ok || encoded == "" || strings.Contains(signature, ".") {
			return "", false
		}

		payload, err := base64.RawURLEncoding.DecodeString(encoded)
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
