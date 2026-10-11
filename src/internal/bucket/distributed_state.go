package bucket

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"sync"
	"time"

	"github.com/hashicorp/raft"

	"token-router/src/internal/config"
)

type distributedProposal struct {
	Account   string             `json:"account"`
	Operation string             `json:"operation"`
	Tokens    int64              `json:"tokens"`
	Limits    config.LimitValues `json:"limits"`
}

type distributedCommand struct {
	distributedProposal
	Now int64 `json:"now"`
}

type distributedState struct {
	Initialized bool               `json:"initialized"`
	Updated     int64              `json:"updated"`
	Requests    float64            `json:"requests"`
	Tokens      float64            `json:"tokens"`
	Limits      config.LimitValues `json:"limits"`
}

type distributedFSM struct {
	mu        sync.Mutex
	states    map[string]distributedState
	nextSweep int64
}

func (f *distributedFSM) Apply(log *raft.Log) any {
	var command distributedCommand
	if err := json.Unmarshal(log.Data, &command); err != nil {
		return ErrUnavailable
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.states == nil {
		f.states = make(map[string]distributedState)
	}
	if command.Now >= f.nextSweep {
		for key, state := range f.states {
			elapsed := time.Unix(0, command.Now).Sub(time.Unix(0, state.Updated)).Seconds()
			if elapsed >= 0 && state.Requests+elapsed*state.Limits.RequestsPerSecond >= float64(state.Limits.Burst) &&
				(state.Limits.TokensPerSecond == 0 || state.Tokens+elapsed*state.Limits.TokensPerSecond >= float64(state.Limits.TokenBurst)) {
				delete(f.states, key)
			}
		}
		f.nextSweep = command.Now + int64(time.Minute)
	}
	state := f.states[command.Account]
	s := &state
	defer func() { f.states[command.Account] = state }()
	if !s.Initialized {
		*s = distributedState{Initialized: true, Updated: command.Now,
			Requests: float64(command.Limits.Burst), Tokens: float64(command.Limits.TokenBurst), Limits: command.Limits}
	}
	// Only the leader supplies time. Clamping prevents a backwards clock on a
	// new leader from refilling the same interval twice.
	now := max(s.Updated, command.Now)
	elapsed := time.Unix(0, now).Sub(time.Unix(0, s.Updated)).Seconds()
	s.Requests = math.Min(float64(s.Limits.Burst), s.Requests+elapsed*s.Limits.RequestsPerSecond)
	if s.Limits.TokensPerSecond > 0 {
		s.Tokens = math.Min(float64(s.Limits.TokenBurst), s.Tokens+elapsed*s.Limits.TokensPerSecond)
	} else {
		s.Tokens = float64(command.Limits.TokenBurst)
	}
	s.Requests = math.Min(s.Requests, float64(command.Limits.Burst))
	s.Tokens = math.Min(s.Tokens, float64(command.Limits.TokenBurst))
	s.Limits, s.Updated = command.Limits, now
	if command.Operation == "usage" {
		if s.Limits.TokensPerSecond > 0 {
			s.Tokens -= float64(command.Tokens)
		}
		return Decision{}
	}
	var retry time.Duration
	if s.Requests < 1 {
		retry = usageRetry((1 - s.Requests) / s.Limits.RequestsPerSecond)
	}
	if s.Limits.TokensPerSecond > 0 && s.Tokens < 1 {
		retry = max(retry, usageRetry((1-s.Tokens)/s.Limits.TokensPerSecond))
	}
	if retry > 0 {
		return Decision{RetryAfter: retry}
	}
	s.Requests--
	return Decision{Allowed: true}
}

func (f *distributedFSM) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	states := make(map[string]distributedState, len(f.states))
	for key, state := range f.states {
		states[key] = state
	}
	return distributedSnapshot{States: states, NextSweep: f.nextSweep}, nil
}

func (f *distributedFSM) Restore(reader io.ReadCloser) error {
	var snapshot distributedSnapshot
	err := json.NewDecoder(reader).Decode(&snapshot)
	if err = errors.Join(err, reader.Close()); err != nil {
		return err
	}
	f.mu.Lock()
	f.states = snapshot.States
	f.nextSweep = snapshot.NextSweep
	f.mu.Unlock()
	return nil
}

type distributedSnapshot struct {
	States    map[string]distributedState `json:"states"`
	NextSweep int64                       `json:"next_sweep"`
}

func (s distributedSnapshot) Persist(sink raft.SnapshotSink) error {
	if err := json.NewEncoder(sink).Encode(s); err != nil {
		return errors.Join(err, sink.Cancel())
	}
	return sink.Close()
}

func (distributedSnapshot) Release() {}
