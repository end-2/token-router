package proxy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTransportConnectionReuse(t *testing.T) {
	for _, test := range []struct {
		name    string
		disable bool
		want    int32
	}{
		{name: "reuse", want: 1},
		{name: "disable reuse", disable: true, want: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			var connections atomic.Int32
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					connections.Add(1)
				}
			}
			upstream.Start()
			t.Cleanup(upstream.Close)

			cfg := testTransportConfig()
			cfg.DisableKeepAlives = test.disable
			handler := newTestHandlerWithTransport(t, upstreamConfig(upstream.URL), cfg)

			for range 2 {
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil))
				if w.Code != http.StatusNoContent {
					t.Fatalf("unexpected response: %d", w.Code)
				}
			}

			if got := connections.Load(); got != test.want {
				t.Fatalf("opened %d connections, want %d", got, test.want)
			}
		})
	}
}

func TestTransportHTTP2Selection(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Upstream-Protocol", strconv.Itoa(r.ProtoMajor))
				w.WriteHeader(http.StatusNoContent)
			}))
			upstream.EnableHTTP2 = true
			upstream.StartTLS()
			t.Cleanup(upstream.Close)

			cfg := testTransportConfig()
			cfg.ForceAttemptHTTP2 = enabled
			handler := newTestHandlerWithTransport(t, upstreamConfig(upstream.URL), cfg)
			handler.transport.TLSClientConfig = upstream.Client().Transport.(*http.Transport).TLSClientConfig.Clone()

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil))

			want := "1"
			if enabled {
				want = "2"
			}
			if w.Code != http.StatusNoContent || w.Header().Get("X-Upstream-Protocol") != want {
				t.Fatalf("status=%d protocol=%q, want HTTP/%s", w.Code, w.Header().Get("X-Upstream-Protocol"), want)
			}
		})
	}
}

func TestTransportConnectionLimitHonorsRequestDeadline(t *testing.T) {
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		select {
		case <-release:
			w.WriteHeader(http.StatusNoContent)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(finish)

	cfg := testTransportConfig()
	cfg.MaxConnsPerHost = 1
	cfg.ResponseHeaderTimeout = 0
	handler := newTestHandlerWithTransport(t, upstreamConfig(upstream.URL), cfg)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	done := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/models", nil))
		done <- w.Code
	}()
	select {
	case <-arrived:
	case <-ctx.Done():
		t.Fatal("first upstream request did not arrive")
	}

	queuedContext, cancelQueued := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancelQueued()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequestWithContext(queuedContext, http.MethodGet, "/v1/models", nil))

	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("queued request returned %d, want 504", w.Code)
	}
	select {
	case <-arrived:
		t.Fatal("queued request exceeded the connection limit")
	default:
	}

	finish()
	select {
	case status := <-done:
		if status != http.StatusNoContent {
			t.Fatalf("active request returned %d", status)
		}
	case <-ctx.Done():
		t.Fatal("active request did not finish")
	}

	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/models", nil))

	if w.Code != http.StatusNoContent {
		t.Fatalf("connection was not released after completion: %d", w.Code)
	}
}

func TestTransportResponseHeaderLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Padding", strings.Repeat("x", 2048))
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)

	for _, test := range []struct {
		name  string
		limit int64
		want  int
	}{
		{name: "default", want: http.StatusNoContent},
		{name: "custom", limit: 512, want: http.StatusBadGateway},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := testTransportConfig()
			cfg.MaxResponseHeaderBytes = test.limit
			handler := newTestHandlerWithTransport(t, upstreamConfig(upstream.URL), cfg)

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil))

			if w.Code != test.want {
				t.Fatalf("response status=%d, want %d", w.Code, test.want)
			}
		})
	}
}
