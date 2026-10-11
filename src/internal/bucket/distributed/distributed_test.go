package distributed

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"token-router/src/internal/bucket"
	"token-router/src/internal/config"
)

type distributedTestNode struct {
	limiter *Limiter
	server  *httptest.Server
	cfg     config.RateLimit
}

func TestDistributedNoopUsageChecksLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rate   float64
		tokens int64
	}{
		{name: "disabled", tokens: 10},
		{name: "zero usage", rate: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := newAdminStream("", "", "", nil)
			t.Cleanup(stream.cancel)
			l := &Limiter{stream: stream, timeout: time.Second,
				limits: config.LimitValues{TokensPerSecond: tc.rate}}

			if err := l.RecordUsage(t.Context(), tc.tokens); err != nil {
				t.Fatalf("no-op usage: %v", err)
			}

			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := l.RecordUsage(ctx, tc.tokens); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled no-op usage: %v", err)
			}

			stream.cancel()
			if err := l.RecordUsage(t.Context(), tc.tokens); !errors.Is(err, bucket.ErrClosed) {
				t.Fatalf("closed no-op usage: %v", err)
			}
		})
	}
}

func distributedCluster(t *testing.T, size int, configure func(*config.RateLimit)) []*distributedTestNode {
	t.Helper()
	nodes := make([]*distributedTestNode, size)
	peers := make([]string, size)
	for i := range nodes {
		server := httptest.NewUnstartedServer(nil)
		peers[i] = server.Listener.Addr().String()
		nodes[i] = &distributedTestNode{server: server}
		t.Cleanup(func() {
			if nodes[i].limiter != nil {
				if err := nodes[i].limiter.Close(); err != nil {
					t.Error(err)
				}
			}
			nodes[i].server.Close()
		})
	}

	for i, node := range nodes {
		cfg := config.Defaults().RateLimit
		cfg.Backend = "memory"
		cfg.Memory.Distributed = config.Distributed{Enabled: true, Address: peers[i], Peers: peers, DataDir: t.TempDir(), Token: "test-cluster-secret", RequestTimeout: 2 * time.Second}
		cfg.RequestsPerSecond, cfg.Burst = .001, 10
		if configure != nil {
			configure(&cfg)
		}
		node.cfg = cfg
		startDistributedNode(t, node)
	}

	waitDistributedLeader(t, nodes)
	return nodes
}

func startDistributedNode(t *testing.T, node *distributedTestNode) {
	t.Helper()
	limiter, err := New(node.cfg)
	if err != nil {
		t.Fatal(err)
	}
	node.limiter = limiter

	mux := http.NewServeMux()
	node.limiter.RegisterAdmin(mux)
	node.server.Config.Handler = mux
	// Raft streams must survive HTTP deadlines after CONNECT hands them off.
	node.server.Config.ReadTimeout = 100 * time.Millisecond
	node.server.Config.WriteTimeout = 10 * time.Second
	node.server.Start()
}

func waitDistributedLeader(t *testing.T, nodes []*distributedTestNode) int {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		leader := -1
		for i, node := range nodes {
			if node.limiter != nil && node.limiter.raft.State() == raft.Leader {
				leader = i
			}
		}

		if leader >= 0 {
			known := true
			for _, node := range nodes {
				if node.limiter != nil {
					address, _ := node.limiter.raft.LeaderWithID()
					known = known && string(address) == nodes[leader].cfg.Memory.Distributed.Address
				}
			}
			if known && nodes[leader].limiter.raft.VerifyLeader().Error() == nil {
				return leader
			}
		}

		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("distributed cluster did not elect a leader")
	return -1
}

func stopDistributedNode(t *testing.T, node *distributedTestNode) {
	t.Helper()
	if err := node.limiter.Close(); err != nil {
		t.Fatal(err)
	}
	node.limiter = nil
	node.server.Close()
}

func TestDistributedSharedAccountsAndFailover(t *testing.T) {
	nodes := distributedCluster(t, 3, func(cfg *config.RateLimit) {
		cfg.TokensPerSecond, cfg.TokenBurst = .001, 5
	})

	alice := bucket.WithAccount(t.Context(), "alice")
	bob := bucket.WithAccount(t.Context(), "bob")

	var allowed atomic.Int32
	var wg sync.WaitGroup
	for i := range 30 {
		wg.Go(func() {
			decision, err := nodes[i%3].limiter.Allow(alice)
			if err != nil {
				t.Error(err)
			} else if decision.Allowed {
				allowed.Add(1)
			}
		})
	}
	wg.Wait()

	if allowed.Load() != 10 {
		t.Fatalf("shared burst allowed %d, want 10", allowed.Load())
	}

	if err := nodes[1].limiter.RecordUsage(bob, 8); err != nil {
		t.Fatal(err)
	}

	leader := waitDistributedLeader(t, nodes)
	follower := nodes[(leader+1)%len(nodes)]
	values := (config.Config{RateLimit: follower.cfg}).Mutable().RateLimit
	values.Burst = 20
	follower.limiter.Update(values)
	if _, err := follower.limiter.Allow(alice); !errors.Is(err, bucket.ErrUnavailable) {
		t.Fatalf("inconsistent policy must fail closed: %v", err)
	}

	for _, node := range nodes {
		node.limiter.Update(values)
	}

	stopDistributedNode(t, nodes[leader])
	newLeader := waitDistributedLeader(t, nodes)
	for _, node := range nodes {
		if node.limiter == nil {
			continue
		}
		for _, ctx := range []context.Context{alice, bob} {
			decision, err := node.limiter.Allow(ctx)
			if err != nil || decision.Allowed || decision.RetryAfter <= 0 {
				t.Fatalf("failover lost balance: %+v, %v", decision, err)
			}
		}

		decision, err := node.limiter.Allow(bucket.WithAccount(t.Context(), "charlie"))
		if err != nil || !decision.Allowed {
			t.Fatalf("independent account denied: %+v, %v", decision, err)
		}
	}

	// Losing another voter removes quorum. A surviving former leader must not
	// admit requests even before it notices its leadership lease has expired.
	for i, node := range nodes {
		if node.limiter != nil && i != newLeader {
			stopDistributedNode(t, node)
			break
		}
	}

	for _, node := range nodes {
		if node.limiter != nil {
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			decision, err := node.limiter.Allow(bucket.WithAccount(ctx, "fresh-account"))
			if err == nil || decision.Allowed {
				t.Fatalf("minority admitted a request: %+v, %v", decision, err)
			}
		}
	}
}

func TestDistributedRestartsPreserveState(t *testing.T) {
	for _, snapshot := range []bool{false, true} {
		t.Run(map[bool]string{false: "log", true: "snapshot"}[snapshot], func(t *testing.T) {
			nodes := distributedCluster(t, 3, func(cfg *config.RateLimit) { cfg.Burst = 1 })
			ctx := bucket.WithAccount(t.Context(), "persisted-account")

			if decision, err := nodes[0].limiter.Allow(ctx); err != nil || !decision.Allowed {
				t.Fatalf("initial allow: %+v, %v", decision, err)
			}

			leader := waitDistributedLeader(t, nodes)
			if err := nodes[leader].limiter.raft.Barrier(2 * time.Second).Error(); err != nil {
				t.Fatal(err)
			}
			if snapshot {
				if err := nodes[leader].limiter.raft.Snapshot().Error(); err != nil {
					t.Fatal(err)
				}
			}

			for _, node := range nodes {
				stopDistributedNode(t, node)
			}

			for _, node := range nodes {
				listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", node.cfg.Memory.Distributed.Address)
				if err != nil {
					t.Fatal(err)
				}
				node.server = &httptest.Server{Listener: listener, Config: &http.Server{ReadHeaderTimeout: time.Second}}
				startDistributedNode(t, node)
			}
			waitDistributedLeader(t, nodes)

			for _, node := range nodes {
				decision, err := node.limiter.Allow(ctx)
				if err != nil || decision.Allowed {
					t.Fatalf("restart reset balance: %+v, %v", decision, err)
				}
			}
		})
	}
}

func TestDistributedAdminAuthenticationAndValidation(t *testing.T) {
	node := distributedCluster(t, 1, nil)[0]
	for _, tc := range []struct {
		name, method, path, body, token string
		status                          int
	}{
		{"no token", "POST", "apply", `{}`, "", 401},
		{"wrong token", "CONNECT", "raft", "", "wrong", 401},
		{"bad operation", "POST", "apply", `{"operation":"delete"}`, "test-cluster-secret", 400},
		{"negative usage", "POST", "apply", `{"operation":"usage","tokens":-1}`, "test-cluster-secret", 400},
		{"invalid account", "POST", "apply", `{"operation":"allow","account":"raw-secret"}`, "test-cluster-secret", 400},
		{"trailing JSON", "POST", "apply", `{"operation":"allow"}{}`, "test-cluster-secret", 400},
		{"bad limits", "POST", "apply", `{"operation":"allow"}`, "test-cluster-secret", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), tc.method, distributedPath+tc.path, strings.NewReader(tc.body))
			r.Header.Set("Authorization", "Bearer "+tc.token)
			r.Header.Set("X-Token-Router-Cluster", node.limiter.stream.cluster)
			w := httptest.NewRecorder()
			node.server.Config.Handler.ServeHTTP(w, r)

			if w.Code != tc.status || strings.Contains(w.Body.String(), "secret") {
				t.Fatalf("status %d, body %s", w.Code, w.Body.String())
			}
		})
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := node.limiter.Allow(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled request: %v", err)
	}

	if err := node.limiter.RecordUsage(t.Context(), -1); !errors.Is(err, bucket.ErrInvalidUsage) {
		t.Fatal(err)
	}

	limiter := node.limiter
	stopDistributedNode(t, node)
	if _, err := limiter.Allow(t.Context()); !errors.Is(err, bucket.ErrClosed) {
		t.Fatalf("closed limiter: %v", err)
	}

	node.cfg.Memory.Distributed.Peers = append(node.cfg.Memory.Distributed.Peers, "127.0.0.1:12345")
	if changed, err := New(node.cfg); err == nil {
		if closeErr := changed.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal("changed membership accepted existing data directory")
	}
}
