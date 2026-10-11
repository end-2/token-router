package app

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"token-router/src/internal/config"
)

func TestDistributedServerAdminAndAccounts(t *testing.T) {
	address := func() string {
		t.Helper()
		listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := listener.Addr().String()
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		return addr
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	cfg := config.Defaults()
	cfg.Server.Address, cfg.Admin.Address = address(), address()
	cfg.Admin.Enabled = true
	cfg.Upstream.Routes[0].BaseURLs = []string{upstream.URL}
	cfg.RateLimit.RequestsPerSecond, cfg.RateLimit.Burst = .001, 1
	cfg.RateLimit.Account.Enabled = true
	cfg.RateLimit.Memory.Distributed = config.Distributed{Enabled: true, Address: cfg.Admin.Address,
		Peers: []string{cfg.Admin.Address}, DataDir: t.TempDir(), Token: "cluster-secret", RequestTimeout: time.Second}
	cfg.Telemetry.Metrics.Enabled, cfg.Telemetry.Logs.Enabled = false, false
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("distributed app did not shut down")
		}
	})
	client := &http.Client{Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()
	request := func(addr, method, path, auth string) int {
		t.Helper()
		r, err := http.NewRequestWithContext(t.Context(), method, "http://"+addr+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", auth)
		response, err := client.Do(r)
		if err != nil {
			return 0
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
		return response.StatusCode
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		status := request(cfg.Server.Address, "GET", "/v1/models", accountBasic("alice", "pw"))
		if status == 204 {
			break
		}
		if (status != 0 && status != 503) || time.Now().After(deadline) {
			t.Fatalf("server did not elect leader, status %d", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, tc := range []struct {
		address, method, path, auth string
		status                      int
	}{
		{cfg.Server.Address, "GET", "/v1/models", accountJWT(`{"sub":"alice"}`), 429},
		{cfg.Server.Address, "GET", "/v1/models", accountJWT(`{"sub":"bob"}`), 204},
		{cfg.Server.Address, "GET", "/v1/models", "", 401},
		{cfg.Admin.Address, "GET", "/config", "", 200},
		{cfg.Admin.Address, "POST", "/internal/rate-limit/apply", "", 401},
		{cfg.Server.Address, "POST", "/internal/rate-limit/apply", "", 404},
	} {
		if status := request(tc.address, tc.method, tc.path, tc.auth); status != tc.status {
			t.Errorf("%s %s: %d, want %d", tc.method, tc.path, status, tc.status)
		}
	}
}
