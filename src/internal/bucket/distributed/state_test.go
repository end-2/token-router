package distributed

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"token-router/src/internal/bucket"
	"token-router/src/internal/config"
)

func applyDistributedState(t *testing.T, f *distributedFSM, command distributedCommand) bucket.Decision {
	t.Helper()
	data, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	result := f.Apply(&raft.Log{Data: data})
	decision, ok := result.(bucket.Decision)
	if !ok {
		t.Fatalf("invalid apply result: %v", result)
	}
	return decision
}

func TestDistributedStateRefillDebtAndLimits(t *testing.T) {
	f := &distributedFSM{}
	start := time.Unix(1700000000, 0)
	limits := config.LimitValues{RequestsPerSecond: 1, Burst: 2, TokensPerSecond: 2, TokenBurst: 4}
	for _, tc := range []struct {
		name, account, op string
		tokens            int64
		after             time.Duration
		allow             bool
		retry             time.Duration
	}{
		{"admission", "alice", "allow", 0, 0, true, 0},
		{"usage debt", "alice", "usage", 7, 0, false, 0},
		{"debt denial", "alice", "allow", 0, 0, false, 2 * time.Second},
		{"independent", "bob", "allow", 0, 0, true, 0},
		{"partial refill", "alice", "allow", 0, time.Second, false, time.Second},
		{"refilled", "alice", "allow", 0, 2 * time.Second, true, 0},
		{"backwards clock", "alice", "allow", 0, time.Second, true, 0},
		{"no double refill", "alice", "allow", 0, 2 * time.Second, false, time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision := applyDistributedState(t, f, distributedCommand{distributedProposal: distributedProposal{
				Account: tc.account, Operation: tc.op, Tokens: tc.tokens, Limits: limits,
			}, Now: start.Add(tc.after).UnixNano()})
			if decision.Allowed != tc.allow || decision.RetryAfter != tc.retry {
				t.Fatalf("decision %+v, want allow=%v retry=%v", decision, tc.allow, tc.retry)
			}
		})
	}
	limits.Burst = 20
	limits.RequestsPerSecond = 2
	decision := applyDistributedState(t, f, distributedCommand{distributedProposal: distributedProposal{
		Account: "alice", Operation: "allow", Limits: limits,
	}, Now: start.Add(2 * time.Second).UnixNano()})
	if decision.Allowed || decision.RetryAfter != 500*time.Millisecond {
		t.Fatalf("capacity update reset balance: %+v", decision)
	}
}

func TestDistributedSnapshotAndAccountCleanup(t *testing.T) {
	original := &distributedFSM{}
	start := time.Unix(1700000000, 0)
	limits := config.LimitValues{RequestsPerSecond: 1, Burst: 1, TokensPerSecond: 1, TokenBurst: 1}
	for _, account := range []string{"idle", "debtor"} {
		applyDistributedState(t, original, distributedCommand{distributedProposal: distributedProposal{
			Account: account, Operation: "allow", Limits: limits,
		}, Now: start.UnixNano()})
	}
	applyDistributedState(t, original, distributedCommand{distributedProposal: distributedProposal{
		Account: "debtor", Operation: "usage", Tokens: 1000, Limits: limits,
	}, Now: start.UnixNano()})
	snapshot, err := original.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	sink := &memorySnapshotSink{}
	if err := snapshot.Persist(sink); err != nil || !sink.closed {
		t.Fatalf("persist: %v, closed=%v", err, sink.closed)
	}
	snapshot.Release()
	restored := &distributedFSM{}
	if err := restored.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatal(err)
	}
	// A restore must preserve cleanup timing as well as balances; otherwise a
	// full old bucket can be recreated at a different capacity on one replica.
	limits.Burst = 10
	for _, elapsed := range []time.Duration{30 * time.Second, 2 * time.Minute} {
		command := distributedCommand{distributedProposal: distributedProposal{
			Account: "idle", Operation: "allow", Limits: limits,
		}, Now: start.Add(elapsed).UnixNano()}
		if a, b := applyDistributedState(t, original, command), applyDistributedState(t, restored, command); a != b {
			t.Fatalf("snapshot diverged: %+v vs %+v", a, b)
		}
		if !reflect.DeepEqual(original.states, restored.states) || original.nextSweep != restored.nextSweep {
			t.Fatal("snapshot restored a different state")
		}
	}
	if _, ok := restored.states["debtor"]; !ok {
		t.Fatal("cleanup removed token debt")
	}
	if err := restored.Restore(io.NopCloser(bytes.NewBufferString("invalid"))); err == nil {
		t.Fatal("invalid snapshot accepted")
	}
}

type memorySnapshotSink struct {
	bytes.Buffer
	closed bool
}

func (*memorySnapshotSink) ID() string    { return "test-snapshot" }
func (*memorySnapshotSink) Cancel() error { return nil }
func (s *memorySnapshotSink) Close() error {
	s.closed = true
	return nil
}
