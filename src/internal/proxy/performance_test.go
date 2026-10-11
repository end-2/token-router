package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
			handler, err := New(upstreamConfig("http://upstream.example"), testTransportConfig(), slog.New(slog.DiscardHandler), options...)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(handler.CloseIdleConnections)
			request := httptest.NewRequestWithContext(b.Context(), http.MethodPost, "/v1/chat/completions", nil)
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

type benchmarkResponseWriter struct {
	header http.Header
}

func (writer *benchmarkResponseWriter) Header() http.Header { return writer.header }
func (*benchmarkResponseWriter) WriteHeader(int)            {}
func (*benchmarkResponseWriter) Write(payload []byte) (int, error) {
	return len(payload), nil
}
func (*benchmarkResponseWriter) Flush() {}
