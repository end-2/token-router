package usage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httputil"
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
		{name: "negative zero", payload: `{"usage":{"total_tokens":-0}}`, valid: true},
		{name: "whitespace count", payload: `{"usage":{"total_tokens": 12 }}`, want: 12, valid: true},
		{name: "case insensitive keys", payload: `{"USAGE":{"TOTAL_TOKENS":12}}`, want: 12, valid: true},
		{name: "Unicode folded key", payload: `{"uſage":{"total_tokens":12}}`, want: 12, valid: true},
		{name: "escaped usage key", payload: `{"u\u0073age":{"total_tokens":12}}`, want: 12, valid: true},
		{name: "escaped preceding content", payload: `{"content":"a\"b","usage":{"total_tokens":12}}`, want: 12, valid: true},
		{name: "escaped nested usage key", payload: `{"response":{"\u0075sage":{"total_tokens":12}}}`, want: 12, valid: true},
		{name: "quoted usage content", payload: `{"content":"usage"}`},
		{name: "integer precision", payload: `{"usage":{"total_tokens":9007199254740993}}`, want: 9007199254740993, valid: true},
		{name: "maximum integer", payload: `{"usage":{"total_tokens":9223372036854775807}}`, want: math.MaxInt64, valid: true},
		{name: "maximum sum", payload: `{"usage":{"input_tokens":9223372036854775800,"output_tokens":7}}`, want: math.MaxInt64, valid: true},
		{name: "null usage", payload: `{"usage":null}`},
		{name: "null usage before nested usage", payload: `{"usage":null,"response":{"usage":{"total_tokens":12}}}`, want: 12, valid: true},
		{name: "null usage before repeated usage", payload: `{"usage":null,"usage":{"total_tokens":12}}`, want: 12, valid: true},
		{name: "repeated null usage", payload: `{"usage":{"total_tokens":12},"usage":null}`},
		{name: "missing usage", payload: `{"choices":[]}`},
		{name: "incomplete counts", payload: `{"usage":{"input_tokens":9}}`},
		{name: "null count", payload: `{"usage":{"input_tokens":null,"output_tokens":9}}`},
		{name: "negative total", payload: `{"usage":{"total_tokens":-1}}`},
		{name: "negative component", payload: `{"usage":{"prompt_tokens":-1,"completion_tokens":9}}`},
		{name: "fraction", payload: `{"usage":{"total_tokens":1.5}}`},
		{name: "exponent", payload: `{"usage":{"total_tokens":1e2}}`},
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

func TestUsageEventsFragmentBoundaries(t *testing.T) {
	for _, ending := range []string{"\n", "\r", "\r\n"} {
		t.Run(fmt.Sprintf("ending_%q", ending), func(t *testing.T) {
			payload := []byte(strings.Join([]string{
				": keepalive", "event: response.completed", `data: {"response":`,
				`data: {"usage":{"total_tokens":12}}}`, "", "", "data: [DONE]", "", "",
			}, ending))
			for split := 0; split <= len(payload); split++ {
				var got int64
				body := &usageBody{ctx: context.Background(), record: func(_ context.Context, tokens int64) { got += tokens }}
				events := usageEvents{observe: body.observe}
				events.feed(payload[:split])
				events.feed(nil)
				events.feed(payload[split:])
				events.finish()
				if got != 12 {
					t.Fatalf("split %d: usage = %d, want 12", split, got)
				}
			}
		})
	}
}

func TestUsageRecorderChainsModifyResponse(t *testing.T) {
	want := errors.New("reject response")
	reverseProxy := &httputil.ReverseProxy{ModifyResponse: func(*http.Response) error { return want }}
	Recorder(func(context.Context, int64) { t.Error("unexpected usage") })(reverseProxy)
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

type usageTestReader func([]byte) (int, error)

func (reader usageTestReader) Read(buffer []byte) (int, error) {
	return reader(buffer)
}
