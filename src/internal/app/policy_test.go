package app

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"token-router/src/internal/bucket/backend"
	"token-router/src/internal/config"
)

func policyApplication(t *testing.T, upstream http.Handler, configure ...func(*config.Config)) *application {
	t.Helper()
	cfg := config.Defaults()
	cfg.RateLimit.RequestsPerSecond, cfg.RateLimit.Burst = 1000, 1000
	for _, f := range configure {
		f(&cfg)
	}
	l, err := backend.New(t.Context(), cfg.RateLimit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := l.Close(); err != nil {
			t.Error(err)
		}
	})
	a := testApplication(t, upstream, l, func(c *config.Config) { *c = cfg })
	t.Cleanup(func() { a.proxy.retire() })
	return a
}

func adminRequest(t *testing.T, a *application, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	a.adminHandler().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), method, "/config", strings.NewReader(body)))
	return w
}

func TestAdminUpdatesPersistAndRejectInvalidChanges(t *testing.T) {
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "policy.yaml")
			writePolicyTestFile(t, file, "{}")
			a := policyApplication(t, http.NotFoundHandler(), func(c *config.Config) {
				c.Policy.File = file
				c.RateLimit.Backend = backend
				if backend == "redis" {
					c.RateLimit.Redis.Address = miniredis.RunT(t).Addr()
				}
			})
			response := adminRequest(t, a, http.MethodPatch, `{"upstream":{"base_urls":["localhost:9001"],"request_timeout":"2s"},"rate_limit":{"burst":3,"tokens_per_second":2,"token_burst":5}}`)
			if response.Code != http.StatusOK {
				t.Fatalf("update: %d %s", response.Code, response.Body)
			}
			snapshot := a.snapshot()
			data, err := config.ReadPolicyFile(file)
			if err != nil {
				t.Fatal(err)
			}
			persisted, err := config.DecodePolicy(data, config.Defaults().Mutable())
			if err != nil || !reflect.DeepEqual(snapshot, persisted) {
				t.Fatalf("file differs from active settings: %v", err)
			}
			if snapshot.RateLimit.Burst != 3 || snapshot.Upstream.RequestTimeout != 2*time.Second {
				t.Fatal("policy values did not change")
			}
			for _, body := range []string{
				`{"rate_limit":{"backend":"redis"}}`, `{"rate_limit":{"enabled":false}}`,
				`{"upstream":{"base_urls":["bad-secret"]},"rate_limit":{"burst":10}}`,
				`{"rate_limit":{"tokens_per_second":0}}`, `{"upstream":{"api_key":null}}`,
				`{"upstream":{"transport":{"max_conns_per_host":-1}}}`,
				`{"upstream":{"transport":{"tls_handshake_timeout":"-1s"}}}`,
				`{"transport":{"max_conns_per_host":7}}`,
			} {
				response = adminRequest(t, a, http.MethodPatch, body)
				if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), "bad-secret") {
					t.Fatalf("invalid update: %d %s", response.Code, response.Body)
				}
				if !reflect.DeepEqual(snapshot, a.snapshot()) {
					t.Fatal("invalid update changed active settings")
				}
			}
			unchanged, err := config.ReadPolicyFile(file)
			if err != nil || string(unchanged) != string(data) {
				t.Fatal("invalid update changed the file")
			}
			get := adminRequest(t, a, http.MethodGet, "")
			if get.Code != http.StatusOK || strings.Contains(get.Body.String(), "backend") || strings.Contains(get.Body.String(), "transport") || get.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("GET did not return mutable settings")
			}
			if response := adminRequest(t, a, http.MethodPut, get.Body.String()); response.Code != http.StatusOK {
				t.Fatalf("GET/PUT round trip: %s", response.Body)
			}
		})
	}
}

func TestAdminPersistenceFailureAndBodyLimit(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "policy.yaml")
	writePolicyTestFile(t, file, "{}")
	link := filepath.Join(dir, "link.yaml")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	a := policyApplication(t, http.NotFoundHandler(), func(c *config.Config) { c.Policy.File = link })
	before := a.snapshot()
	if w := adminRequest(t, a, http.MethodPatch, `{"rate_limit":{"burst":5}}`); w.Code != http.StatusInternalServerError {
		t.Fatalf("expected persistence failure: %d %s", w.Code, w.Body)
	}
	if !reflect.DeepEqual(before, a.snapshot()) {
		t.Fatal("failed persistence changed active state")
	}
	if w := adminRequest(t, a, http.MethodPatch, strings.Repeat("x", config.MaxPolicyBytes+1)); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body returned %d", w.Code)
	}
	a.ready.Store(false)
	if w := adminRequest(t, a, http.MethodPatch, "{}"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("update during shutdown returned %d", w.Code)
	}
}

func TestPolicyReplacementPreservesStartupTransport(t *testing.T) {
	a := policyApplication(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Padding", strings.Repeat("x", 2048))
		w.WriteHeader(http.StatusNoContent)
	}), func(c *config.Config) { c.Transport.MaxResponseHeaderBytes = 512 })
	for _, phase := range []string{"startup", "after policy replacement"} {
		t.Run(phase, func(t *testing.T) {
			if phase == "after policy replacement" {
				before := a.proxy
				response := adminRequest(t, a, http.MethodPatch, `{"upstream":{"api_key":"replacement-key"}}`)
				if response.Code != http.StatusOK || a.proxy == before {
					t.Fatalf("policy did not replace proxy: %d", response.Code)
				}
			}
			w := httptest.NewRecorder()
			a.handler().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil))
			if w.Code != http.StatusBadGateway {
				t.Fatalf("startup response header limit was not applied: %d", w.Code)
			}
		})
	}
}

func writePolicyTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestPolicyWatcherReplacementRecoveryAndSymlink(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "policy.yaml")
	writePolicyTestFile(t, file, "rate_limit: {burst: 2}")
	a := policyApplication(t, http.NotFoundHandler(), func(c *config.Config) {
		c.Policy.File, c.Policy.WatchInterval = file, 5*time.Millisecond
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); a.watchPolicy(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	eventually(t, func() bool { return a.snapshot().RateLimit.Burst == 2 })
	replacement := filepath.Join(dir, "replacement.yaml")
	writePolicyTestFile(t, replacement, "rate_limit: {burst: 3}")
	if err := os.Rename(replacement, file); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return a.snapshot().RateLimit.Burst == 3 })
	writePolicyTestFile(t, file, "rate_limit: {burst: -1}")
	if err := a.reloadPolicy(); err == nil || a.snapshot().RateLimit.Burst != 3 {
		t.Fatal("invalid file replaced active configuration")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := a.reloadPolicy(); err == nil || a.snapshot().RateLimit.Burst != 3 {
		t.Fatal("missing file replaced active configuration")
	}
	writePolicyTestFile(t, replacement, "rate_limit: {burst: 4}")
	if err := os.Symlink(replacement, file); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return a.snapshot().RateLimit.Burst == 4 })
	other := filepath.Join(dir, "other.yaml")
	writePolicyTestFile(t, other, "rate_limit: {burst: 5}")
	temporaryLink := filepath.Join(dir, "link.yaml")
	if err := os.Symlink(other, temporaryLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporaryLink, file); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return a.snapshot().RateLimit.Burst == 5 })
}

func TestPolicySwapPreservesStreamAndAccountsUsage(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	a := policyApplication(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(w, "data: first\n\n"); err != nil {
			t.Error(err)
			return
		}
		w.(http.Flusher).Flush()
		select {
		case <-release:
			if _, err := io.WriteString(w, "data: {\"usage\":{\"total_tokens\":25}}\n\n"); err != nil {
				t.Error(err)
			}
		case <-r.Context().Done():
		}
	}), func(c *config.Config) { c.RateLimit.TokensPerSecond, c.RateLimit.TokenBurst = 0.001, 10 })
	server := httptest.NewServer(a.handler())
	t.Cleanup(server.Close)
	t.Cleanup(finish)
	client := &http.Client{Timeout: 5 * time.Second}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	reader := bufio.NewReader(response.Body)
	if first, err := reader.ReadString('\n'); err != nil || first != "data: first\n" {
		t.Fatalf("stream did not start: %q %v", first, err)
	}
	next := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := io.WriteString(w, "new upstream"); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(next.Close)
	updated := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		updated <- adminRequest(t, a, http.MethodPatch, fmt.Sprintf(`{"upstream":{"base_urls":[%q]},"rate_limit":{"token_burst":20}}`, next.URL))
	}()
	select {
	case w := <-updated:
		if w.Code != http.StatusOK {
			t.Fatalf("update: %s", w.Body)
		}
	case <-time.After(time.Second):
		t.Fatal("open stream blocked configuration update")
	}
	w := httptest.NewRecorder()
	a.handler().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil))
	if w.Code != http.StatusOK || w.Body.String() != "new upstream" {
		t.Fatal("new request did not use replacement upstream")
	}
	finish()
	if tail, err := io.ReadAll(reader); err != nil || !strings.Contains(string(tail), "total_tokens") {
		t.Fatalf("old stream did not finish: %q %v", tail, err)
	}
	eventually(t, func() bool {
		w := httptest.NewRecorder()
		a.handler().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil))
		return w.Code == http.StatusTooManyRequests
	})
}

func TestConcurrentAdminFileAndProxyRequests(t *testing.T) {
	file := filepath.Join(t.TempDir(), "policy.yaml")
	writePolicyTestFile(t, file, "{}")
	a := policyApplication(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }), func(c *config.Config) { c.Policy.File = file })
	handler := a.handler()
	var wg sync.WaitGroup
	for i := range 30 {
		wg.Go(func() {
			w := adminRequest(t, a, http.MethodPatch, fmt.Sprintf(`{"upstream":{"request_timeout":"%ds"},"rate_limit":{"burst":%d}}`, i+1, i+1))
			if w.Code != http.StatusOK {
				t.Errorf("admin status %d", w.Code)
			}
			if err := a.reloadPolicy(); err != nil {
				t.Error(err)
			}
			w = httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil))
			if w.Code != http.StatusNoContent && w.Code != http.StatusTooManyRequests {
				t.Errorf("proxy status %d", w.Code)
			}
		})
	}
	wg.Wait()
}

func TestAdminBindFailureClosesPublicListener(t *testing.T) {
	occupied, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := occupied.Close(); err != nil {
			t.Error(err)
		}
	})
	available, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := available.Addr().String()
	if err := available.Close(); err != nil {
		t.Fatal(err)
	}
	a := policyApplication(t, http.NotFoundHandler(), func(c *config.Config) {
		c.Server.Address = address
		c.Admin.Enabled, c.Admin.Address = true, occupied.Addr().String()
	})
	if err := a.serve(t.Context()); err == nil {
		t.Fatal("occupied admin address did not fail startup")
	}
	reopened, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", address)
	if err != nil {
		t.Fatalf("public listener leaked: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}
