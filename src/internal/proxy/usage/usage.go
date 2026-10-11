// Package usage observes OpenAI token counts in JSON and SSE responses.
package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"mime"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"
)

// Observation is bounded independently of the response forwarded to the client.
const maxUsageObservationBytes = 8 << 20

// Recorder observes OpenAI usage in successful generation POST responses.
// The callback receives positive increases in reported total tokens and the upstream
// request context. Request bodies and response bytes pass through unchanged.
func Recorder(record func(context.Context, int64)) func(*httputil.ReverseProxy) {
	return func(proxy *httputil.ReverseProxy) {
		if record == nil {
			return
		}

		rewrite := proxy.Rewrite
		proxy.Rewrite = func(request *httputil.ProxyRequest) {
			eligible := request.In.Method == http.MethodPost && usageGenerationPath(request.In.URL.Path)
			if rewrite != nil {
				rewrite(request)
			}
			if eligible {
				request.Out.Header.Set("Accept-Encoding", "identity")
				request.Out = request.Out.WithContext(context.WithValue(request.Out.Context(), usageEligibleKey{}, true))
			}
		}

		modifyResponse := proxy.ModifyResponse
		proxy.ModifyResponse = func(response *http.Response) error {
			if modifyResponse != nil {
				if err := modifyResponse(response); err != nil {
					return err
				}
			}

			if response.Request == nil || response.Request.Method != http.MethodPost ||
				response.Request.Context().Value(usageEligibleKey{}) != true ||
				response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices || response.Body == nil {
				return nil
			}
			encoding := strings.TrimSpace(response.Header.Get("Content-Encoding"))
			if encoding != "" && !strings.EqualFold(encoding, "identity") {
				return nil
			}

			contentType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
			if err != nil {
				contentType = ""
			}

			body := &usageBody{ReadCloser: response.Body, ctx: response.Request.Context(), record: record}
			if strings.EqualFold(contentType, "text/event-stream") {
				body.events = &usageEvents{observe: body.observe}
			}
			response.Body = body
			return nil
		}
	}
}

type usageEligibleKey struct{}

// Stored completion updates can return historical usage. Match generation paths
// before adding the upstream base path so those updates are not charged again.
func usageGenerationPath(path string) bool {
	switch path {
	case "/v1/chat/completions", "/v1/completions", "/v1/responses", "/v1/responses/compact",
		"/v1/embeddings", "/v1/images/generations", "/v1/images/edits", "/v1/images/variations",
		"/v1/audio/speech", "/v1/audio/transcriptions", "/v1/audio/translations":
		return true
	default:
		return false
	}
}

type usageBody struct {
	io.ReadCloser
	mu       sync.Mutex
	ctx      context.Context
	record   func(context.Context, int64)
	events   *usageEvents
	buffer   []byte
	tooLarge bool
	finished bool
	closed   bool
	recorded int64
}

func (body *usageBody) Read(buffer []byte) (int, error) {
	n, err := body.ReadCloser.Read(buffer)
	body.mu.Lock()
	defer body.mu.Unlock()
	if body.finished {
		return n, err
	}

	if body.events != nil {
		body.events.feed(buffer[:n])
	} else if !body.tooLarge {
		if n > maxUsageObservationBytes-len(body.buffer) {
			body.buffer = nil
			body.tooLarge = true
		} else {
			body.buffer = append(body.buffer, buffer[:n]...)
		}
	}

	if err != nil {
		body.finish(err == io.EOF)
	} else if body.closed {
		body.observeJSON()
	}
	return n, err
}

func (body *usageBody) Close() error {
	// Closing first releases an in-flight transport read. Close does not finalize
	// observer state because that read may still return more response bytes.
	err := body.ReadCloser.Close()

	body.mu.Lock()
	defer body.mu.Unlock()
	body.closed = true
	body.observeJSON()
	return err
}

func (body *usageBody) finish(completeStream bool) {
	if body.finished {
		return
	}

	body.finished = true
	if body.events != nil {
		if completeStream {
			body.events.finish()
		}
	} else {
		body.observeJSON()
		body.buffer = nil
	}
}

func (body *usageBody) observeJSON() {
	if body.events == nil && !body.tooLarge {
		body.observe(body.buffer, "", false)
	}
}

func (body *usageBody) observe(payload []byte, eventType string, streaming bool) {
	total, ok := responseUsage(payload, eventType, streaming)
	if !ok || total <= body.recorded {
		return
	}

	delta := total - body.recorded
	body.recorded = total
	body.record(body.ctx, delta)
}

func responseUsage(payload []byte, eventType string, streaming bool) (int64, bool) {
	if !mayContainUsage(payload) {
		return 0, false
	}

	var response struct {
		Type     string          `json:"type"`
		Usage    json.RawMessage `json:"usage"`
		Response *struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		return 0, false
	}

	if total, ok := usageTotal(response.Usage); ok {
		return total, true
	}
	if response.Response == nil {
		return 0, false
	}

	if streaming {
		if response.Type != "" {
			eventType = response.Type
		}
		switch eventType {
		case "response.completed", "response.incomplete", "response.failed":
		default:
			return 0, false
		}
	}
	return usageTotal(response.Response.Usage)
}

// Events without non-null usage can skip JSON decoding. Escaped strings still
// use the decoder so escaped keys and case-insensitive matching remain supported.
func mayContainUsage(payload []byte) bool {
	for len(payload) > 0 {
		start := bytes.IndexByte(payload, '"')
		if start < 0 {
			return false
		}

		payload = payload[start+1:]
		end := bytes.IndexByte(payload, '"')
		if end < 0 {
			return false
		}

		value := payload[:end]
		if bytes.IndexByte(value, '\\') >= 0 {
			return true
		}
		if bytes.EqualFold(value, []byte("usage")) {
			remainder := bytes.TrimSpace(payload[end+1:])
			if len(remainder) == 0 || remainder[0] != ':' || !bytes.HasPrefix(bytes.TrimSpace(remainder[1:]), []byte("null")) {
				return true
			}
		}

		payload = payload[end+1:]
	}
	return false
}

func usageTotal(payload json.RawMessage) (int64, bool) {
	if len(payload) == 0 || bytes.Equal(payload, []byte("null")) {
		return 0, false
	}

	var usage struct {
		Total      json.RawMessage `json:"total_tokens"`
		Prompt     json.RawMessage `json:"prompt_tokens"`
		Completion json.RawMessage `json:"completion_tokens"`
		Input      json.RawMessage `json:"input_tokens"`
		Output     json.RawMessage `json:"output_tokens"`
	}
	if err := json.Unmarshal(payload, &usage); err != nil {
		return 0, false
	}

	if total, ok := tokenCount(usage.Total); ok {
		return total, true
	}
	if total, ok := tokenSum(usage.Prompt, usage.Completion); ok {
		return total, true
	}
	return tokenSum(usage.Input, usage.Output)
}

func tokenSum(first, second json.RawMessage) (int64, bool) {
	left, leftOK := tokenCount(first)
	right, rightOK := tokenCount(second)
	if !leftOK || !rightOK || left > math.MaxInt64-right {
		return 0, false
	}
	return left + right, true
}

func tokenCount(payload json.RawMessage) (int64, bool) {
	if len(payload) == 0 || (payload[0] != '-' && (payload[0] < '0' || payload[0] > '9')) {
		return 0, false
	}
	value, err := strconv.ParseInt(string(payload), 10, 64)
	return value, err == nil && value >= 0
}

// usageEvents recognizes line endings across transport reads without delaying
// forwarding. An oversized event is skipped until its terminating blank line.
type usageEvents struct {
	observe      func([]byte, string, bool)
	line         []byte
	data         []byte
	eventType    string
	eventBytes   int
	lineNonEmpty bool
	previousCR   bool
	tooLarge     bool
}

func (events *usageEvents) feed(payload []byte) {
	for len(payload) > 0 {
		if events.previousCR {
			events.previousCR = false
			if payload[0] == '\n' {
				payload = payload[1:]
				continue
			}
		}

		end := bytes.IndexAny(payload, "\r\n")
		if end < 0 {
			end = len(payload)
		}
		if end > 0 {
			events.lineNonEmpty = true
			if !events.tooLarge {
				if end > maxUsageObservationBytes-events.eventBytes {
					events.tooLarge = true
					events.line = nil
					events.data = nil
					events.eventType = ""
				} else {
					events.eventBytes += end
					events.line = append(events.line, payload[:end]...)
				}
			}
		}
		if end == len(payload) {
			return
		}

		events.endLine()
		events.previousCR = payload[end] == '\r'
		payload = payload[end+1:]
	}
}

func (events *usageEvents) endLine() {
	if !events.lineNonEmpty {
		events.dispatch()
	} else if !events.tooLarge {
		field, value, _ := bytes.Cut(events.line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "data":
			events.data = append(events.data, value...)
			events.data = append(events.data, '\n')
		case "event":
			events.eventType = string(value)
		}
	}

	events.line = events.line[:0]
	events.lineNonEmpty = false
}

func (events *usageEvents) dispatch() {
	if !events.tooLarge && len(events.data) > 0 {
		events.observe(events.data[:len(events.data)-1], events.eventType, true)
	}

	events.data = events.data[:0]
	events.eventType = ""
	events.eventBytes = 0
	events.tooLarge = false
}

func (events *usageEvents) finish() {
	if events.lineNonEmpty {
		events.endLine()
	}
	events.dispatch()
}
