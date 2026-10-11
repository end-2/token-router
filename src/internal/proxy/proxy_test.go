package proxy

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"token-router/src/internal/config"
)

func TestForwardRequestAndUpstreamError(t *testing.T) {
	for _, test := range []struct {
		name         string
		apiKey       string
		organization string
		project      string
		wantAuth     string
		wantOrg      string
		wantProject  string
	}{
		{name: "configured credentials", apiKey: "server-secret", organization: "server-org", project: "server-project", wantAuth: "Bearer server-secret", wantOrg: "server-org", wantProject: "server-project"},
		{name: "client credentials", wantAuth: "Bearer client-secret", wantOrg: "client-org", wantProject: "client-project"},
	} {
		t.Run(test.name, func(t *testing.T) {
			type receivedRequest struct {
				method, path, rawPath, query, host, body string
				header                                   http.Header
			}
			received := make(chan receivedRequest, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Errorf("read upstream request: %v", err)
				}
				received <- receivedRequest{request.Method, request.URL.Path, request.URL.RawPath, request.URL.RawQuery, request.Host, string(body), request.Header.Clone()}

				writer.Header().Set("Content-Type", "application/json")
				writer.Header().Set("Retry-After", "42")
				writer.WriteHeader(http.StatusTooManyRequests)
				if _, err := io.WriteString(writer, `{"error":{"message":"quota exhausted"}}`); err != nil {
					t.Errorf("write upstream response: %v", err)
				}
			}))
			defer upstream.Close()

			cfg := upstreamConfig(upstream.URL + "/prefix/")
			cfg.APIKey, cfg.Organization, cfg.Project = test.apiKey, test.organization, test.project
			handler := newTestHandler(t, cfg)

			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://client.invalid/v1/files/a%2Fb/content?foo=one&foo=two&encoding=a%2Bb", strings.NewReader(`{"stream":true}`))
			request.Header.Set("Authorization", "Bearer client-secret")
			request.Header.Set("OpenAI-Organization", "client-org")
			request.Header.Set("OpenAI-Project", "client-project")
			request.Header.Set("Forwarded", "host=attacker.invalid")
			request.Header.Set("X-Forwarded-For", "192.0.2.1")
			request.Header.Set("X-Forwarded-Prefix", "/attacker")
			request.Header.Set("Proxy-Authorization", "Basic proxy-secret")
			request.Header.Set("Connection", "keep-alive, X-Private-Hop")
			request.Header.Set("X-Private-Hop", "hop-secret")

			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "42" || response.Body.String() != `{"error":{"message":"quota exhausted"}}` {
				t.Fatalf("upstream response changed: status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
			}

			got := <-received
			if got.method != http.MethodPost || got.path != "/prefix/v1/files/a/b/content" || got.rawPath != "/prefix/v1/files/a%2Fb/content" || got.query != "foo=one&foo=two&encoding=a%2Bb" || got.body != `{"stream":true}` {
				t.Fatalf("request not preserved: %+v", got)
			}
			if got.host != strings.TrimPrefix(upstream.URL, "http://") {
				t.Errorf("upstream host = %q", got.host)
			}
			for key, want := range map[string]string{"Authorization": test.wantAuth, "OpenAI-Organization": test.wantOrg, "OpenAI-Project": test.wantProject} {
				if got.header.Get(key) != want {
					t.Errorf("%s header did not follow credential configuration", key)
				}
			}
			for _, key := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Prefix", "Proxy-Authorization", "X-Private-Hop"} {
				if got.header.Get(key) != "" {
					t.Errorf("untrusted header %s reached upstream", key)
				}
			}
		})
	}
}

func TestRedirectIsPassedThrough(t *testing.T) {
	var followed atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/redirected" {
			followed.Store(true)
		}
		writer.Header().Set("Location", "/redirected")
		writer.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer upstream.Close()
	handler := newTestHandler(t, upstreamConfig(upstream.URL))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil))

	if response.Code != http.StatusTemporaryRedirect || response.Header().Get("Location") != "/redirected" || followed.Load() {
		t.Fatalf("redirect was not passed through unchanged: status=%d", response.Code)
	}
}

func TestTransportErrorDoesNotExposeSecrets(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	upstream.Close()

	var logs bytes.Buffer
	cfg := upstreamConfig(upstream.URL)
	cfg.APIKey = "private-upstream-key"
	handler, err := New(cfg, testTransportConfig(), slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer handler.CloseIdleConnections()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models?private=query-secret", nil))

	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), `"code":"upstream_error"`) {
		t.Fatalf("unexpected transport error: status=%d body=%s", response.Code, response.Body.String())
	}
	for _, secret := range []string{upstream.URL, cfg.APIKey, "query-secret"} {
		if strings.Contains(response.Body.String()+logs.String(), secret) {
			t.Error("transport failure exposed request details")
		}
	}
}

func TestTimeoutBeforeResponseHeaders(t *testing.T) {
	for _, test := range []struct {
		name          string
		headerTimeout time.Duration
		wholeTimeout  time.Duration
	}{
		{name: "response headers", headerTimeout: 50 * time.Millisecond},
		{name: "whole request", wholeTimeout: 50 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			canceled := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
				<-request.Context().Done()
				close(canceled)
			}))
			defer upstream.Close()

			cfg := upstreamConfig(upstream.URL)
			transport := testTransportConfig()
			transport.ResponseHeaderTimeout = test.headerTimeout
			cfg.RequestTimeout = test.wholeTimeout
			handler := newTestHandlerWithTransport(t, cfg, transport)

			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil))

			if response.Code != http.StatusGatewayTimeout || !strings.Contains(response.Body.String(), `"code":"upstream_timeout"`) {
				t.Fatalf("unexpected timeout: status=%d body=%s", response.Code, response.Body.String())
			}
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("timeout did not cancel upstream request")
			}
		})
	}
}

func TestStreamingFlushesBeforeUpstreamCompletes(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseUpstream()
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(writer, "data: first\n\n"); err != nil {
			t.Errorf("write first event: %v", err)
			return
		}
		if err := http.NewResponseController(writer).Flush(); err != nil {
			t.Errorf("flush first event: %v", err)
			return
		}

		select {
		case <-release:
		case <-request.Context().Done():
			return
		}
		if _, err := io.WriteString(writer, "data: second\n\n"); err != nil {
			t.Errorf("write second event: %v", err)
		}
	}))
	defer upstream.Close()
	handler := newTestHandler(t, upstreamConfig(upstream.URL))
	server := httptest.NewServer(handler)
	defer server.Close()

	client := &http.Client{Timeout: 2 * time.Second}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close response: %v", err)
		}
	}()

	reader := bufio.NewReader(response.Body)
	first, err := reader.ReadString('\n')
	if err != nil || first != "data: first\n" {
		t.Fatalf("first event was not flushed before upstream completed: event=%q error=%v", first, err)
	}

	releaseUpstream()
	remainder, err := io.ReadAll(reader)
	if err != nil || string(remainder) != "\ndata: second\n\n" {
		t.Fatalf("remaining stream changed: body=%q error=%v", remainder, err)
	}
}

func TestStreamingCancellation(t *testing.T) {
	for _, test := range []struct {
		name    string
		timeout time.Duration
	}{
		{name: "client disconnect"},
		{name: "whole request deadline", timeout: 150 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			canceled := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "text/event-stream")
				if _, err := io.WriteString(writer, "data: first\n\n"); err != nil {
					t.Errorf("write stream: %v", err)
					return
				}
				if err := http.NewResponseController(writer).Flush(); err != nil {
					t.Errorf("flush stream: %v", err)
					return
				}
				<-request.Context().Done()
				close(canceled)
			}))
			defer upstream.Close()

			cfg := upstreamConfig(upstream.URL)
			cfg.RequestTimeout = test.timeout
			handler := newTestHandler(t, cfg)
			server := httptest.NewServer(handler)
			defer server.Close()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/responses", nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := (&http.Client{Timeout: 2 * time.Second}).Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := response.Body.Close(); err != nil {
					t.Errorf("close response: %v", err)
				}
			}()

			first, err := bufio.NewReader(response.Body).ReadString('\n')
			if err != nil || first != "data: first\n" {
				t.Fatalf("read first event: event=%q error=%v", first, err)
			}

			if test.timeout == 0 {
				cancel()
			}
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("stream cancellation did not reach upstream")
			}
		})
	}
}

func TestRejectProtocolUpgrade(t *testing.T) {
	for _, test := range []struct {
		name       string
		connection string
		upgrade    string
	}{
		{name: "websocket", connection: "keep-alive, Upgrade", upgrade: "websocket"},
		{name: "upgrade token only", connection: "UPGRADE"},
		{name: "upgrade header only", upgrade: "h2c"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := newTestHandler(t, upstreamConfig("http://127.0.0.1:1"))
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/realtime", nil)
			request.Header.Set("Connection", test.connection)
			request.Header.Set("Upgrade", test.upgrade)

			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"unsupported_upgrade"`) {
				t.Fatalf("upgrade was not rejected: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestRejectUnsolicitedUpstreamUpgrade(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Connection", "Upgrade")
		writer.Header().Set("Upgrade", "websocket")
		writer.WriteHeader(http.StatusSwitchingProtocols)
	}))
	defer upstream.Close()
	handler := newTestHandler(t, upstreamConfig(upstream.URL))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil))

	if response.Code != http.StatusBadGateway {
		t.Fatalf("unsolicited upstream upgrade returned status %d", response.Code)
	}
}

func TestInvalidUpstreamURL(t *testing.T) {
	for _, target := range []string{"", "://bad", "file:///tmp/upstream", "https://:443", "https://user:secret@example.com", "https://example.com?key=secret", "https://example.com?", "https://example.com#fragment"} {
		t.Run(target, func(t *testing.T) {
			if _, err := New(upstreamConfig(target), testTransportConfig(), nil); err == nil {
				t.Fatal("accepted invalid upstream URL")
			}
		})
	}
}

func upstreamConfig(baseURL string) config.Upstream {
	baseURLs := []string{}
	if baseURL != "" {
		baseURLs = append(baseURLs, baseURL)
	}
	cfg := config.Defaults().Upstream
	cfg.Routes = []config.UpstreamRoute{{Host: "*", Model: "*", BaseURLs: baseURLs}}
	return cfg
}

func testTransportConfig() config.Transport {
	cfg := config.Defaults().Transport
	cfg.DialTimeout = time.Second
	cfg.ResponseHeaderTimeout = time.Second
	cfg.IdleConnTimeout = time.Second
	cfg.MaxIdleConns = 10
	cfg.MaxIdleConnsPerHost = 5
	return cfg
}

func newTestHandler(t *testing.T, cfg config.Upstream) *Handler {
	t.Helper()
	return newTestHandlerWithTransport(t, cfg, testTransportConfig())
}

func newTestHandlerWithTransport(t *testing.T, cfg config.Upstream, transport config.Transport) *Handler {
	t.Helper()
	handler, err := New(cfg, transport, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(handler.CloseIdleConnections)
	return handler
}
