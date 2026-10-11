//go:build e2e

package e2e_test

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

const requestTimeout = 5 * time.Second

type routerOptions struct {
	yaml  string
	env   map[string]string
	flags []string
}

type routerProcess struct {
	session    *gexec.Session
	url        string
	metricsURL string
}

func launchRouter(upstreamURL string, options routerOptions) *routerProcess {
	GinkgoHelper()
	directory := GinkgoT().TempDir()
	args := []string{"serve"}
	if options.yaml != "" {
		path := filepath.Join(directory, "config.yaml")
		Expect(os.WriteFile(path, []byte(options.yaml), 0o600)).To(Succeed())
		args = append(args, "--config", path)
	}
	args = append(args, options.flags...)
	command := exec.Command(binaryPath, args...)
	command.Dir = directory
	// Isolate every process from the developer's application configuration.
	command.Env = slices.DeleteFunc(os.Environ(), func(value string) bool {
		return strings.HasPrefix(value, "TOKEN_ROUTER_") || strings.HasPrefix(value, "OTEL_")
	})
	environment := map[string]string{
		"TOKEN_ROUTER_SERVER_ADDRESS":                       "127.0.0.1:0",
		"TOKEN_ROUTER_SERVER_SHUTDOWN_TIMEOUT":              "2s",
		"TOKEN_ROUTER_UPSTREAM_BASE_URL":                    upstreamURL,
		"TOKEN_ROUTER_RATE_LIMIT_ENABLED":                   "false",
		"TOKEN_ROUTER_TELEMETRY_METRICS_PROMETHEUS_ENABLED": "true",
		"TOKEN_ROUTER_TELEMETRY_METRICS_PROMETHEUS_ADDRESS": "127.0.0.1:0",
		"TOKEN_ROUTER_TELEMETRY_LOGS_LEVEL":                 "info",
		"TOKEN_ROUTER_TELEMETRY_LOGS_FORMAT":                "json",
	}
	for key, value := range options.env {
		environment[key] = value
	}
	for key, value := range environment {
		command.Env = append(command.Env, key+"="+value)
	}
	session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	process := &routerProcess{session: session}
	DeferCleanup(func() {
		if session.ExitCode() != -1 {
			return
		}
		// A failed spec must still reap its subprocess before the next spec starts.
		defer func() {
			if session.ExitCode() == -1 {
				err := session.Command.Process.Kill()
				Expect(err == nil || errors.Is(err, os.ErrProcessDone)).To(BeTrue(), "kill subprocess: %v", err)
				Eventually(session, requestTimeout).Should(gexec.Exit())
			}
		}()
		Expect(session.Command.Process.Signal(syscall.SIGTERM)).To(Succeed())
		Eventually(session, 10*time.Second).Should(gexec.Exit(0))
	})
	return process
}

func startRouter(ctx context.Context, upstreamURL string, options routerOptions) *routerProcess {
	GinkgoHelper()
	process := launchRouter(upstreamURL, options)
	if address := options.env["TOKEN_ROUTER_SERVER_ADDRESS"]; address != "" && !strings.HasSuffix(address, ":0") {
		process.url = "http://" + address
	} else {
		Eventually(func() ([]string, error) {
			if process.session.ExitCode() != -1 {
				return nil, StopTrying(fmt.Sprintf("router exited before listening: %s", process.session.Err.Contents()))
			}
			return listenerAddresses(process.session.Err.Contents()), nil
		}, requestTimeout).Should(HaveLen(2), "both HTTP listeners must report their assigned ports")
		addresses := listenerAddresses(process.session.Err.Contents())
		process.url = "http://" + addresses[0]
		process.metricsURL = "http://" + addresses[1]
	}
	client := newHTTPClient()
	Eventually(func() (int, error) {
		if process.session.ExitCode() != -1 {
			return 0, StopTrying(fmt.Sprintf("router exited before readiness: %s", process.session.Err.Contents()))
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, process.url+"/readyz", nil)
		if err != nil {
			return 0, err
		}
		response, err := client.Do(request)
		if err != nil {
			return 0, err
		}
		return response.StatusCode, response.Body.Close()
	}, requestTimeout).Should(Equal(http.StatusOK))
	return process
}

func unusedAddress() string {
	GinkgoHelper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	address := listener.Addr().String()
	Expect(listener.Close()).To(Succeed())
	return address
}

func listenerAddresses(output []byte) []string {
	var addresses []string
	for _, line := range strings.Split(string(output), "\n") {
		var entry struct {
			Message string `json:"msg"`
			Address string `json:"address"`
		}
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Message == "HTTP listener started" {
			addresses = append(addresses, entry.Address)
		}
	}
	return addresses
}

func newHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	DeferCleanup(transport.CloseIdleConnections)
	return &http.Client{Timeout: requestTimeout, Transport: transport}
}

func newUpstream(handler http.HandlerFunc) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer GinkgoRecover()
		handler(writer, request)
	}))
	DeferCleanup(func() {
		server.CloseClientConnections()
		server.Close()
	})
	return server
}

func request(ctx context.Context, client *http.Client, method, url, body string, headers map[string]string) *http.Response {
	GinkgoHelper()
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
	Expect(err).NotTo(HaveOccurred())
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	response, err := client.Do(req)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { Expect(response.Body.Close()).To(Succeed()) })
	return response
}

func readBody(response *http.Response) string {
	GinkgoHelper()
	body, err := io.ReadAll(response.Body)
	Expect(err).NotTo(HaveOccurred())
	Expect(response.Body.Close()).To(Succeed())
	return string(body)
}

func writeBody(writer http.ResponseWriter, body string) {
	GinkgoHelper()
	_, err := io.WriteString(writer, body)
	Expect(err).NotTo(HaveOccurred())
}

func writeEvent(writer http.ResponseWriter, data string) {
	GinkgoHelper()
	writeBody(writer, "data: "+data+"\n\n")
	Expect(http.NewResponseController(writer).Flush()).To(Succeed())
}

func readEvent(reader *bufio.Reader) string {
	GinkgoHelper()
	line, err := reader.ReadString('\n')
	Expect(err).NotTo(HaveOccurred())
	separator, err := reader.ReadString('\n')
	Expect(err).NotTo(HaveOccurred())
	Expect(separator).To(Equal("\n"))
	return strings.TrimSuffix(line, "\n")
}

func redisEnvironment(key string) map[string]string {
	return map[string]string{
		"TOKEN_ROUTER_RATE_LIMIT_ENABLED":             "true",
		"TOKEN_ROUTER_RATE_LIMIT_BACKEND":             "redis",
		"TOKEN_ROUTER_RATE_LIMIT_REQUESTS_PER_SECOND": "0.001",
		"TOKEN_ROUTER_RATE_LIMIT_BURST":               "5",
		"TOKEN_ROUTER_RATE_LIMIT_REDIS_ADDRESS":       redisAddress,
		"TOKEN_ROUTER_RATE_LIMIT_REDIS_KEY":           key,
	}
}

func newRedisKey() string {
	key := "token-router:e2e:" + rand.Text()
	DeferCleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		Expect(redisClient.Del(ctx, key).Err()).To(Succeed())
	})
	return key
}
