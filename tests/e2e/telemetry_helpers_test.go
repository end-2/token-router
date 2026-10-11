//go:build e2e

package e2e_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const telemetryTimeout = 45 * time.Second

type telemetryBackend struct {
	otlp, prometheus, tempo, loki, collectorHealth string
}

var telemetry telemetryBackend

type traceSpan struct {
	TraceID      string `json:"traceId"`
	SpanID       string `json:"spanId"`
	ParentSpanID string `json:"parentSpanId"`
	Name         string `json:"name"`
	Kind         any    `json:"kind"`
	Status       struct {
		Code any `json:"code"`
	} `json:"status"`
}

type logEntry struct {
	Message string `json:"msg"`
	TraceID string `json:"trace_id"`
	SpanID  string `json:"span_id"`
	Status  int    `json:"status"`
}

func telemetryEnvironment(service string) map[string]string {
	return map[string]string{
		"TOKEN_ROUTER_TELEMETRY_OTLP_ENABLED":            "true",
		"TOKEN_ROUTER_TELEMETRY_SERVICE_NAME":            service,
		"TOKEN_ROUTER_TELEMETRY_OTLP_ENDPOINT":           telemetry.otlp,
		"TOKEN_ROUTER_TELEMETRY_METRICS_EXPORT_INTERVAL": "100ms",
		"TOKEN_ROUTER_TELEMETRY_SHUTDOWN_TIMEOUT":        "5s",
	}
}

func uniqueService() string {
	return "token-router-e2e-" + strings.ToLower(rand.Text())
}

func newTraceParent() (traceID, parentID, traceParent string) {
	GinkgoHelper()
	identifier := make([]byte, 24)
	_, err := rand.Read(identifier)
	Expect(err).NotTo(HaveOccurred())
	traceID, parentID = hex.EncodeToString(identifier[:16]), hex.EncodeToString(identifier[16:])
	return traceID, parentID, "00-" + traceID + "-" + parentID + "-01"
}

func getBackend(ctx context.Context, client *http.Client, endpoint string) (body []byte, status int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { err = errors.Join(err, response.Body.Close()) }()
	body, err = io.ReadAll(io.LimitReader(response.Body, 4<<20))
	return body, response.StatusCode, err
}

func metricValue(ctx context.Context, client *http.Client, expression string) (float64, error) {
	body, status, err := getBackend(ctx, client, telemetry.prometheus+"/api/v1/query?query="+url.QueryEscape(expression))
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("Prometheus query status %d: %s", status, body)
	}
	var result struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Value []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, err
	}
	if result.Status != "success" {
		return 0, fmt.Errorf("Prometheus query did not succeed: %s", body)
	}
	var total float64
	for _, sample := range result.Data.Result {
		if len(sample.Value) != 2 {
			return 0, fmt.Errorf("invalid Prometheus sample: %s", body)
		}
		var value string
		if err := json.Unmarshal(sample.Value[1], &value); err != nil {
			return 0, err
		}
		number, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return 0, err
		}
		total += number
	}
	return total, nil
}

func requestMetric(service string, status int) string {
	return fmt.Sprintf(`token_router_http_requests_total{service_name=%q,http_route="/v1/*",http_response_status_code=%q}`, service, strconv.Itoa(status))
}

func limitMetric(service, result string) string {
	return fmt.Sprintf(`token_router_rate_limit_decisions_total{service_name=%q,result=%q}`, service, result)
}

func getTrace(ctx context.Context, client *http.Client, traceID string) ([]traceSpan, string, error) {
	body, status, err := getBackend(ctx, client, telemetry.tempo+"/api/traces/"+traceID)
	if err != nil {
		return nil, "", err
	}
	if status == http.StatusNotFound {
		return nil, "", nil
	}
	if status != http.StatusOK {
		return nil, "", fmt.Errorf("Tempo query status %d: %s", status, body)
	}
	var document any
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, "", err
	}
	var spans []traceSpan
	var walk func(any) error
	walk = func(value any) error {
		switch node := value.(type) {
		case map[string]any:
			if _, found := node["spanId"]; found {
				encoded, err := json.Marshal(node)
				if err != nil {
					return err
				}
				var span traceSpan
				if err := json.Unmarshal(encoded, &span); err != nil {
					return err
				}
				span.TraceID, span.SpanID, span.ParentSpanID = hexID(span.TraceID), hexID(span.SpanID), hexID(span.ParentSpanID)
				spans = append(spans, span)
				return nil
			}
			for _, child := range node {
				if err := walk(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range node {
				if err := walk(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	err = walk(document)
	return spans, string(body), err
}

func hexID(identifier string) string {
	if _, err := hex.DecodeString(identifier); err == nil {
		return strings.ToLower(identifier)
	}
	if decoded, err := base64.StdEncoding.DecodeString(identifier); err == nil {
		return hex.EncodeToString(decoded)
	}
	return identifier
}

func spanKind(span traceSpan, name string, number float64) bool {
	return span.Kind == name || span.Kind == number
}

func tracePair(spans []traceSpan) (server, client traceSpan) {
	for _, span := range spans {
		if spanKind(span, "SPAN_KIND_SERVER", 2) {
			server = span
		}
		if spanKind(span, "SPAN_KIND_CLIENT", 3) {
			client = span
		}
	}
	return server, client
}

func queryLogs(ctx context.Context, client *http.Client, service string) (int, string, error) {
	parameters := url.Values{
		"query":     {fmt.Sprintf(`{service_name=%q}`, service)},
		"start":     {strconv.FormatInt(time.Now().Add(-5*time.Minute).UnixNano(), 10)},
		"end":       {strconv.FormatInt(time.Now().Add(time.Minute).UnixNano(), 10)},
		"limit":     {"1000"},
		"direction": {"backward"},
	}
	body, status, err := getBackend(ctx, client, telemetry.loki+"/loki/api/v1/query_range?"+parameters.Encode())
	if err != nil {
		return 0, "", err
	}
	if status != http.StatusOK {
		return 0, "", fmt.Errorf("Loki query status %d: %s", status, body)
	}
	var document struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Values []json.RawMessage `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return 0, "", err
	}
	if document.Status != "success" {
		return 0, "", fmt.Errorf("Loki query did not succeed: %s", body)
	}
	count := 0
	for _, stream := range document.Data.Result {
		count += len(stream.Values)
	}
	return count, string(body), nil
}

func accessLogs(output []byte) ([]logEntry, error) {
	var result []logEntry
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		var entry logEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return nil, fmt.Errorf("stderr must contain JSON log records: %w", err)
		}
		if entry.Message == "HTTP request completed" {
			result = append(result, entry)
		}
	}
	return result, nil
}

func assertSignalsAbsent(ctx context.Context, client *http.Client, service, traceID string, status int, metricsEnabled, tracesEnabled, logsEnabled bool) {
	GinkgoHelper()
	// Cover the Collector batch delay and a complete Prometheus scrape interval.
	Consistently(func(g Gomega) {
		if !metricsEnabled {
			value, err := metricValue(ctx, client, requestMetric(service, status))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(value).To(BeZero())
		}
		if !tracesEnabled {
			spans, _, err := getTrace(ctx, client, traceID)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(spans).To(BeEmpty())
		}
		if !logsEnabled {
			count, _, err := queryLogs(ctx, client, service)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(count).To(BeZero())
		}
	}, 6*time.Second, 500*time.Millisecond).Should(Succeed())
}
