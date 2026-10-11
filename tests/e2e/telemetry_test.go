//go:build e2e

package e2e_test

import (
	"bufio"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

var _ = Describe("the OpenTelemetry pipeline", func() {
	It("correlates JSON logs, Prometheus metrics and propagated server and client spans without request secrets", func(ctx SpecContext) {
		service := uniqueService()
		traceID, parentID, traceParent := newTraceParent()
		forwarded := make(chan string, 1)
		upstream := newUpstream(func(writer http.ResponseWriter, req *http.Request) {
			forwarded <- req.Header.Get("Traceparent")
			writer.WriteHeader(http.StatusOK)
			writeBody(writer, `{"id":"test-response"}`)
		})
		process := startRouter(ctx, upstream.URL, routerOptions{env: telemetryEnvironment(service)})
		client := newHTTPClient()
		response := request(ctx, client, http.MethodPost,
			process.url+"/v1/responses?credential=e2e-secret-query",
			`{"input":"e2e-secret-prompt"}`, map[string]string{
				"Authorization": "Bearer e2e-secret-api-key", "Traceparent": traceParent,
			})
		Expect(response.StatusCode).To(Equal(http.StatusOK))
		Expect(readBody(response)).To(ContainSubstring("test-response"))
		var upstreamParent string
		Eventually(forwarded, requestTimeout).Should(Receive(&upstreamParent))
		upstreamParts := strings.Split(upstreamParent, "-")
		Expect(upstreamParts).To(HaveLen(4))
		Expect(upstreamParts[1]).To(Equal(traceID))

		var server, outbound traceSpan
		var traceJSON string
		Eventually(func(g Gomega) {
			spans, body, err := getTrace(ctx, client, traceID)
			g.Expect(err).NotTo(HaveOccurred())
			server, outbound = tracePair(spans)
			g.Expect(server.SpanID).NotTo(BeEmpty())
			g.Expect(outbound.SpanID).NotTo(BeEmpty())
			traceJSON = body
		}, telemetryTimeout).Should(Succeed())
		Expect(server.TraceID).To(Equal(traceID))
		Expect(server.ParentSpanID).To(Equal(parentID))
		Expect(server.Name).To(Equal("POST /v1/*"))
		Expect(outbound.TraceID).To(Equal(traceID))
		Expect(outbound.ParentSpanID).To(Equal(server.SpanID))
		Expect(outbound.SpanID).To(Equal(upstreamParts[2]))
		Expect(outbound.Name).To(Equal("POST upstream"))

		Eventually(func(g Gomega) {
			entries, err := accessLogs(process.session.Err.Contents())
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(entries).To(ContainElement(logEntry{
				Message: "HTTP request completed", TraceID: traceID, SpanID: server.SpanID, Status: http.StatusOK,
			}))
		}, requestTimeout).Should(Succeed())
		Eventually(func() (float64, error) {
			return metricValue(ctx, client, requestMetric(service, http.StatusOK))
		}, telemetryTimeout).Should(Equal(float64(1)))
		var logJSON string
		Eventually(func(g Gomega) {
			count, body, err := queryLogs(ctx, client, service)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(count).To(BeNumerically(">", 0))
			g.Expect(body).To(ContainSubstring("HTTP request completed"))
			g.Expect(body).To(ContainSubstring(traceID))
			g.Expect(body).To(ContainSubstring(server.SpanID))
			logJSON = body
		}, telemetryTimeout).Should(Succeed())
		for _, secret := range []string{"e2e-secret-query", "e2e-secret-prompt", "e2e-secret-api-key"} {
			Expect(string(process.session.Err.Contents())).NotTo(ContainSubstring(secret))
			Expect(traceJSON).NotTo(ContainSubstring(secret))
			Expect(logJSON).NotTo(ContainSubstring(secret))
		}
	}, SpecTimeout(90*time.Second))

	DescribeTable("exports only configured signals",
		func(ctx SpecContext, metricsEnabled, tracesEnabled, logsEnabled bool) {
			service := uniqueService()
			traceID, parentID, traceParent := newTraceParent()
			upstream := newUpstream(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(http.StatusServiceUnavailable)
			})
			environment := telemetryEnvironment(service)
			environment["TOKEN_ROUTER_SERVER_ADDRESS"] = unusedAddress()
			environment["TOKEN_ROUTER_TELEMETRY_METRICS_ENABLED"] = fmt.Sprint(metricsEnabled)
			environment["TOKEN_ROUTER_TELEMETRY_TRACES_ENABLED"] = fmt.Sprint(tracesEnabled)
			environment["TOKEN_ROUTER_TELEMETRY_LOGS_ENABLED"] = fmt.Sprint(logsEnabled)
			process := startRouter(ctx, upstream.URL, routerOptions{env: environment})
			client := newHTTPClient()
			response := request(ctx, client, http.MethodGet, process.url+"/v1/models", "", map[string]string{"Traceparent": traceParent})
			Expect(response.StatusCode).To(Equal(http.StatusServiceUnavailable))
			readBody(response)
			Expect(process.session.Command.Process.Signal(syscall.SIGTERM)).To(Succeed())
			Eventually(process.session, 10*time.Second).Should(gexec.Exit(0))

			if metricsEnabled {
				Eventually(func() (float64, error) {
					return metricValue(ctx, client, requestMetric(service, http.StatusServiceUnavailable))
				}, telemetryTimeout).Should(Equal(float64(1)))
			}
			if tracesEnabled {
				Eventually(func(g Gomega) {
					spans, _, err := getTrace(ctx, client, traceID)
					g.Expect(err).NotTo(HaveOccurred())
					server, outbound := tracePair(spans)
					g.Expect(server.ParentSpanID).To(Equal(parentID))
					g.Expect(outbound.ParentSpanID).To(Equal(server.SpanID))
					g.Expect(server.Status.Code).To(Or(Equal(float64(2)), Equal("STATUS_CODE_ERROR")))
					g.Expect(outbound.Status.Code).To(Or(Equal(float64(2)), Equal("STATUS_CODE_ERROR")))
				}, telemetryTimeout).Should(Succeed())
			}
			if logsEnabled {
				Eventually(func(g Gomega) {
					count, body, err := queryLogs(ctx, client, service)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(count).To(BeNumerically(">", 0))
					g.Expect(body).To(ContainSubstring("HTTP request completed"))
				}, telemetryTimeout).Should(Succeed())
			}
			assertSignalsAbsent(ctx, client, service, traceID, http.StatusServiceUnavailable,
				metricsEnabled, tracesEnabled, logsEnabled)
			entries, err := accessLogs(process.session.Err.Contents())
			Expect(err).NotTo(HaveOccurred())
			if logsEnabled {
				Expect(entries).NotTo(BeEmpty())
			} else {
				Expect(process.session.Err.Contents()).To(BeEmpty(), "disabled logs must not write to stderr")
			}
			if !tracesEnabled {
				for _, entry := range entries {
					Expect(entry.TraceID).To(BeEmpty())
					Expect(entry.SpanID).To(BeEmpty())
				}
			}
		},
		Entry("metrics without traces or exported logs", SpecTimeout(90*time.Second), true, false, false),
		Entry("error traces without metrics or exported logs", SpecTimeout(90*time.Second), false, true, false),
		Entry("exported logs without metrics or traces", SpecTimeout(90*time.Second), false, false, true),
	)

	It("keeps JSON access logs and the Prometheus listener when OTLP export is disabled", func(ctx SpecContext) {
		service := uniqueService()
		traceID, _, traceParent := newTraceParent()
		upstream := newUpstream(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusNoContent)
		})
		environment := telemetryEnvironment(service)
		environment["TOKEN_ROUTER_TELEMETRY_OTLP_ENABLED"] = "false"
		process := startRouter(ctx, upstream.URL, routerOptions{env: environment})
		client := newHTTPClient()
		response := request(ctx, client, http.MethodGet, process.url+"/v1/models", "", map[string]string{"Traceparent": traceParent})
		Expect(response.StatusCode).To(Equal(http.StatusNoContent))
		readBody(response)
		metrics := request(ctx, client, http.MethodGet, process.metricsURL+"/metrics", "", nil)
		Expect(readBody(metrics)).To(ContainSubstring("token_router_http_requests_total"))
		Expect(process.session.Command.Process.Signal(syscall.SIGTERM)).To(Succeed())
		Eventually(process.session, 10*time.Second).Should(gexec.Exit(0))
		entries, err := accessLogs(process.session.Err.Contents())
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(ContainElement(logEntry{Message: "HTTP request completed", Status: http.StatusNoContent}))
		assertSignalsAbsent(ctx, client, service, traceID, http.StatusNoContent, false, false, false)
	}, SpecTimeout(30*time.Second))

	It("exports allowed and denied rate limit decisions through the Collector", func(ctx SpecContext) {
		service := uniqueService()
		upstream := newUpstream(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusNoContent)
		})
		environment := telemetryEnvironment(service)
		environment["TOKEN_ROUTER_RATE_LIMIT_ENABLED"] = "true"
		environment["TOKEN_ROUTER_RATE_LIMIT_REQUESTS_PER_SECOND"] = "0.001"
		environment["TOKEN_ROUTER_RATE_LIMIT_BURST"] = "1"
		process := startRouter(ctx, upstream.URL, routerOptions{env: environment})
		client := newHTTPClient()
		for _, status := range []int{http.StatusNoContent, http.StatusTooManyRequests} {
			response := request(ctx, client, http.MethodGet, process.url+"/v1/models", "", nil)
			Expect(response.StatusCode).To(Equal(status))
			readBody(response)
		}
		Eventually(func(g Gomega) {
			for _, result := range []string{"allowed", "denied"} {
				value, err := metricValue(ctx, client, limitMetric(service, result))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(value).To(Equal(float64(1)))
			}
			value, err := metricValue(ctx, client, requestMetric(service, http.StatusTooManyRequests))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(value).To(Equal(float64(1)))
		}, telemetryTimeout).Should(Succeed())
	}, SpecTimeout(60*time.Second))

	It("flushes final SSE spans, logs and long-interval metrics after graceful shutdown", func(ctx SpecContext) {
		service := uniqueService()
		traceID, _, traceParent := newTraceParent()
		finish := make(chan struct{})
		var finishOnce sync.Once
		complete := func() { finishOnce.Do(func() { close(finish) }) }
		DeferCleanup(complete)
		upstream := newUpstream(func(writer http.ResponseWriter, req *http.Request) {
			writer.Header().Set("Content-Type", "text/event-stream")
			writeEvent(writer, "first")
			select {
			case <-finish:
				writeEvent(writer, "[DONE]")
			case <-req.Context().Done():
			}
		})
		environment := telemetryEnvironment(service)
		environment["TOKEN_ROUTER_TELEMETRY_METRICS_EXPORT_INTERVAL"] = "1h"
		process := startRouter(ctx, upstream.URL, routerOptions{env: environment})
		client := newHTTPClient()
		response := request(ctx, client, http.MethodPost, process.url+"/v1/responses", `{}`, map[string]string{"Traceparent": traceParent})
		Expect(response.StatusCode).To(Equal(http.StatusOK))
		reader := bufio.NewReader(response.Body)
		Expect(readEvent(reader)).To(Equal("data: first"))
		spans, _, err := getTrace(ctx, client, traceID)
		Expect(err).NotTo(HaveOccurred())
		Expect(spans).To(BeEmpty(), "active SSE spans must finish with the response body")
		value, err := metricValue(ctx, client, requestMetric(service, http.StatusOK))
		Expect(err).NotTo(HaveOccurred())
		Expect(value).To(BeZero())
		Expect(process.session.Command.Process.Signal(syscall.SIGTERM)).To(Succeed())
		Consistently(process.session, 100*time.Millisecond).ShouldNot(gexec.Exit())
		complete()
		Expect(readEvent(reader)).To(Equal("data: [DONE]"))
		readBody(response)
		Eventually(process.session, 10*time.Second).Should(gexec.Exit(0))
		Eventually(func(g Gomega) {
			spans, _, err := getTrace(ctx, client, traceID)
			g.Expect(err).NotTo(HaveOccurred())
			server, outbound := tracePair(spans)
			g.Expect(server.SpanID).NotTo(BeEmpty())
			g.Expect(outbound.ParentSpanID).To(Equal(server.SpanID))
			value, err := metricValue(ctx, client, requestMetric(service, http.StatusOK))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(value).To(Equal(float64(1)))
			_, logs, err := queryLogs(ctx, client, service)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(logs).To(ContainSubstring("HTTP request completed"))
			g.Expect(logs).To(ContainSubstring(traceID))
		}, telemetryTimeout).Should(Succeed())
	}, SpecTimeout(60*time.Second))
})
