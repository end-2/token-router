package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestResponseFlushingPolicy(t *testing.T) {
	for _, test := range []struct {
		name, contentType string
		contentLength     int64
		wantFlush         bool
	}{
		{name: "known JSON", contentType: "application/json", contentLength: 2},
		{name: "unknown JSON", contentType: "application/json", contentLength: -1, wantFlush: true},
		{name: "known SSE", contentType: "text/event-stream", contentLength: 2, wantFlush: true},
		{name: "unknown SSE", contentType: "text/event-stream; charset=utf-8", contentLength: -1, wantFlush: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, err := New(upstreamConfig("http://upstream.example"), nil, WithTransport(func(http.RoundTripper) http.RoundTripper {
				return usageTestTransport(func(request *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {test.contentType}},
						ContentLength: test.contentLength, Body: io.NopCloser(strings.NewReader("ok")), Request: request}, nil
				})
			}))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(handler.CloseIdleConnections)
			writer := &flushCountingWriter{header: make(http.Header)}
			handler.ServeHTTP(writer, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil))
			if got := writer.flushes.Load() > 0; got != test.wantFlush {
				t.Fatalf("flushed = %t, want %t", got, test.wantFlush)
			}
			if writer.payload.String() != "ok" {
				t.Fatalf("response = %q", writer.payload.String())
			}
		})
	}
}

type flushCountingWriter struct {
	header  http.Header
	payload strings.Builder
	flushes atomic.Int64
}

func (writer *flushCountingWriter) Header() http.Header { return writer.header }
func (*flushCountingWriter) WriteHeader(int)            {}
func (writer *flushCountingWriter) Write(payload []byte) (int, error) {
	return writer.payload.Write(payload)
}
func (writer *flushCountingWriter) Flush() { writer.flushes.Add(1) }
