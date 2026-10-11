package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestUsageRecorderPreservesProxyExchange(t *testing.T) {
	for _, test := range []struct {
		name        string
		contentType string
		body        string
		want        []int64
	}{
		{name: "JSON", contentType: "application/json", body: `{"id":"response-1","usage":{"total_tokens":31}}`, want: []int64{31}},
		{name: "chat SSE", contentType: "text/event-stream; charset=utf-8", body: "data: {\"usage\":null}\n\ndata: {\"usage\":{\"total_tokens\":12}}\n\ndata: {\"usage\":{\"total_tokens\":12}}\n\ndata: {\"usage\":{\"total_tokens\":10}}\n\ndata: {\"usage\":{\"total_tokens\":19}}\n\ndata: [DONE]\n\n", want: []int64{12, 7}},
		{name: "responses SSE fragmented lines", contentType: "text/event-stream", body: ": keepalive\r\nevent: response.in_progress\r\ndata: {\"response\":{\"usage\":{\"total_tokens\":99}}}\r\n\r\nevent: response.completed\r\ndata: {\"response\":\r\ndata: {\"usage\":{\"input_tokens\":13,\"output_tokens\":7}}}\r\n\r\n", want: []int64{20}},
		{name: "SSE CR endings", contentType: "text/event-stream", body: "data: broken\r\rdata: {\"usage\":{\"total_tokens\":12}}\r\r", want: []int64{12}},
		{name: "SSE final event at EOF", contentType: "text/event-stream", body: "data: {\"usage\":{\"total_tokens\":12}}", want: []int64{12}},
	} {
		t.Run(test.name, func(t *testing.T) {
			const requestBody = `{"model":"example","stream":true,"stream_options":{"include_usage":true}}`
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				body, err := io.ReadAll(request.Body)
				if err != nil || string(body) != requestBody {
					t.Errorf("request body changed: %q, error: %v", body, err)
				}
				if request.Header.Get("Accept-Encoding") != "identity" {
					t.Errorf("Accept-Encoding = %q", request.Header.Get("Accept-Encoding"))
				}
				if request.URL.Path != "/prefix/v1/responses" || request.Header.Get("Authorization") != "Bearer server-key" {
					t.Errorf("upstream rewrite changed: path=%s authorization=%q", request.URL.Path, request.Header.Get("Authorization"))
				}
				writer.Header().Set("Content-Type", test.contentType)
				writer.Header().Set("X-Request-ID", "upstream-id")
				writer.WriteHeader(http.StatusCreated)
				if _, err := io.WriteString(writer, test.body); err != nil {
					t.Errorf("write response: %v", err)
				}
			}))
			defer upstream.Close()
			var got []int64
			cfg := upstreamConfig(upstream.URL + "/prefix")
			cfg.APIKey = "server-key"
			type contextKey struct{}
			handler, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)),
				WithUsageRecorder(func(ctx context.Context, tokens int64) {
					if ctx.Value(contextKey{}) != "request-value" {
						t.Error("usage callback lost request context")
					}
					got = append(got, tokens)
				}),
				WithTransport(func(transport http.RoundTripper) http.RoundTripper {
					return usageTestTransport(func(request *http.Request) (*http.Response, error) {
						response, err := transport.RoundTrip(request)
						if err == nil {
							response.Body = &fragmentedUsageBody{ReadCloser: response.Body, chunk: 3}
						}
						return response, err
					})
				}),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer handler.CloseIdleConnections()
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", strings.NewReader(requestBody))
			request = request.WithContext(context.WithValue(request.Context(), contextKey{}, "request-value"))
			request.Header.Set("Accept-Encoding", "gzip")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusCreated || response.Header().Get("X-Request-ID") != "upstream-id" || response.Body.String() != test.body {
				t.Fatalf("response changed: status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("usage increments = %v, want %v", got, test.want)
			}
		})
	}
}

func TestUsageRecorderIgnoresIneligibleResponses(t *testing.T) {
	var compressed bytes.Buffer
	compressor := gzip.NewWriter(&compressed)
	if _, err := io.WriteString(compressor, `{"usage":{"total_tokens":12}}`); err != nil {
		t.Fatal(err)
	}
	if err := compressor.Close(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name         string
		method       string
		path         string
		basePath     string
		status       int
		encoding     string
		wantEncoding string
		body         string
	}{
		{name: "GET stored response", method: http.MethodGet, path: "/v1/responses/resp_123", status: http.StatusOK, wantEncoding: "gzip", body: `{"usage":{"total_tokens":12}}`},
		{name: "POST completion metadata", method: http.MethodPost, path: "/v1/chat/completions/chatcmpl_123", status: http.StatusOK, wantEncoding: "gzip", body: `{"usage":{"total_tokens":12}}`},
		{name: "unknown POST", method: http.MethodPost, path: "/v1/custom", status: http.StatusOK, wantEncoding: "gzip", body: `{"usage":{"total_tokens":12}}`},
		{name: "generation path after base rewrite", method: http.MethodPost, path: "/responses", basePath: "/v1", status: http.StatusOK, wantEncoding: "gzip", body: `{"usage":{"total_tokens":12}}`},
		{name: "upstream error", method: http.MethodPost, path: "/v1/responses", status: http.StatusBadRequest, wantEncoding: "identity", body: `{"usage":{"total_tokens":12}}`},
		{name: "compressed response", method: http.MethodPost, path: "/v1/responses", status: http.StatusOK, encoding: "gzip", wantEncoding: "identity", body: compressed.String()},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if got := request.Header.Get("Accept-Encoding"); got != test.wantEncoding {
					t.Errorf("Accept-Encoding = %q, want %q", got, test.wantEncoding)
				}
				if request.URL.Path != test.basePath+test.path {
					t.Errorf("upstream path = %q, want %q", request.URL.Path, test.basePath+test.path)
				}
				writer.Header().Set("Content-Type", "application/json")
				if test.encoding != "" {
					writer.Header().Set("Content-Encoding", test.encoding)
				}
				writer.WriteHeader(test.status)
				if _, err := io.WriteString(writer, test.body); err != nil {
					t.Errorf("write response: %v", err)
				}
			}))
			defer upstream.Close()
			handler := usageTestHandler(t, upstream.URL+test.basePath, func(context.Context, int64) { t.Error("ineligible response recorded usage") })
			request := httptest.NewRequestWithContext(t.Context(), test.method, test.path, nil)
			request.Header.Set("Accept-Encoding", "gzip")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status || response.Body.String() != test.body || response.Header().Get("Content-Encoding") != test.encoding {
				t.Fatal("ineligible response changed")
			}
		})
	}
}

func TestUsageRecorderGenerationEndpoints(t *testing.T) {
	for _, test := range []struct {
		path string
		body string
		want int64
	}{
		{path: "/v1/completions", body: `{"usage":{"prompt_tokens":10,"completion_tokens":2}}`, want: 12},
		{path: "/v1/responses/compact", body: `{"usage":{"input_tokens":8,"output_tokens":4}}`, want: 12},
		{path: "/v1/embeddings", body: `{"usage":{"prompt_tokens":12,"total_tokens":12}}`, want: 12},
		{path: "/v1/images/generations", body: `{"usage":{"input_tokens":8,"output_tokens":4}}`, want: 12},
		{path: "/v1/audio/transcriptions", body: `{"usage":{"total_tokens":12}}`, want: 12},
		{path: "/v1/audio/speech", body: `{"usage":{"type":"duration","seconds":12}}`},
	} {
		t.Run(test.path, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Accept-Encoding") != "identity" {
					t.Errorf("generation request Accept-Encoding = %q", request.Header.Get("Accept-Encoding"))
				}
				writer.Header().Set("Content-Type", "application/json")
				if _, err := io.WriteString(writer, test.body); err != nil {
					t.Errorf("write response: %v", err)
				}
			}))
			defer upstream.Close()
			var got int64
			handler := usageTestHandler(t, upstream.URL, func(_ context.Context, tokens int64) { got += tokens })
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, test.path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK || response.Body.String() != test.body {
				t.Fatal("generation response changed")
			}
			if got != test.want {
				t.Fatalf("recorded tokens = %d, want %d", got, test.want)
			}
		})
	}
}

func TestUsageRecorderStreamsAndRecordsBeforeCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	releaseUsage := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(writer, "data: {\"choices\":[],\"usage\":null}\n\n"); err != nil {
			t.Errorf("write content event: %v", err)
			return
		}
		if err := http.NewResponseController(writer).Flush(); err != nil {
			t.Errorf("flush content event: %v", err)
			return
		}
		select {
		case <-releaseUsage:
		case <-request.Context().Done():
			close(upstreamCanceled)
			return
		}
		if _, err := io.WriteString(writer, "data: {\"usage\":{\"total_tokens\":17}}\n\n"); err != nil {
			t.Errorf("write usage event: %v", err)
			return
		}
		if err := http.NewResponseController(writer).Flush(); err != nil {
			t.Errorf("flush usage event: %v", err)
			return
		}
		<-request.Context().Done()
		close(upstreamCanceled)
	}))
	recorded := make(chan int64, 1)
	handler := usageTestHandler(t, upstream.URL, func(_ context.Context, tokens int64) { recorded <- tokens })
	server := httptest.NewServer(handler)
	defer func() {
		cancel()
		server.Close()
		upstream.Close()
	}()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(request)
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
	if err != nil || first != "data: {\"choices\":[],\"usage\":null}\n" {
		t.Fatalf("first event did not arrive before usage: event=%q error=%v", first, err)
	}
	close(releaseUsage)
	select {
	case got := <-recorded:
		if got != 17 {
			t.Fatalf("usage = %d, want 17", got)
		}
	case <-ctx.Done():
		t.Fatal("usage was not recorded while stream remained open")
	}
	cancel()
	select {
	case <-upstreamCanceled:
	case <-time.After(time.Second):
		t.Fatal("client cancellation did not cancel upstream")
	}
}

func usageTestHandler(t *testing.T, upstreamURL string, record func(context.Context, int64)) *Handler {
	t.Helper()
	handler, err := New(upstreamConfig(upstreamURL), slog.New(slog.NewTextHandler(io.Discard, nil)), WithUsageRecorder(record))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(handler.CloseIdleConnections)
	return handler
}

type usageTestTransport func(*http.Request) (*http.Response, error)

func (transport usageTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

type fragmentedUsageBody struct {
	io.ReadCloser
	chunk int
}

func (body *fragmentedUsageBody) Read(buffer []byte) (int, error) {
	if len(buffer) > body.chunk {
		buffer = buffer[:body.chunk]
	}
	return body.ReadCloser.Read(buffer)
}
