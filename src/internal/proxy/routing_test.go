package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"token-router/src/internal/config"
)

func routingUpstream(t *testing.T, name string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Upstream", name)
		if _, err := io.Copy(writer, request.Body); err != nil {
			t.Errorf("echo request: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func TestRoutingSelectorsAndPriority(t *testing.T) {
	fallback := routingUpstream(t, "fallback")
	host := routingUpstream(t, "host")
	model := routingUpstream(t, "model")
	both := routingUpstream(t, "both")
	port := routingUpstream(t, "port")
	ipv6 := routingUpstream(t, "ipv6")
	cfg := upstreamConfig(fallback)
	cfg.Routes = []config.UpstreamRoute{
		{Host: "api.example.com", BaseURLs: []string{host}},
		{Model: "chat", BaseURLs: []string{model}},
		{Host: "api.example.com", Model: "chat", BaseURLs: []string{both}},
		{Host: "port.example.com:8443", BaseURLs: []string{port}},
		{Host: "[::1]", BaseURLs: []string{ipv6}},
		{Host: "tie.example.com", BaseURLs: []string{host}},
	}
	handler := newTestHandler(t, cfg)
	for _, test := range []struct{ name, host, body, want string }{
		{"host only", "api.example.com", `{}`, "host"},
		{"model only", "other.example.com", `{"model":"chat"}`, "model"},
		{"both conditions", "api.example.com", `{"model":"chat"}`, "both"},
		{"host case dot and port", "API.EXAMPLE.COM.:443", `{"model":"chat"}`, "both"},
		{"host matches model differs", "api.example.com", `{"model":"embed"}`, "host"},
		{"model is case sensitive", "other.example.com", `{"model":"Chat"}`, "fallback"},
		{"JSON key is case sensitive", "other.example.com", `{"MODEL":"chat"}`, "fallback"},
		{"nested model ignored", "other.example.com", `{"metadata":{"model":"chat"}}`, "fallback"},
		{"missing model", "other.example.com", `{}`, "fallback"},
		{"null model", "other.example.com", `{"model":null}`, "fallback"},
		{"empty body", "api.example.com", "", "host"},
		{"unmatched", "other.example.com", `{"model":"embed"}`, "fallback"},
		{"explicit host port", "port.example.com:8443", "", "port"},
		{"different host port", "port.example.com:443", "", "fallback"},
		{"IPv6 host", "[::1]:8080", "", "ipv6"},
		{"equal specificity follows configuration order", "tie.example.com", `{"model":"chat"}`, "model"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(test.body))
			request.Host = test.host
			request.Header.Set("Content-Type", "application/json; charset=utf-8")
			request.Header.Set("X-Forwarded-Host", "api.example.com")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK || response.Header().Get("X-Upstream") != test.want || response.Body.String() != test.body {
				t.Fatalf("status=%d upstream=%q body=%q; want upstream=%q and unchanged body", response.Code, response.Header().Get("X-Upstream"), response.Body.String(), test.want)
			}
		})
	}
}

func TestRoutingRoundRobinAndDuplicateConditions(t *testing.T) {
	first := routingUpstream(t, "first")
	second := routingUpstream(t, "second")
	third := routingUpstream(t, "third")
	cfg := upstreamConfig("")
	cfg.Routes = []config.UpstreamRoute{
		{Host: "api.example.com", Model: "chat", BaseURLs: []string{strings.TrimPrefix(first, "http://"), second}},
		{Host: "API.EXAMPLE.COM.", Model: "chat", BaseURLs: []string{third}},
	}
	handler := newTestHandler(t, cfg)
	forward := func() string {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://api.example.com/v1/responses", strings.NewReader(`{"model":"chat"}`))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Errorf("status = %d", response.Code)
		}
		return response.Header().Get("X-Upstream")
	}
	for i, want := range []string{"first", "second", "third", "first", "second", "third"} {
		if got := forward(); got != want {
			t.Fatalf("request %d selected %q, want %q", i, got, want)
		}
	}
	var wg sync.WaitGroup
	results := make(chan string, 90)
	for range cap(results) {
		wg.Go(func() { results <- forward() })
	}
	wg.Wait()
	close(results)
	counts := make(map[string]int)
	for result := range results {
		counts[result]++
	}
	for _, name := range []string{"first", "second", "third"} {
		if counts[name] != 30 {
			t.Errorf("concurrent requests to %s = %d, want 30", name, counts[name])
		}
	}
}

func TestDefaultPoolAndCatchAll(t *testing.T) {
	first := routingUpstream(t, "first")
	second := routingUpstream(t, "second")
	catchAll := routingUpstream(t, "catch-all")
	for _, test := range []struct {
		name   string
		routes []config.UpstreamRoute
		want   []string
	}{
		{name: "default pool", want: []string{"first", "second", "first"}},
		{name: "catch-all rule overrides default pool", routes: []config.UpstreamRoute{{BaseURLs: []string{catchAll}}}, want: []string{"catch-all", "catch-all", "catch-all"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := upstreamConfig("http://127.0.0.1:1")
			cfg.BaseURLs = []string{first, second}
			cfg.Routes = test.routes
			handler := newTestHandler(t, cfg)
			for _, want := range test.want {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil))
				if response.Code != http.StatusOK || response.Header().Get("X-Upstream") != want {
					t.Fatalf("status=%d upstream=%q; want %q", response.Code, response.Header().Get("X-Upstream"), want)
				}
			}
		})
	}
}

func TestRoutingBodyValidation(t *testing.T) {
	target := routingUpstream(t, "selected")
	for _, test := range []struct {
		name, body, contentType, encoding, host, code string
		status                                        int
		unknownLength, hostOnly                       bool
	}{
		{name: "malformed", body: `{"model":"secret-value"`, status: 400, code: "invalid_request_body"},
		{name: "multiple values", body: `{} {}`, status: 400, code: "invalid_request_body"},
		{name: "array", body: `[]`, status: 400, code: "invalid_request_body"},
		{name: "null", body: `null`, status: 400, code: "invalid_request_body"},
		{name: "nonstring model", body: `{"model":12}`, status: 400, code: "invalid_model"},
		{name: "large known length", body: strings.Repeat("x", 65), status: 413, code: "routing_body_too_large"},
		{name: "large unknown length", body: strings.Repeat("x", 65), unknownLength: true, status: 413, code: "routing_body_too_large"},
		{name: "exact limit", body: `{"model":"chat"}` + strings.Repeat(" ", 48), status: 200},
		{name: "compressed JSON", body: "compressed", encoding: "gzip", status: 415, code: "unsupported_content_encoding"},
		{name: "missing model and no fallback", body: `{}`, status: 404, code: "route_not_found"},
		{name: "unmatched model", body: `{"model":"secret-value"}`, status: 404, code: "route_not_found"},
		{name: "unmatched host bypasses JSON parsing", body: "invalid", host: "other.example.com", status: 404, code: "route_not_found"},
		{name: "host only bypasses JSON parsing and size limit", body: strings.Repeat("x", 65), hostOnly: true, status: 200},
		{name: "multipart passes to host route", body: strings.Repeat("x", 65), contentType: "multipart/form-data; boundary=example", hostOnly: true, status: 200},
		{name: "non-JSON cannot match model", body: `{"model":"chat"}`, contentType: "text/plain", status: 404, code: "route_not_found"},
		{name: "JSON suffix", body: `{"model":"chat"}`, contentType: "application/custom+json", status: 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := upstreamConfig("")
			cfg.MaxRoutingBodyBytes = 64
			model := "chat"
			if test.hostOnly {
				model = ""
			}
			cfg.Routes = []config.UpstreamRoute{{Host: "api.example.com", Model: model, BaseURLs: []string{target}}}
			handler := newTestHandler(t, cfg)
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://api.example.com/v1/responses", strings.NewReader(test.body))
			if test.unknownLength {
				request.ContentLength = -1
			}
			if test.host != "" {
				request.Host = test.host
			}
			request.Header.Set("Content-Type", test.contentType)
			request.Header.Set("Content-Encoding", test.encoding)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status=%d body=%s; want %d", response.Code, response.Body.String(), test.status)
			}
			if test.status == http.StatusOK {
				if response.Body.String() != test.body {
					t.Error("routed request body changed")
				}
			} else if response.Header().Get("X-Upstream") != "" || !strings.Contains(response.Body.String(), test.code) || strings.Contains(response.Body.String(), "secret-value") {
				t.Fatalf("unexpected routing error: %s", response.Body.String())
			}
		})
	}
}

func TestModelRoutingPreservesExchange(t *testing.T) {
	const payload = "{\n  \"model\" : \"chat\", \"input\": \"hello\"\n}\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil || string(body) != payload || request.Method != http.MethodPost || request.URL.EscapedPath() != "/gateway/v1/a%2Fb" || request.URL.RawQuery != "key=a%2Fb" {
			t.Errorf("request changed: method=%s url=%s body=%q err=%v", request.Method, request.URL, body, err)
		}
		if request.Host == "api.example.com" {
			t.Error("incoming Host was forwarded instead of upstream Host")
		}
		if request.Header.Get("Authorization") != "Bearer server-key" || request.Header.Get("X-Forwarded-Host") != "" || request.Trailer.Get("X-Checksum") != "checksum" {
			t.Errorf("headers or trailer changed: headers=%v trailer=%v", request.Header, request.Trailer)
		}
		writer.Header().Set("Retry-After", "12")
		writer.WriteHeader(http.StatusTooManyRequests)
		if _, err := io.WriteString(writer, `{"error":"limited"}`); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer upstream.Close()
	cfg := upstreamConfig("")
	cfg.APIKey = "server-key"
	cfg.Routes = []config.UpstreamRoute{{Host: "api.example.com", Model: "chat", BaseURLs: []string{upstream.URL + "/gateway"}}}
	server := httptest.NewServer(newTestHandler(t, cfg))
	defer server.Close()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/v1/a%2Fb?key=a%2Fb", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "api.example.com"
	request.ContentLength = -1
	request.Trailer = http.Header{"X-Checksum": {"checksum"}}
	request.Header.Set("X-Forwarded-Host", "spoof.example.com")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") != "12" || string(body) != `{"error":"limited"}` {
		t.Fatalf("response changed: status=%d body=%s err=%v", response.StatusCode, body, err)
	}
}

func TestModelRoutingRequestTimeoutInterruptsBodyRead(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer upstream.Close()
	cfg := upstreamConfig("")
	cfg.RequestTimeout = 50 * time.Millisecond
	cfg.Routes = []config.UpstreamRoute{{Model: "chat", BaseURLs: []string{upstream.URL}}}
	server := httptest.NewServer(newTestHandler(t, cfg))
	defer server.Close()
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprint(conn, "POST /v1/responses HTTP/1.1\r\nHost: example.com\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"model\":"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	if response.StatusCode != http.StatusGatewayTimeout || calls.Load() != 0 {
		t.Fatalf("status=%d upstream calls=%d", response.StatusCode, calls.Load())
	}
}

func TestRoutingBodyReadFailure(t *testing.T) {
	cfg := upstreamConfig("")
	cfg.Routes = []config.UpstreamRoute{{Model: "chat", BaseURLs: []string{"127.0.0.1:1"}}}
	handler := newTestHandler(t, cfg)
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", nil)
	request.Body = io.NopCloser(failingRoutingBody{})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), "secret-value") {
		t.Fatalf("unexpected body read error: status=%d body=%s", response.Code, response.Body.String())
	}
}

type failingRoutingBody struct{}

func (failingRoutingBody) Read([]byte) (int, error) {
	return 0, fmt.Errorf("secret-value")
}

func TestRoutingPreservesRequestContext(t *testing.T) {
	type contextKey struct{}
	cfg := upstreamConfig("")
	cfg.Routes = []config.UpstreamRoute{{Model: "chat", BaseURLs: []string{"127.0.0.1:1"}}}
	handler, err := New(cfg, nil, WithTransport(func(http.RoundTripper) http.RoundTripper {
		return usageTestTransport(func(request *http.Request) (*http.Response, error) {
			if request.Context().Value(contextKey{}) != "retained" {
				t.Error("routing lost request context")
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: request}, nil
		})
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer handler.CloseIdleConnections()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"chat"}`))
	request = request.WithContext(context.WithValue(request.Context(), contextKey{}, "retained"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
