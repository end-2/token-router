//go:build e2e

package e2e_test

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/redis/go-redis/v9"
)

var binaryPath string
var redisAddress string
var redisClient *redis.Client

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Token Router E2E Suite")
}

var _ = BeforeSuite(func(ctx SpecContext) {
	binaryPath = os.Getenv("TOKEN_ROUTER_E2E_BINARY")
	Expect(binaryPath).NotTo(BeEmpty(), "TOKEN_ROUTER_E2E_BINARY must point to the built token-router binary")
	Expect(filepath.IsAbs(binaryPath)).To(BeTrue(), "TOKEN_ROUTER_E2E_BINARY must be an absolute path")
	info, err := os.Stat(binaryPath)
	Expect(err).NotTo(HaveOccurred())
	Expect(info.Mode().IsRegular()).To(BeTrue())
	Expect(info.Mode().Perm()&0o111).NotTo(BeZero(), "the E2E binary must be executable")

	redisAddress = os.Getenv("TOKEN_ROUTER_E2E_REDIS_ADDRESS")
	Expect(redisAddress).NotTo(BeEmpty(), "TOKEN_ROUTER_E2E_REDIS_ADDRESS must point to a real Redis server")
	_, _, err = net.SplitHostPort(redisAddress)
	Expect(err).NotTo(HaveOccurred())
	redisClient = redis.NewClient(&redis.Options{
		Addr: redisAddress, DialTimeout: 2 * time.Second,
		ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second,
		ContextTimeoutEnabled: true, MaxRetries: -1,
	})
	DeferCleanup(redisClient.Close)
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	Expect(redisClient.Ping(pingCtx).Err()).To(Succeed(), "the E2E Redis server must be reachable")

	for key, destination := range map[string]*string{
		"TOKEN_ROUTER_E2E_OTLP_ENDPOINT":        &telemetry.otlp,
		"TOKEN_ROUTER_E2E_PROMETHEUS_URL":       &telemetry.prometheus,
		"TOKEN_ROUTER_E2E_TEMPO_URL":            &telemetry.tempo,
		"TOKEN_ROUTER_E2E_LOKI_URL":             &telemetry.loki,
		"TOKEN_ROUTER_E2E_COLLECTOR_HEALTH_URL": &telemetry.collectorHealth,
	} {
		*destination = os.Getenv(key)
		Expect(*destination).NotTo(BeEmpty(), "%s must point to its real telemetry service", key)
		endpoint, err := url.Parse(*destination)
		Expect(err).NotTo(HaveOccurred())
		Expect(endpoint.Scheme).To(Equal("http"), "%s must use the test HTTP endpoint", key)
		Expect(endpoint.Host).NotTo(BeEmpty())
	}
	client := newHTTPClient()
	for _, endpoint := range []string{
		telemetry.collectorHealth,
		telemetry.prometheus + "/-/ready",
		telemetry.tempo + "/ready",
		telemetry.loki + "/ready",
	} {
		Eventually(func() (int, error) {
			_, status, err := getBackend(ctx, client, endpoint)
			return status, err
		}, telemetryTimeout, 250*time.Millisecond).Should(Equal(http.StatusOK), "service readiness: %s", endpoint)
	}
}, NodeTimeout(120*time.Second))
