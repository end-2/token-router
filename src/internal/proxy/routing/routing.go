// Package routing selects upstream pools using request hosts and models.
package routing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"token-router/src/internal/config"
)

type upstreamPool struct {
	targets []*url.URL
	next    atomic.Uint64
}

func (pool *upstreamPool) selectTarget() *url.URL {
	if len(pool.targets) == 1 {
		return pool.targets[0]
	}
	return pool.targets[(pool.next.Add(1)-1)%uint64(len(pool.targets))]
}

type routeCondition struct {
	host, port, model string
}

func (condition routeCondition) matchesHost(host, port string) bool {
	return condition.host == "" || (condition.host == host && (condition.port == "" || condition.port == port))
}

type upstreamRoute struct {
	routeCondition
	pool        *upstreamPool
	specificity int
}

// Router selects destinations from immutable rules and concurrent round-robin pools.
type Router struct {
	routes       []upstreamRoute
	maxBodyBytes int64
}

// New validates routing configuration and constructs upstream pools.
func New(cfg config.Upstream) (*Router, error) {
	if err := cfg.ValidateRouting(); err != nil {
		return nil, err
	}

	routing := &Router{maxBodyBytes: int64(cfg.MaxRoutingBodyBytes)}
	pools := make(map[routeCondition]*upstreamPool)
	for _, rule := range cfg.Routes {
		host, port := splitRouteHost(normalizeCondition(rule.Host))
		condition := routeCondition{host: host, port: port, model: normalizeCondition(rule.Model)}
		pool, err := newUpstreamPool(rule.BaseURLs)
		if err != nil {
			return nil, err
		}
		if existing := pools[condition]; existing != nil {
			existing.targets = append(existing.targets, pool.targets...)
			continue
		}
		pools[condition] = pool

		specificity := 0
		if host != "" {
			specificity++
		}
		if condition.model != "" {
			specificity++
		}
		routing.routes = append(routing.routes, upstreamRoute{condition, pool, specificity})
	}
	return routing, nil
}

func normalizeCondition(value string) string {
	if value == "*" {
		return ""
	}
	return value
}

func newUpstreamPool(addresses []string) (*upstreamPool, error) {
	pool := &upstreamPool{targets: make([]*url.URL, 0, len(addresses))}
	for _, address := range addresses {
		target, err := config.ParseUpstreamURL(address)
		if err != nil {
			return nil, err
		}
		pool.targets = append(pool.targets, target)
	}
	return pool, nil
}

func splitRouteHost(authority string) (string, string) {
	if !strings.Contains(authority, ":") {
		return strings.ToLower(strings.TrimSuffix(strings.Trim(authority, "[]"), ".")), ""
	}
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		host, port = strings.Trim(authority, "[]"), ""
	}
	return strings.ToLower(strings.TrimSuffix(host, ".")), port
}

// Error describes the HTTP response for an unsuccessful route selection.
type Error struct {
	Status        int
	Code, Message string
}

// SelectTarget selects an upstream and preserves inspected request bytes for forwarding.
func (routing *Router) SelectTarget(writer http.ResponseWriter, request *http.Request) (*url.URL, *Error) {
	if len(routing.routes) == 1 && routing.routes[0].specificity == 0 {
		return routing.routes[0].pool.selectTarget(), nil
	}

	host, port := splitRouteHost(request.Host)
	needsModel := false
	for _, route := range routing.routes {
		if route.model != "" && route.matchesHost(host, port) {
			needsModel = true
			break
		}
	}

	model := ""
	if needsModel {
		var err *Error
		model, err = routing.readModel(writer, request)
		if err != nil {
			return nil, err
		}
	}

	var selected *upstreamPool
	specificity := -1
	for _, route := range routing.routes {
		if route.specificity > specificity && route.matchesHost(host, port) && (route.model == "" || route.model == model) {
			selected, specificity = route.pool, route.specificity
		}
	}
	if selected == nil {
		return nil, &Error{http.StatusNotFound, "route_not_found", "No upstream route matches the request."}
	}
	return selected.selectTarget(), nil
}

func (routing *Router) readModel(writer http.ResponseWriter, request *http.Request) (string, *Error) {
	if request.Body == nil || request.Body == http.NoBody {
		return "", nil
	}
	if raw := request.Header.Get("Content-Type"); raw != "" {
		mediaType, _, err := mime.ParseMediaType(raw)
		if err != nil || (mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json")) {
			return "", nil
		}
	}
	if encoding := strings.TrimSpace(request.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return "", &Error{http.StatusUnsupportedMediaType, "unsupported_content_encoding", "Model routing requires an uncompressed JSON request body."}
	}

	tooLarge := &Error{http.StatusRequestEntityTooLarge, "routing_body_too_large", "The request body exceeds the model routing limit."}
	if request.ContentLength > routing.maxBodyBytes {
		return "", tooLarge
	}

	// Preserve the original closer and bytes so forwarding retains trailers and body ownership.
	original := request.Body
	// A context timeout alone does not interrupt a server-side Body.Read.
	// Set the read deadline only on cancellation to preserve server.read_timeout.
	interrupted := make(chan struct{})
	stop := context.AfterFunc(request.Context(), func() {
		defer close(interrupted)
		if err := http.NewResponseController(writer).SetReadDeadline(time.Now()); err != nil {
			return
		}
	})
	payload, err := io.ReadAll(http.MaxBytesReader(writer, original, routing.maxBodyBytes))
	if !stop() {
		<-interrupted
	}
	if err != nil {
		var sizeError *http.MaxBytesError
		if errors.As(err, &sizeError) {
			return "", tooLarge
		}
		return "", &Error{http.StatusBadRequest, "invalid_request_body", "The request body could not be read for model routing."}
	}

	request.Body = &replayBody{Reader: bytes.NewReader(payload), Closer: original}
	if len(bytes.TrimSpace(payload)) == 0 {
		return "", nil
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil || object == nil {
		return "", &Error{http.StatusBadRequest, "invalid_request_body", "Model routing requires a JSON object."}
	}

	var model string
	if raw, ok := object["model"]; ok {
		if err := json.Unmarshal(raw, &model); err != nil {
			return "", &Error{http.StatusBadRequest, "invalid_model", "The request model must be a string."}
		}
	}
	return model, nil
}

type replayBody struct {
	io.Reader
	io.Closer
}
