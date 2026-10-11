package app

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"token-router/src/internal/bucket"
)

func TestInformationalResponsePreservesFinalStatus(t *testing.T) {
	for _, status := range []int{http.StatusContinue, http.StatusEarlyHints} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			a := testApplication(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Link", "</style.css>; rel=preload")
				writer.WriteHeader(status)

				writer.Header().Del("Link")
				writer.Header().Set("Content-Type", "application/json")
				writer.Header().Set("Retry-After", "10")
				writer.WriteHeader(http.StatusTooManyRequests)
				if _, err := io.WriteString(writer, `{"error":"rate limited"}`); err != nil {
					t.Errorf("write upstream error: %v", err)
				}
			}), &fixedLimiter{decision: bucket.Decision{Allowed: true}})
			server := httptest.NewServer(a.handler())
			defer server.Close()

			informational := make(chan int, 2)
			trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, _ textproto.MIMEHeader) error {
				informational <- code
				return nil
			}}
			request, err := http.NewRequestWithContext(httptrace.WithClientTrace(t.Context(), trace), http.MethodGet, server.URL+"/v1/models", nil)
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

			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}

			if response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") != "10" || string(body) != `{"error":"rate limited"}` {
				t.Fatalf("final response was changed after 1xx: status=%d headers=%v body=%s", response.StatusCode, response.Header, body)
			}

			select {
			case got := <-informational:
				if got != status {
					t.Fatalf("informational response = %d, want %d", got, status)
				}
			default:
				t.Fatal("informational response was not forwarded")
			}

			metrics := httptest.NewRecorder()
			a.metrics.Handler().ServeHTTP(metrics, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
			if !strings.Contains(metrics.Body.String(), `token_router_http_requests_total{method="GET",route="/v1/*",status="429"} 1`) {
				t.Fatal("metrics did not record the final upstream status")
			}
		})
	}
}

func TestProxyResponseWriterPreservesStreamingFlush(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	releaseUpstream := func() { once.Do(func() { close(release) }) }
	defer releaseUpstream()
	a := testApplication(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(writer, "data: first\n\n"); err != nil {
			t.Errorf("write event: %v", err)
			return
		}
		if err := http.NewResponseController(writer).Flush(); err != nil {
			t.Errorf("flush event: %v", err)
			return
		}
		select {
		case <-release:
		case <-request.Context().Done():
		}
	}), &fixedLimiter{decision: bucket.Decision{Allowed: true}})
	server := httptest.NewServer(a.handler())
	defer server.Close()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/v1/responses", nil)
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
		t.Fatalf("stream was not flushed through Echo: event=%q error=%v", first, err)
	}

	releaseUpstream()
}
