//go:build e2e

package e2e_test

import (
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("upstream token usage limits", func() {
	DescribeTable("blocks later requests after reported token usage exhausts capacity",
		func(ctx SpecContext, backend, contentType, body string) {
			var forwarded atomic.Int64
			upstream := newUpstream(func(writer http.ResponseWriter, _ *http.Request) {
				forwarded.Add(1)
				writer.Header().Set("Content-Type", contentType)
				writeBody(writer, body)
			})

			environment := map[string]string{
				"TOKEN_ROUTER_RATE_LIMIT_ENABLED": "true",
				"TOKEN_ROUTER_RATE_LIMIT_BACKEND": backend,
			}
			if backend == "redis" {
				environment = redisEnvironment(newRedisKey())
			}
			options := routerOptions{env: environment, flags: []string{
				"--rate-limit-burst", "20",
				"--rate-limit-tokens-per-second", "0.001",
				"--rate-limit-token-burst", "10",
			}}

			first := startRouter(ctx, upstream.URL, options)
			second := first
			if backend == "redis" {
				second = startRouter(ctx, upstream.URL, options)
			}

			client := newHTTPClient()
			response := request(ctx, client, http.MethodPost, first.url+"/v1/chat/completions",
				`{"model":"test-model","stream":true,"stream_options":{"include_usage":true}}`,
				map[string]string{"Content-Type": "application/json"})

			Expect(response.StatusCode).To(Equal(http.StatusOK))
			Expect(readBody(response)).To(Equal(body))

			response = request(ctx, client, http.MethodPost, second.url+"/v1/responses", `{}`, nil)
			Expect(response.StatusCode).To(Equal(http.StatusTooManyRequests))
			retry, err := strconv.Atoi(response.Header.Get("Retry-After"))
			Expect(err).NotTo(HaveOccurred())
			Expect(retry).To(BeNumerically(">", 0))
			Expect(forwarded.Load()).To(Equal(int64(1)))
		},
		Entry("memory JSON", SpecTimeout(20*time.Second), "memory", "application/json", `{"usage":{"prompt_tokens":8,"completion_tokens":4,"total_tokens":12}}`),
		Entry("Redis JSON shared by processes", SpecTimeout(20*time.Second), "redis", "application/json", `{"usage":{"input_tokens":8,"output_tokens":4,"total_tokens":12}}`),
		Entry("memory SSE", SpecTimeout(20*time.Second), "memory", "text/event-stream", "data: {\"usage\":null}\n\ndata: {\"usage\":{\"total_tokens\":12}}\n\ndata: [DONE]\n\n"),
		Entry("Redis SSE shared by processes", SpecTimeout(20*time.Second), "redis", "text/event-stream", "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"total_tokens\":12}}}\n\n"),
	)
})
