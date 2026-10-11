//go:build e2e

package e2e_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"
)

var _ = Describe("streaming and process shutdown", func() {
	It("delivers SSE events incrementally and propagates downstream cancellation", func(ctx SpecContext) {
		nextEvent := make(chan struct{})
		canceled := make(chan struct{})
		upstream := newUpstream(func(writer http.ResponseWriter, req *http.Request) {
			writer.Header().Set("Content-Type", "text/event-stream")
			writeEvent(writer, "first")
			select {
			case <-nextEvent:
				writeEvent(writer, "second")
			case <-req.Context().Done():
				close(canceled)
				return
			}
			<-req.Context().Done()
			close(canceled)
		})
		process := startRouter(ctx, upstream.URL, routerOptions{})
		requestCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		response := request(requestCtx, newHTTPClient(), http.MethodPost, process.url+"/v1/chat/completions", `{"stream":true}`, nil)
		Expect(response.StatusCode).To(Equal(http.StatusOK))
		Expect(response.Header.Get("Content-Type")).To(Equal("text/event-stream"))
		reader := bufio.NewReader(response.Body)
		Expect(readEvent(reader)).To(Equal("data: first"))
		// The upstream cannot produce the second event until the client receives the first.
		close(nextEvent)
		Expect(readEvent(reader)).To(Equal("data: second"))
		cancel()
		Expect(response.Body.Close()).To(Succeed())
		Eventually(canceled, requestTimeout).Should(BeClosed())
	}, SpecTimeout(15*time.Second))

	It("drains an active SSE response after SIGTERM and exits successfully", func(ctx SpecContext) {
		finish := make(chan struct{})
		canceled := make(chan struct{})
		upstream := newUpstream(func(writer http.ResponseWriter, req *http.Request) {
			writer.Header().Set("Content-Type", "text/event-stream")
			writeEvent(writer, "first")
			select {
			case <-finish:
				writeEvent(writer, "[DONE]")
			case <-req.Context().Done():
				close(canceled)
			}
		})
		process := startRouter(ctx, upstream.URL, routerOptions{})
		response := request(ctx, newHTTPClient(), http.MethodPost, process.url+"/v1/chat/completions", `{"stream":true}`, nil)
		reader := bufio.NewReader(response.Body)
		Expect(readEvent(reader)).To(Equal("data: first"))
		Expect(process.session.Command.Process.Signal(syscall.SIGTERM)).To(Succeed())
		Eventually(process.session.Err, requestTimeout).Should(gbytes.Say("shutting down HTTP listeners"))
		Consistently(process.session, 150*time.Millisecond).ShouldNot(gexec.Exit(), func() string {
			upstreamCanceled := false
			select {
			case <-canceled:
				upstreamCanceled = true
			default:
			}
			return fmt.Sprintf("active SSE drain: exit code=%d, upstream canceled=%t, request context=%v",
				process.session.ExitCode(), upstreamCanceled, ctx.Err())
		})
		Expect(canceled).NotTo(BeClosed())
		close(finish)
		Expect(readEvent(reader)).To(Equal("data: [DONE]"))
		remainder, err := io.ReadAll(reader)
		Expect(err).NotTo(HaveOccurred())
		Expect(remainder).To(BeEmpty())
		Eventually(process.session, requestTimeout).Should(gexec.Exit(0))
	}, SpecTimeout(15*time.Second))

	It("forces an unfinished SSE request to close when the shutdown deadline expires", func(ctx SpecContext) {
		canceled := make(chan struct{})
		upstream := newUpstream(func(writer http.ResponseWriter, req *http.Request) {
			writer.Header().Set("Content-Type", "text/event-stream")
			writeEvent(writer, "first")
			<-req.Context().Done()
			close(canceled)
		})
		process := startRouter(ctx, upstream.URL, routerOptions{flags: []string{"--server-shutdown-timeout", "250ms"}})
		response := request(ctx, newHTTPClient(), http.MethodPost, process.url+"/v1/chat/completions", `{"stream":true}`, nil)
		reader := bufio.NewReader(response.Body)
		Expect(readEvent(reader)).To(Equal("data: first"))
		Expect(process.session.Command.Process.Signal(syscall.SIGTERM)).To(Succeed())
		Eventually(process.session, requestTimeout).Should(gexec.Exit(1))
		Expect(process.session.Err).To(gbytes.Say("HTTP shutdown: context deadline exceeded"))
		_, err := io.ReadAll(reader)
		Expect(err).To(HaveOccurred(), "forced shutdown must interrupt the unfinished chunked response")
		Eventually(canceled, requestTimeout).Should(BeClosed())
	}, SpecTimeout(15*time.Second))
})
