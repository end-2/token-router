//go:build e2e

package e2e_test

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

var _ = Describe("policy configuration", func() {
	DescribeTable("watches replacement files and persists updates through a separate admin listener",
		func(ctx SpecContext, backend string) {
			first := newUpstream(func(w http.ResponseWriter, _ *http.Request) { writeBody(w, "first") })
			second := newUpstream(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				writeBody(w, `{"usage":{"total_tokens":10}}`)
			})
			file := filepath.Join(GinkgoT().TempDir(), "policy.yaml")
			Expect(os.WriteFile(file, fmt.Appendf(nil, "upstream:\n  base_urls: [%q]\nrate_limit:\n  requests_per_second: 1000\n  burst: 100\n", first.URL), 0600)).To(Succeed())
			environment := map[string]string{"TOKEN_ROUTER_RATE_LIMIT_ENABLED": "true"}
			if backend == "redis" {
				environment = redisEnvironment(newRedisKey())
			}
			options := routerOptions{env: environment, flags: []string{
				"--admin-enabled", "--admin-address=127.0.0.1:0", "--policy-file", file, "--policy-watch-interval=10ms",
			}}
			process := launchRouter(ctx, first.URL, options)
			Eventually(func() []string { return listenerAddresses(process.session.Err.Contents()) }, requestTimeout).Should(HaveLen(3))
			addresses := listenerAddresses(process.session.Err.Contents())
			publicURL, adminURL := "http://"+addresses[0], "http://"+addresses[1]
			client := newHTTPClient()
			Expect(readBody(request(ctx, client, http.MethodGet, publicURL+"/v1/models", "", nil))).To(Equal("first"))
			Expect(request(ctx, client, http.MethodGet, publicURL+"/config", "", nil).StatusCode).To(Equal(http.StatusNotFound))
			Expect(request(ctx, client, http.MethodGet, adminURL+"/v1/models", "", nil).StatusCode).To(Equal(http.StatusNotFound))

			update := fmt.Sprintf(`{"upstream":{"base_urls":[%q]},"rate_limit":{"tokens_per_second":0.001,"token_burst":10}}`, second.URL)
			response := request(ctx, client, http.MethodPatch, adminURL+"/config", update, map[string]string{"Content-Type": "application/json"})
			Expect(response.StatusCode).To(Equal(http.StatusOK))
			Expect(readBody(response)).To(ContainSubstring(second.URL))
			response = request(ctx, client, http.MethodPost, publicURL+"/v1/responses", "{}", nil)
			Expect(response.StatusCode).To(Equal(http.StatusOK))
			Expect(readBody(response)).To(ContainSubstring("total_tokens"))
			Expect(request(ctx, client, http.MethodGet, publicURL+"/v1/models", "", nil).StatusCode).To(Equal(http.StatusTooManyRequests))
			response = request(ctx, client, http.MethodPatch, adminURL+"/config", `{"rate_limit":{"backend":"memory"}}`, nil)
			Expect(response.StatusCode).To(Equal(http.StatusBadRequest))
			Expect(readBody(request(ctx, client, http.MethodGet, adminURL+"/config", "", nil))).To(ContainSubstring(second.URL))

			Expect(process.session.Command.Process.Signal(syscall.SIGTERM)).To(Succeed())
			Eventually(process.session, requestTimeout).Should(gexec.Exit(0))
			process = launchRouter(ctx, first.URL, options)
			Eventually(func() []string { return listenerAddresses(process.session.Err.Contents()) }, requestTimeout).Should(HaveLen(3))
			addresses = listenerAddresses(process.session.Err.Contents())
			publicURL, adminURL = "http://"+addresses[0], "http://"+addresses[1]
			Expect(readBody(request(ctx, client, http.MethodGet, adminURL+"/config", "", nil))).To(ContainSubstring(second.URL))

			replacement := filepath.Join(filepath.Dir(file), "next.yaml")
			Expect(os.WriteFile(replacement, fmt.Appendf(nil, "upstream:\n  base_urls: [%q]\nrate_limit:\n  requests_per_second: 1000\n  burst: 100\n", first.URL), 0600)).To(Succeed())
			Expect(os.Rename(replacement, file)).To(Succeed())
			Eventually(func(g Gomega) {
				response := request(ctx, client, http.MethodGet, publicURL+"/v1/models", "", nil)
				g.Expect(response.StatusCode).To(Equal(http.StatusOK))
				g.Expect(readBody(response)).To(Equal("first"))
			}, requestTimeout).Should(Succeed())
		},
		Entry("memory", SpecTimeout(20*time.Second), "memory"),
		Entry("Redis", SpecTimeout(20*time.Second), "redis"),
	)
})
