package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestResponseUsage(t *testing.T) {
	for _, test := range []struct {
		name      string
		payload   string
		eventType string
		streaming bool
		want      int64
		valid     bool
	}{
		{name: "total preferred", payload: `{"usage":{"total_tokens":12,"prompt_tokens":10,"completion_tokens":20}}`, want: 12, valid: true},
		{name: "chat fallback", payload: `{"usage":{"prompt_tokens":10,"completion_tokens":20}}`, want: 30, valid: true},
		{name: "responses fallback", payload: `{"usage":{"input_tokens":10,"output_tokens":20,"output_tokens_details":{"reasoning_tokens":7}}}`, want: 30, valid: true},
		{name: "null total fallback", payload: `{"usage":{"total_tokens":null,"input_tokens":10,"output_tokens":20}}`, want: 30, valid: true},
		{name: "zero", payload: `{"usage":{"total_tokens":0}}`, valid: true},
		{name: "integer precision", payload: `{"usage":{"total_tokens":9007199254740993}}`, want: 9007199254740993, valid: true},
		{name: "maximum integer", payload: `{"usage":{"total_tokens":9223372036854775807}}`, want: math.MaxInt64, valid: true},
		{name: "maximum sum", payload: `{"usage":{"input_tokens":9223372036854775800,"output_tokens":7}}`, want: math.MaxInt64, valid: true},
		{name: "null usage", payload: `{"usage":null}`},
		{name: "missing usage", payload: `{"choices":[]}`},
		{name: "incomplete counts", payload: `{"usage":{"input_tokens":9}}`},
		{name: "null count", payload: `{"usage":{"input_tokens":null,"output_tokens":9}}`},
		{name: "negative total", payload: `{"usage":{"total_tokens":-1}}`},
		{name: "negative component", payload: `{"usage":{"prompt_tokens":-1,"completion_tokens":9}}`},
		{name: "fraction", payload: `{"usage":{"total_tokens":1.5}}`},
		{name: "string", payload: `{"usage":{"total_tokens":"12"}}`},
		{name: "overflow", payload: `{"usage":{"total_tokens":9223372036854775808}}`},
		{name: "sum overflow", payload: `{"usage":{"input_tokens":9223372036854775807,"output_tokens":1}}`},
		{name: "malformed", payload: `{"usage":{"total_tokens":12}`},
		{name: "multiple JSON values", payload: `{"usage":{"total_tokens":12}} {}`},
		{name: "completed response", payload: `{"type":"response.completed","response":{"usage":{"total_tokens":12}}}`, streaming: true, want: 12, valid: true},
		{name: "incomplete response", payload: `{"type":"response.incomplete","response":{"usage":{"total_tokens":12}}}`, streaming: true, want: 12, valid: true},
		{name: "failed response", payload: `{"type":"response.failed","response":{"usage":{"total_tokens":12}}}`, streaming: true, want: 12, valid: true},
		{name: "event name fallback", payload: `{"response":{"usage":{"total_tokens":12}}}`, eventType: "response.completed", streaming: true, want: 12, valid: true},
		{name: "unfinished response", payload: `{"type":"response.in_progress","response":{"usage":{"total_tokens":12}}}`, streaming: true},
		{name: "untyped nested stream usage", payload: `{"response":{"usage":{"total_tokens":12}}}`, streaming: true},
		{name: "nested JSON usage", payload: `{"response":{"usage":{"total_tokens":12}}}`, want: 12, valid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, valid := responseUsage([]byte(test.payload), test.eventType, test.streaming)
			if got != test.want || valid != test.valid {
				t.Fatalf("responseUsage = (%d, %t), want (%d, %t)", got, valid, test.want, test.valid)
			}
		})
	}
}

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
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(requestBody))
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
			request := httptest.NewRequest(test.method, test.path, nil)
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
			request := httptest.NewRequest(http.MethodPost, test.path, nil)
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

func TestUsageRecorderChainsModifyResponse(t *testing.T) {
	want := errors.New("reject response")
	reverseProxy := &httputil.ReverseProxy{ModifyResponse: func(*http.Response) error { return want }}
	WithUsageRecorder(func(context.Context, int64) { t.Error("unexpected usage") })(reverseProxy)
	if err := reverseProxy.ModifyResponse(&http.Response{}); !errors.Is(err, want) {
		t.Fatalf("previous ModifyResponse error = %v, want %v", err, want)
	}
}

func TestUsageObservationIsBounded(t *testing.T) {
	for _, test := range []struct {
		name      string
		streaming bool
		body      string
		want      int64
	}{
		{name: "oversized JSON", body: `{"padding":"` + strings.Repeat("x", maxUsageObservationBytes) + `","usage":{"total_tokens":12}}`},
		{name: "oversized SSE event followed by usage", streaming: true, body: "data: " + strings.Repeat("x", maxUsageObservationBytes) + "\n\ndata: {\"usage\":{\"total_tokens\":12}}\n\n", want: 12},
	} {
		t.Run(test.name, func(t *testing.T) {
			var got int64
			body := &usageBody{ReadCloser: io.NopCloser(strings.NewReader(test.body)), ctx: context.Background(), record: func(_ context.Context, tokens int64) { got += tokens }}
			if test.streaming {
				body.events = &usageEvents{observe: body.observe}
			}
			forwarded, err := io.ReadAll(body)
			if err != nil || string(forwarded) != test.body {
				t.Fatalf("oversized response changed: error=%v", err)
			}
			if got != test.want {
				t.Fatalf("usage = %d, want %d", got, test.want)
			}
			if len(body.buffer) > maxUsageObservationBytes {
				t.Fatal("JSON observation buffer exceeded limit")
			}
		})
	}
}

func TestUsageRecorderProcessesBytesReturnedWithReadError(t *testing.T) {
	for _, test := range []struct {
		name      string
		streaming bool
		payload   string
		readError error
		want      int64
	}{
		{name: "JSON bytes and EOF", payload: `{"usage":{"total_tokens":17}}`, readError: io.EOF, want: 17},
		{name: "JSON transport error", payload: `{"usage":{"total_tokens":17}}`, readError: io.ErrUnexpectedEOF, want: 17},
		{name: "JSON client cancellation", payload: `{"usage":{"total_tokens":17}}`, readError: context.Canceled, want: 17},
		{name: "JSON partial body at transport error", payload: `{"usage":{"total_tokens":17`, readError: io.ErrUnexpectedEOF},
		{name: "SSE bytes and EOF", streaming: true, payload: "data: {\"usage\":{\"total_tokens\":17}}\n\n", readError: io.EOF, want: 17},
		{name: "SSE event before cancellation", streaming: true, payload: "data: {\"usage\":{\"total_tokens\":17}}\n\n", readError: context.Canceled, want: 17},
		{name: "SSE partial event at cancellation", streaming: true, payload: "data: {\"usage\":{\"total_tokens\":17", readError: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			var got int64
			body := &usageBody{
				ReadCloser: io.NopCloser(usageTestReader(func(buffer []byte) (int, error) { return copy(buffer, test.payload), test.readError })),
				ctx:        context.Background(),
				record:     func(_ context.Context, tokens int64) { got += tokens },
			}
			if test.streaming {
				body.events = &usageEvents{observe: body.observe}
			}
			buffer := make([]byte, 1024)
			n, err := body.Read(buffer)
			if string(buffer[:n]) != test.payload || !errors.Is(err, test.readError) {
				t.Fatalf("read result changed: bytes=%q error=%v", buffer[:n], err)
			}
			if got != test.want {
				t.Fatalf("usage = %d, want %d", got, test.want)
			}
			if err := body.Close(); err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("close charged incomplete usage: %d", got)
			}
		})
	}
}

func TestUsageRecorderCloseFinalizesReceivedJSON(t *testing.T) {
	for _, test := range []struct {
		name      string
		streaming bool
		payload   string
		want      int64
	}{
		{name: "complete JSON before EOF", payload: `{"usage":{"total_tokens":17}}`, want: 17},
		{name: "partial JSON", payload: `{"usage":{"total_tokens":17`},
		{name: "SSE without event terminator", streaming: true, payload: "data: {\"usage\":{\"total_tokens\":17}}"},
		{name: "SSE complete event", streaming: true, payload: "data: {\"usage\":{\"total_tokens\":17}}\n\n", want: 17},
	} {
		t.Run(test.name, func(t *testing.T) {
			var got int64
			body := &usageBody{
				ReadCloser: io.NopCloser(strings.NewReader(test.payload)),
				ctx:        context.Background(),
				record:     func(_ context.Context, tokens int64) { got += tokens },
			}
			if test.streaming {
				body.events = &usageEvents{observe: body.observe}
			}
			buffer := make([]byte, len(test.payload))
			n, err := body.Read(buffer)
			if n != len(test.payload) || err != nil {
				t.Fatalf("read = %d, %v; expected full payload without EOF", n, err)
			}
			for range 2 {
				if err := body.Close(); err != nil {
					t.Fatal(err)
				}
				if got != test.want {
					t.Fatalf("usage after close = %d, want %d", got, test.want)
				}
			}
		})
	}
}

func TestUsageRecorderCloseUnblocksRead(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() {
		if err := writer.Close(); err != nil {
			t.Errorf("close pipe writer: %v", err)
		}
	})
	const payload = `{"usage":{"total_tokens":17}}`
	body := &usageBody{ReadCloser: reader, ctx: context.Background()}
	recorded := make(chan int64, 1)
	body.record = func(_ context.Context, tokens int64) { recorded <- tokens }
	readDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, body)
		readDone <- err
	}()
	if _, err := io.WriteString(writer, payload); err != nil {
		t.Fatal(err)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- body.Close() }()
	for _, result := range []<-chan error{closeDone, readDone} {
		select {
		case err := <-result:
			if err != nil && !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("close or read failed: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("closing observer did not unblock read")
		}
	}
	select {
	case got := <-recorded:
		if got != 17 {
			t.Fatalf("recorded usage = %d, want 17", got)
		}
	default:
		t.Fatal("close lost complete JSON usage")
	}
}

func TestUsageRecorderObservesReadBytesArrivingAfterClose(t *testing.T) {
	readStarted := make(chan struct{})
	releaseRead := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseRead) })
	t.Cleanup(release)
	recorded := make(chan int64, 1)
	body := &usageBody{
		ReadCloser: io.NopCloser(usageTestReader(func(buffer []byte) (int, error) {
			close(readStarted)
			<-releaseRead
			return copy(buffer, `{"usage":{"total_tokens":17}}`), context.Canceled
		})),
		ctx:    context.Background(),
		record: func(_ context.Context, tokens int64) { recorded <- tokens },
	}
	readDone := make(chan error, 1)
	go func() {
		_, err := body.Read(make([]byte, 128))
		readDone <- err
	}()
	<-readStarted
	closeDone := make(chan error, 1)
	go func() { closeDone <- body.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("close waited for observer read")
	}
	release()
	select {
	case err := <-readDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("read error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("read did not finish after release")
	}
	select {
	case got := <-recorded:
		if got != 17 {
			t.Fatalf("recorded usage = %d, want 17", got)
		}
	default:
		t.Fatal("read bytes arriving after close lost usage")
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

type usageTestReader func([]byte) (int, error)

func (reader usageTestReader) Read(buffer []byte) (int, error) {
	return reader(buffer)
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
