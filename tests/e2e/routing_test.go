//go:build e2e

package e2e_test

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("upstream routing", func() {
	It("matches Host and model and balances duplicate conditions across URL and address arrays", func(ctx SpecContext) {
		addresses := make(map[string]string)
		for _, name := range []string{"default", "host", "model", "first", "second", "third"} {
			upstream := newUpstream(func(writer http.ResponseWriter, req *http.Request) {
				writer.Header().Set("X-Upstream", name)
				body, err := io.ReadAll(req.Body)
				Expect(err).NotTo(HaveOccurred())
				writeBody(writer, string(body))
			})
			addresses[name] = upstream.URL
		}

		process := startRouter(ctx, "", routerOptions{yaml: fmt.Sprintf(`upstream:
  routes:
    - host: "*"
      model: "*"
      base_urls: [%q]
    - host: api.example.com
      model: "*"
      base_urls: [%q]
    - host: "*"
      model: chat
      base_urls: [%q]
    - host: api.example.com
      model: chat
      base_urls: [%q, %q]
    - host: API.EXAMPLE.COM.
      model: chat
      base_urls: [%q]
`, addresses["default"], addresses["host"], addresses["model"], strings.TrimPrefix(addresses["first"], "http://"), addresses["second"], addresses["third"])})

		client := newHTTPClient()
		for _, scenario := range []struct{ host, model, want string }{
			{"API.EXAMPLE.COM:8080", "chat", "first"},
			{"api.example.com", "chat", "second"},
			{"api.example.com", "chat", "third"},
			{"api.example.com", "chat", "first"},
			{"api.example.com", "embed", "host"},
			{"other.example.com", "chat", "model"},
			{"other.example.com", "embed", "default"},
		} {
			body := fmt.Sprintf(`{"model":%q,"input":"preserved"}`, scenario.model)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, process.url+"/v1/responses", strings.NewReader(body))
			Expect(err).NotTo(HaveOccurred())
			req.Host = scenario.host
			req.Header.Set("Content-Type", "application/json")

			response, err := client.Do(req)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(response.Body.Close()).To(Succeed()) })

			Expect(response.StatusCode).To(Equal(http.StatusOK))
			Expect(response.Header.Get("X-Upstream")).To(Equal(scenario.want))
			Expect(readBody(response)).To(Equal(body))
		}
	}, SpecTimeout(15*time.Second))

	It("flushes routed SSE responses and charges their usage to the shared token bucket", func(ctx SpecContext) {
		release := make(chan struct{}, 1)
		upstream := newUpstream(func(writer http.ResponseWriter, req *http.Request) {
			body, err := io.ReadAll(req.Body)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(body)).To(MatchJSON(`{"model":"chat","stream":true}`))

			writer.Header().Set("Content-Type", "text/event-stream")
			writeEvent(writer, `{"type":"response.created"}`)
			select {
			case <-release:
				writeEvent(writer, `{"type":"response.completed","response":{"usage":{"total_tokens":10}}}`)
			case <-req.Context().Done():
			}
		})

		process := startRouter(ctx, "", routerOptions{flags: []string{
			"--upstream-routes", fmt.Sprintf(`[{"model":"chat","base_urls":[%q]}]`, upstream.URL),
			"--rate-limit-enabled=true", "--rate-limit-tokens-per-second=0.001", "--rate-limit-token-burst=10",
		}})

		client := newHTTPClient()
		response := request(ctx, client, http.MethodPost, process.url+"/v1/responses", `{"model":"chat","stream":true}`, nil)
		Expect(response.StatusCode).To(Equal(http.StatusOK))
		reader := bufio.NewReader(response.Body)
		Expect(readEvent(reader)).To(Equal(`data: {"type":"response.created"}`))

		release <- struct{}{}
		remaining, err := io.ReadAll(reader)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(remaining)).To(ContainSubstring(`"total_tokens":10`))
		Expect(response.Body.Close()).To(Succeed())

		denied := request(ctx, client, http.MethodPost, process.url+"/v1/responses", `{"model":"chat"}`, nil)
		Expect(denied.StatusCode).To(Equal(http.StatusTooManyRequests))
	}, SpecTimeout(15*time.Second))
})
