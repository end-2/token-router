package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStreamingPreservesRequestBodyUntilTransportFinishes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	releaseRead := make(chan struct{})
	var releaseOnce sync.Once
	unblockRead := func() { releaseOnce.Do(func() { close(releaseRead) }) }
	readResult := make(chan error, 1)
	finishStream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if _, err := io.Copy(io.Discard, request.Body); err != nil {
			t.Errorf("read upstream body: %v", err)
			return
		}

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
		case <-finishStream:
			if _, err := io.WriteString(writer, "data: done\n\n"); err != nil {
				t.Errorf("write final event: %v", err)
			}
		case <-request.Context().Done():
		}
	}))

	handler := newTestHandler(t, upstreamConfig(upstream.URL))
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		request.Body = &delayedEOFBody{ReadCloser: request.Body, remaining: request.ContentLength, release: releaseRead, result: readResult}
		handler.ServeHTTP(writer, request)
	}))
	defer func() {
		unblockRead()
		cancel()
		server.Close()
		upstream.Close()
	}()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"stream":true}`))
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
	if first, err := reader.ReadString('\n'); err != nil || first != "data: first\n" {
		t.Fatalf("first event was not flushed: event=%q error=%v", first, err)
	}
	// The response has started while the outbound transport still needs its EOF read.
	unblockRead()
	select {
	case err := <-readResult:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("response flush closed the request body before transport finished: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("outbound transport did not finish reading the request body")
	}

	close(finishStream)
	remainder, err := io.ReadAll(reader)
	if err != nil || string(remainder) != "\ndata: done\n\n" {
		t.Fatalf("stream ended prematurely: body=%q error=%v", remainder, err)
	}
}

// delayedEOFBody holds the transport's final EOF read until the first SSE event is flushed.
type delayedEOFBody struct {
	io.ReadCloser
	remaining int64
	release   <-chan struct{}
	result    chan<- error
}

func (body *delayedEOFBody) Read(buffer []byte) (int, error) {
	if body.remaining > 0 {
		n, err := body.ReadCloser.Read(buffer)
		body.remaining -= int64(n)
		return n, err
	}

	<-body.release
	n, err := body.ReadCloser.Read(buffer)
	body.result <- err
	return n, err
}
