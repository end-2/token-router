//go:build e2e

package e2e_test

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"
)

var _ = Describe("the running proxy", func() {
	It("applies defaults, YAML, environment, then command-line authentication settings", func(ctx SpecContext) {
		authorization := make(chan string, 4)
		upstream := newUpstream(func(writer http.ResponseWriter, req *http.Request) {
			authorization <- req.Header.Get("Authorization")
			writer.WriteHeader(http.StatusNoContent)
		})
		for _, scenario := range []struct {
			name     string
			options  routerOptions
			expected string
		}{
			{name: "the default forwards the client's authorization", expected: "Bearer client-key"},
			{name: "YAML overrides the default", options: routerOptions{
				yaml: "upstream:\n  api_key: yaml-key\n",
			}, expected: "Bearer yaml-key"},
			{name: "environment overrides YAML", options: routerOptions{
				yaml: "upstream:\n  api_key: yaml-key\n", env: map[string]string{"TOKEN_ROUTER_UPSTREAM_API_KEY": "env-key"},
			}, expected: "Bearer env-key"},
			{name: "command-line flags override environment and YAML", options: routerOptions{
				yaml: "upstream:\n  api_key: yaml-key\n", env: map[string]string{"TOKEN_ROUTER_UPSTREAM_API_KEY": "env-key"},
				flags: []string{"--upstream-api-key", "flag-key"},
			}, expected: "Bearer flag-key"},
		} {
			By(scenario.name)
			process := startRouter(ctx, upstream.URL, scenario.options)
			response := request(ctx, newHTTPClient(), http.MethodGet, process.url+"/v1/models", "", map[string]string{
				"Authorization": "Bearer client-key",
			})
			Expect(response.StatusCode).To(Equal(http.StatusNoContent))
			Eventually(authorization, requestTimeout).Should(Receive(Equal(scenario.expected)))
		}
	}, SpecTimeout(30*time.Second))

	It("preserves the request and upstream error status, headers, and body", func(ctx SpecContext) {
		type forwardedRequest struct {
			method, uri, body, contentType, organization, project string
		}
		received := make(chan forwardedRequest, 1)
		upstream := newUpstream(func(writer http.ResponseWriter, req *http.Request) {
			body, err := io.ReadAll(req.Body)
			Expect(err).NotTo(HaveOccurred())
			received <- forwardedRequest{
				method: req.Method, uri: req.RequestURI, body: string(body), contentType: req.Header.Get("Content-Type"),
				organization: req.Header.Get("OpenAI-Organization"), project: req.Header.Get("OpenAI-Project"),
			}
			writer.Header().Set("Content-Type", "application/json")
			writer.Header().Set("X-Request-ID", "upstream-request-id")
			writer.Header().Set("Retry-After", "7")
			writer.WriteHeader(http.StatusTooManyRequests)
			writeBody(writer, `{"error":{"code":"insufficient_quota"}}`)
		})
		process := startRouter(ctx, upstream.URL+"/gateway", routerOptions{flags: []string{
			"--upstream-organization", "test-org", "--upstream-project", "test-project",
		}})
		body := `{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`
		response := request(ctx, newHTTPClient(), http.MethodPost,
			process.url+"/v1/chat/completions?trace=a%2Fb&tag=one&tag=two", body,
			map[string]string{"Content-Type": "application/json"})
		Expect(response.StatusCode).To(Equal(http.StatusTooManyRequests))
		Expect(response.Header.Get("X-Request-ID")).To(Equal("upstream-request-id"))
		Expect(response.Header.Get("Retry-After")).To(Equal("7"))
		Expect(response.Header.Get("Content-Type")).To(Equal("application/json"))
		Expect(readBody(response)).To(Equal(`{"error":{"code":"insufficient_quota"}}`))
		Eventually(received, requestTimeout).Should(Receive(Equal(forwardedRequest{
			method: http.MethodPost, uri: "/gateway/v1/chat/completions?trace=a%2Fb&tag=one&tag=two", body: body,
			contentType: "application/json", organization: "test-org", project: "test-project",
		})))
	}, SpecTimeout(15*time.Second))

	It("serves health and readiness and exports Prometheus metrics on its separate listener", func(ctx SpecContext) {
		upstream := newUpstream(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			writeBody(writer, `{"data":[]}`)
		})
		process := startRouter(ctx, upstream.URL, routerOptions{flags: []string{"--telemetry-metrics-prometheus-path", "/observe"}})
		client := newHTTPClient()
		for path, expected := range map[string]string{"/healthz": "ok", "/readyz": "ready"} {
			response := request(ctx, client, http.MethodGet, process.url+path, "", nil)
			Expect(response.StatusCode).To(Equal(http.StatusOK))
			Expect(readBody(response)).To(MatchJSON(fmt.Sprintf(`{"status":%q}`, expected)))
		}
		response := request(ctx, client, http.MethodGet, process.url+"/v1/models", "", nil)
		Expect(response.StatusCode).To(Equal(http.StatusOK))
		Expect(readBody(response)).To(MatchJSON(`{"data":[]}`))
		Eventually(func(g Gomega) {
			metrics := request(ctx, client, http.MethodGet, process.metricsURL+"/observe", "", nil)
			g.Expect(metrics.StatusCode).To(Equal(http.StatusOK))
			body := readBody(metrics)
			g.Expect(body).To(ContainSubstring(`token_router_http_requests_total{method="GET",route="/v1/*",status="200"} 1`))
			g.Expect(body).To(ContainSubstring(`token_router_rate_limit_decisions_total{result="allowed"} 1`))
			g.Expect(body).To(ContainSubstring("go_goroutines"))
		}, requestTimeout).Should(Succeed())
		Expect(request(ctx, client, http.MethodGet, process.url+"/observe", "", nil).StatusCode).To(Equal(http.StatusNotFound))
		Expect(request(ctx, client, http.MethodGet, process.metricsURL+"/metrics", "", nil).StatusCode).To(Equal(http.StatusNotFound))
	}, SpecTimeout(15*time.Second))

	It("returns an OpenAI-shaped timeout error when the upstream does not send headers", func(ctx SpecContext) {
		canceled := make(chan struct{})
		upstream := newUpstream(func(_ http.ResponseWriter, req *http.Request) {
			<-req.Context().Done()
			close(canceled)
		})
		process := startRouter(ctx, upstream.URL, routerOptions{flags: []string{"--upstream-response-header-timeout", "100ms"}})
		response := request(ctx, newHTTPClient(), http.MethodGet, process.url+"/v1/models", "", nil)
		Expect(response.StatusCode).To(Equal(http.StatusGatewayTimeout))
		Expect(readBody(response)).To(MatchJSON(`{"error":{"message":"The upstream request timed out.","type":"proxy_error","code":"upstream_timeout"}}`))
		Eventually(canceled, requestTimeout).Should(BeClosed())
	}, SpecTimeout(15*time.Second))

	It("rejects requests above the memory bucket capacity with Retry-After", func(ctx SpecContext) {
		var forwarded atomic.Int64
		upstream := newUpstream(func(writer http.ResponseWriter, _ *http.Request) {
			forwarded.Add(1)
			writer.WriteHeader(http.StatusNoContent)
		})
		process := startRouter(ctx, upstream.URL, routerOptions{env: map[string]string{
			"TOKEN_ROUTER_RATE_LIMIT_ENABLED": "true", "TOKEN_ROUTER_RATE_LIMIT_BACKEND": "memory",
			"TOKEN_ROUTER_RATE_LIMIT_REQUESTS_PER_SECOND": "0.001", "TOKEN_ROUTER_RATE_LIMIT_BURST": "1",
		}})
		client := newHTTPClient()
		Expect(request(ctx, client, http.MethodGet, process.url+"/v1/models", "", nil).StatusCode).To(Equal(http.StatusNoContent))
		response := request(ctx, client, http.MethodGet, process.url+"/v1/models", "", nil)
		Expect(response.StatusCode).To(Equal(http.StatusTooManyRequests))
		retry, err := strconv.Atoi(response.Header.Get("Retry-After"))
		Expect(err).NotTo(HaveOccurred())
		Expect(retry).To(BeNumerically(">", 0))
		Expect(readBody(response)).To(MatchJSON(`{"error":{"type":"rate_limit_exceeded","code":"rate_limit_exceeded","message":"Rate limit exceeded"}}`))
		Expect(forwarded.Load()).To(Equal(int64(1)))
	}, SpecTimeout(15*time.Second))

	It("shares one atomic Redis bucket across concurrent requests to separate processes", func(ctx SpecContext) {
		var forwarded atomic.Int64
		upstream := newUpstream(func(writer http.ResponseWriter, _ *http.Request) {
			forwarded.Add(1)
			writer.WriteHeader(http.StatusNoContent)
		})
		options := routerOptions{env: redisEnvironment(newRedisKey())}
		first := startRouter(ctx, upstream.URL, options)
		second := startRouter(ctx, upstream.URL, options)
		client := newHTTPClient()
		const count = 30
		type outcome struct {
			status int
			err    error
		}
		results := make(chan outcome, count)
		start := make(chan struct{})
		var workers sync.WaitGroup
		for i := range count {
			workers.Go(func() {
				<-start
				url := first.url
				if i%2 != 0 {
					url = second.url
				}
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/v1/models", nil)
				if err != nil {
					results <- outcome{err: err}
					return
				}
				response, err := client.Do(req)
				if err != nil {
					results <- outcome{err: err}
					return
				}
				results <- outcome{status: response.StatusCode, err: response.Body.Close()}
			})
		}
		close(start)
		workers.Wait()
		close(results)
		allowed, denied := 0, 0
		for result := range results {
			Expect(result.err).NotTo(HaveOccurred())
			switch result.status {
			case http.StatusNoContent:
				allowed++
			case http.StatusTooManyRequests:
				denied++
			default:
				Fail(fmt.Sprintf("unexpected proxy status: %d", result.status))
			}
		}
		Expect(allowed).To(Equal(5))
		Expect(denied).To(Equal(count - 5))
		Expect(forwarded.Load()).To(Equal(int64(5)))
	}, SpecTimeout(20*time.Second))

	It("fails closed when Redis cannot evaluate the bucket", func(ctx SpecContext) {
		var forwarded atomic.Int64
		upstream := newUpstream(func(writer http.ResponseWriter, _ *http.Request) {
			forwarded.Add(1)
			writer.WriteHeader(http.StatusNoContent)
		})
		key := newRedisKey()
		process := startRouter(ctx, upstream.URL, routerOptions{env: redisEnvironment(key)})
		// A wrong-type key causes an actual Redis command error without changing the shared server.
		Expect(redisClient.Set(ctx, key, "invalid-bucket-type", time.Minute).Err()).To(Succeed())
		response := request(ctx, newHTTPClient(), http.MethodGet, process.url+"/v1/models", "", nil)
		Expect(response.StatusCode).To(Equal(http.StatusServiceUnavailable))
		Expect(readBody(response)).To(MatchJSON(`{"error":{"type":"rate_limiter_unavailable","code":"rate_limiter_unavailable","message":"Rate limiter is unavailable"}}`))
		Expect(forwarded.Load()).To(BeZero())
	}, SpecTimeout(15*time.Second))

	DescribeTable("rejects invalid configuration before opening listeners",
		func(options routerOptions, message string) {
			process := launchRouter("http://127.0.0.1:1", options)
			Eventually(process.session, requestTimeout).Should(gexec.Exit(1))
			Expect(process.session.Err).To(gbytes.Say(message))
			Expect(listenerAddresses(process.session.Err.Contents())).To(BeEmpty())
		},
		Entry("an unknown YAML field", routerOptions{yaml: "upstream:\n  api_keey: bad\n"}, "configuration"),
		Entry("an unsupported strategy", routerOptions{flags: []string{"--rate-limit-backend", "invalid"}}, "rate_limit.backend"),
		Entry("a nonpositive shutdown deadline", routerOptions{flags: []string{"--server-shutdown-timeout", "0s"}}, "server.shutdown_timeout"),
	)
})
