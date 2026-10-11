// Package observability defines bounded labels shared by metrics and tracing.
package observability

import "net/http"

// LimitOutcomes lists rate limiter results in the order used by LimitOutcome.
var LimitOutcomes = [...]string{"allowed", "denied", "error"}

// BoundedMethod limits HTTP method labels to standard methods and OTHER.
func BoundedMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace, http.MethodPatch:
		return method
	default:
		return "OTHER"
	}
}

// LimitOutcome indexes LimitOutcomes for an admission result.
func LimitOutcome(allowed bool, err error) int {
	if err != nil {
		return 2
	}
	if !allowed {
		return 1
	}
	return 0
}
