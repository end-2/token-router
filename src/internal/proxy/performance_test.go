package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"token-router/src/internal/config"
)

func BenchmarkProxyForward(b *testing.B) {
	for _, test := range []struct {
		name, contentType, payload string
		usage                      bool
	}{
		{name: "JSON", contentType: "application/json", payload: `{"output":"` + strings.Repeat("x", 4096) + `"}`},
		{name: "JSONUsage", contentType: "application/json", payload: `{"output":"` + strings.Repeat("x", 4096) + `","usage":{"total_tokens":12}}`, usage: true},
		{name: "SSEUsage", contentType: "text/event-stream", payload: strings.Repeat("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n", 128) + "data: {\"usage\":{\"total_tokens\":128}}\n\n", usage: true},
	} {
		b.Run(test.name, func(b *testing.B) {
			options := []Option{WithTransport(func(http.RoundTripper) http.RoundTripper {
				return usageTestTransport(func(request *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {test.contentType}},
						ContentLength: int64(len(test.payload)), Body: io.NopCloser(strings.NewReader(test.payload)), Request: request}, nil
				})
			})}
			if test.usage {
				options = append(options, WithUsageRecorder(func(context.Context, int64) {}))
			}
			handler, err := New(upstreamConfig("http://upstream.example"), slog.New(slog.DiscardHandler), options...)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(handler.CloseIdleConnections)
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			request.Header.Set("Authorization", "Bearer benchmark")
			request.Header.Set("Content-Type", "application/json")
			writer := &benchmarkResponseWriter{header: make(http.Header)}
			b.SetBytes(int64(len(test.payload)))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				clear(writer.header)
				handler.ServeHTTP(writer, request)
			}
		})
	}
}

func BenchmarkRouting(b *testing.B) {
	for _, test := range []struct {
		name, payload string
		routes        []config.UpstreamRoute
	}{
		{name: "Default"},
		{name: "Host", routes: []config.UpstreamRoute{{Host: "api.example.com", BaseURLs: []string{"http://upstream.example"}}}},
		{name: "Model64KiB", payload: `{"model":"chat","input":"` + strings.Repeat("x", 64<<10) + `"}`, routes: []config.UpstreamRoute{{Model: "chat", BaseURLs: []string{"http://upstream.example"}}}},
	} {
		b.Run(test.name, func(b *testing.B) {
			cfg := upstreamConfig("http://upstream.example")
			cfg.Routes = test.routes
			routing, err := newRouter(cfg)
			if err != nil {
				b.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "http://api.example.com/v1/responses", nil)
			request.Header.Set("Content-Type", "application/json")
			request.ContentLength = int64(len(test.payload))
			reader := strings.NewReader(test.payload)
			body := io.NopCloser(reader)
			writer := httptest.NewRecorder()
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				reader.Reset(test.payload)
				request.Body = body
				if _, routeErr := routing.selectTarget(writer, request); routeErr != nil {
					b.Fatal(routeErr)
				}
			}
		})
	}
}

func BenchmarkUsageEvents(b *testing.B) {
	for _, test := range []struct{ name, payload string }{
		{name: "Content", payload: "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"},
		{name: "NullUsage", payload: "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}],\"usage\":null}\n\n"},
		{name: "Usage", payload: "data: {\"usage\":{\"total_tokens\":12}}\n\n"},
	} {
		b.Run(test.name, func(b *testing.B) {
			body := &usageBody{ctx: context.Background(), record: func(context.Context, int64) {}}
			events := usageEvents{observe: body.observe}
			payload := []byte(test.payload)
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				events.feed(payload)
			}
		})
	}
}

type benchmarkResponseWriter struct {
	header http.Header
}

func (writer *benchmarkResponseWriter) Header() http.Header { return writer.header }
func (*benchmarkResponseWriter) WriteHeader(int)            {}
func (*benchmarkResponseWriter) Write(payload []byte) (int, error) {
	return len(payload), nil
}
func (*benchmarkResponseWriter) Flush() {}
