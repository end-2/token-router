package bucket

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"go.etcd.io/bbolt"

	"token-router/src/internal/config"
)

type distributedLimiter struct {
	mu        sync.RWMutex
	limits    config.LimitValues
	raft      *raft.Raft
	store     *raftboltdb.BoltStore
	transport *raft.NetworkTransport
	stream    *adminStream
	client    *http.Client
	timeout   time.Duration
	queue     chan distributedCall
	done      chan struct{}
	once      sync.Once
	closeErr  error
}

type distributedCall struct {
	ctx      context.Context
	proposal distributedProposal
	result   chan distributedResult
}

type distributedResult struct {
	decision Decision
	err      error
}

func newDistributed(cfg config.RateLimit) (_ *distributedLimiter, err error) {
	d := cfg.Memory.Distributed
	if err := d.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(d.DataDir, 0700); err != nil {
		return nil, fmt.Errorf("create distributed data directory: %w", ErrUnavailable)
	}
	store, err := raftboltdb.New(raftboltdb.Options{Path: filepath.Join(d.DataDir, "raft.db"),
		BoltOptions: &bbolt.Options{Timeout: d.RequestTimeout}})
	if err != nil {
		return nil, fmt.Errorf("open distributed storage: %w", ErrUnavailable)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, store.Close())
		}
	}()
	peers := slices.Clone(d.Peers)
	slices.Sort(peers)
	digest := sha256.Sum256([]byte(strings.Join(peers, "\n")))
	cluster := hex.EncodeToString(digest[:])
	// Persist identity under the same file lock as the Raft state. Restarting a
	// data directory with another identity or voter set cannot bootstrap safely.
	identity := []byte(d.Address + "\n" + cluster)
	stored, err := store.Get([]byte("token-router-identity"))
	if err != nil && !errors.Is(err, raftboltdb.ErrKeyNotFound) {
		return nil, fmt.Errorf("read distributed identity: %w", ErrUnavailable)
	}
	if len(stored) > 0 && !bytes.Equal(stored, identity) {
		return nil, errors.New("distributed address and peers must match the persisted cluster identity")
	}
	if err := store.Set([]byte("token-router-identity"), identity); err != nil {
		return nil, fmt.Errorf("persist distributed identity: %w", ErrUnavailable)
	}
	snapshots, err := raft.NewFileSnapshotStore(d.DataDir, 2, io.Discard)
	if err != nil {
		return nil, fmt.Errorf("open distributed snapshots: %w", ErrUnavailable)
	}
	stream := newAdminStream(d.Address, d.Token, cluster, peers)
	transport := raft.NewNetworkTransport(stream, 3, d.RequestTimeout, io.Discard)
	defer func() {
		if err != nil {
			err = errors.Join(err, transport.Close())
		}
	}()
	raftConfig := raft.DefaultConfig()
	raftConfig.LocalID = raft.ServerID(d.Address)
	raftConfig.Logger = hclog.NewNullLogger()
	exists, err := raft.HasExistingState(store, store, snapshots)
	if err != nil {
		return nil, fmt.Errorf("read distributed state: %w", ErrUnavailable)
	}
	if !exists {
		membership := raft.Configuration{}
		for _, peer := range peers {
			membership.Servers = append(membership.Servers, raft.Server{Suffrage: raft.Voter,
				ID: raft.ServerID(peer), Address: raft.ServerAddress(peer)})
		}
		if err := raft.BootstrapCluster(raftConfig, store, store, snapshots, transport, membership); err != nil {
			return nil, fmt.Errorf("bootstrap distributed cluster: %w", ErrUnavailable)
		}
	}
	node, err := raft.NewRaft(raftConfig, &distributedFSM{}, store, store, snapshots, transport)
	if err != nil {
		return nil, fmt.Errorf("start distributed node: %w", ErrUnavailable)
	}
	l := &distributedLimiter{limits: (config.Config{RateLimit: cfg}).Mutable().RateLimit,
		raft: node, store: store, transport: transport, stream: stream, timeout: d.RequestTimeout,
		queue: make(chan distributedCall, 128), done: make(chan struct{}),
		client: &http.Client{
			Transport:     &http.Transport{MaxIdleConns: 16, MaxIdleConnsPerHost: 16, IdleConnTimeout: time.Minute},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	go l.applyLoop()
	return l, nil
}

func (l *distributedLimiter) Update(values config.LimitValues) {
	l.mu.Lock()
	l.limits = values
	l.mu.Unlock()
}

func (l *distributedLimiter) Allow(ctx context.Context) (Decision, error) {
	return l.submit(ctx, "allow", 0)
}

func (l *distributedLimiter) RecordUsage(ctx context.Context, tokens int64) error {
	if tokens < 0 {
		return ErrInvalidUsage
	}
	_, err := l.submit(ctx, "usage", tokens)
	return err
}

func (l *distributedLimiter) submit(ctx context.Context, operation string, tokens int64) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	if l.stream.ctx.Err() != nil {
		return Decision{}, ErrClosed
	}
	l.mu.RLock()
	proposal := distributedProposal{Account: accountKey(ctx), Operation: operation, Tokens: tokens, Limits: l.limits}
	l.mu.RUnlock()
	if operation == "usage" && (tokens == 0 || proposal.Limits.TokensPerSecond == 0) {
		return Decision{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	stop := context.AfterFunc(l.stream.ctx, cancel)
	defer stop()
	if l.raft.State() == raft.Leader {
		return l.apply(ctx, proposal)
	}
	leader, _ := l.raft.LeaderWithID()
	if leader == "" || !l.stream.peers[string(leader)] || string(leader) == l.stream.address {
		return Decision{}, ErrUnavailable
	}
	data, err := json.Marshal(proposal)
	if err != nil {
		return Decision{}, ErrUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+string(leader)+distributedPath+"apply", bytes.NewReader(data))
	if err != nil {
		return Decision{}, ErrUnavailable
	}
	// No redirects or application retries: an ambiguous reply may already have
	// committed, so resubmitting would debit the same request or usage twice.
	l.stream.headers(request)
	request.Header.Set("Content-Type", "application/json")
	response, err := l.client.Do(request)
	if err != nil {
		return Decision{}, distributedError(ctx)
	}
	defer func() {
		// The operation's result is determined by the reply, not idle cleanup.
		_ = response.Body.Close()
	}()
	if response.StatusCode != http.StatusOK {
		return Decision{}, ErrUnavailable
	}
	var decision Decision
	if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&decision); err != nil || decision.RetryAfter < 0 {
		return Decision{}, ErrUnavailable
	}
	return decision, nil
}

func distributedError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrUnavailable
}

func (l *distributedLimiter) apply(ctx context.Context, proposal distributedProposal) (Decision, error) {
	call := distributedCall{ctx: ctx, proposal: proposal, result: make(chan distributedResult, 1)}
	select {
	case l.queue <- call:
	case <-ctx.Done():
		return Decision{}, ctx.Err()
	case <-l.stream.ctx.Done():
		return Decision{}, ErrClosed
	}
	select {
	case result := <-call.result:
		return result.decision, result.err
	case <-ctx.Done():
		return Decision{}, ctx.Err()
	case <-l.stream.ctx.Done():
		return Decision{}, ErrClosed
	}
}

func (l *distributedLimiter) applyLoop() {
	defer close(l.done)
	for {
		select {
		case <-l.stream.ctx.Done():
			return
		case call := <-l.queue:
			decision, err := l.commit(call.ctx, call.proposal)
			call.result <- distributedResult{decision: decision, err: err}
		}
	}
}

func (l *distributedLimiter) commit(ctx context.Context, proposal distributedProposal) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	l.mu.RLock()
	limits := l.limits
	l.mu.RUnlock()
	// Identity settings are local admission policy. Already admitted streams keep
	// their original account even if that policy changes while streaming.
	if proposal.Operation == "usage" {
		proposal.Limits.Account = limits.Account
	}
	if proposal.Limits != limits || l.raft.State() != raft.Leader {
		return Decision{}, ErrUnavailable
	}
	command := distributedCommand{distributedProposal: proposal, Now: time.Now().UnixNano()}
	data, err := json.Marshal(command)
	if err != nil {
		return Decision{}, ErrUnavailable
	}
	deadline, _ := ctx.Deadline()
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return Decision{}, context.DeadlineExceeded
	}
	// Apply's timeout only bounds enqueueing. The caller's context separately
	// bounds its wait; leadership loss or Shutdown releases this single worker.
	future := l.raft.Apply(data, remaining)
	if err := future.Error(); err != nil {
		return Decision{}, distributedError(ctx)
	}
	decision, ok := future.Response().(Decision)
	if !ok {
		return Decision{}, ErrUnavailable
	}
	return decision, nil
}

func (l *distributedLimiter) RegisterAdmin(mux *http.ServeMux) {
	mux.HandleFunc("CONNECT "+distributedPath+"raft", l.stream.serveConnect)
	mux.HandleFunc("POST "+distributedPath+"apply", l.serveApply)
}

func (l *distributedLimiter) serveApply(w http.ResponseWriter, r *http.Request) {
	if !l.stream.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var proposal distributedProposal
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&proposal); err != nil || decoder.Decode(new(any)) != io.EOF ||
		(proposal.Operation != "allow" && proposal.Operation != "usage") || proposal.Tokens < 0 ||
		(proposal.Operation == "allow" && proposal.Tokens != 0) {
		http.Error(w, "invalid operation", http.StatusBadRequest)
		return
	}
	if proposal.Account != "" {
		decoded, err := hex.DecodeString(proposal.Account)
		if err != nil || len(decoded) != sha256.Size {
			http.Error(w, "invalid account", http.StatusBadRequest)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), l.timeout)
	defer cancel()
	decision, err := l.apply(ctx, proposal)
	if err != nil {
		http.Error(w, "rate limiter unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// A failed response cannot undo a committed operation and must not be retried.
	if err := json.NewEncoder(w).Encode(decision); err != nil {
		return
	}
}

func (l *distributedLimiter) Close() error {
	l.once.Do(func() {
		l.stream.cancel()
		l.client.CloseIdleConnections()
		l.closeErr = errors.Join(l.raft.Shutdown().Error(), l.transport.Close())
		<-l.done
		l.closeErr = errors.Join(l.closeErr, l.store.Close())
	})
	return l.closeErr
}
