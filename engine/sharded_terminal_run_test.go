package engine

import (
	"context"
	"testing"
)

// TestShardedGetTerminalRunCrossesAShardBoundary is the case ShardedStore's
// GetTerminalRun exists for, and the one its own doc comment promises:
//
//	"a chain is NOT shard-local: every continuation gets a fresh id, and
//	 getShard hashes the id, so successive runs of one chain routinely land on
//	 different shards"
//
// Two shards, one two-run chain split across them. run1 lives on shard-0 and
// continued into run2, which lives on shard-1. Each shard models a real store:
// it answers only about ids it holds, and returns nil for anything else --
// which is exactly what walkToTerminalRun does when its head lookup misses.
func TestShardedGetTerminalRunCrossesAShardBoundary(t *testing.T) {
	const run1, run2 = "run-1", "run-2"

	// shard-0 holds run1 only. Its local walk finds no successor, because
	// run2 is not in its rows, so it reports run1 as the end of the chain.
	shard0 := &mockShardStore{name: "shard-0"}
	shard0.getWorkflowByIDFn = func(_ context.Context, id string) (*WorkflowInstance, error) {
		if id == run1 {
			return &WorkflowInstance{ID: run1, Status: "done", Result: "{}"}, nil
		}
		return nil, nil
	}
	// shard-0 holds no successor: run2 is not among its rows.
	shard0.successorOfRunFn = func(_ context.Context, id string) (string, error) {
		return "", nil
	}

	// shard-1 holds run2. Asked about run1 it returns nil, because its head
	// lookup misses -- a real store cannot answer for a row it does not have.
	shard1 := &mockShardStore{name: "shard-1"}
	shard1.getWorkflowByIDFn = func(_ context.Context, id string) (*WorkflowInstance, error) {
		if id == run2 {
			return &WorkflowInstance{ID: run2, Status: "done", Result: `{"outcome":"finished"}`,
				ContinuedFrom: run1}, nil
		}
		return nil, nil
	}
	// shard-1 CAN answer who continued from run1, even though it does not
	// hold run1 -- that asymmetry is the entire bug.
	shard1.successorOfRunFn = func(_ context.Context, id string) (string, error) {
		if id == run1 {
			return run2, nil
		}
		return "", nil
	}

	ss, err := NewShardedStore(
		[]ShardConfig{{Name: "shard-0"}, {Name: "shard-1"}},
		[]WorkflowStore{shard0, shard1},
		[]func() error{func() error { return nil }, func() error { return nil }},
	)
	if err != nil {
		t.Fatalf("NewShardedStore: %v", err)
	}

	got, err := ss.GetTerminalRun(context.Background(), run1)
	if err != nil {
		t.Fatalf("GetTerminalRun: %v", err)
	}
	if got == nil {
		t.Fatal("GetTerminalRun returned nil for a run that exists")
	}
	if got.ID != run2 {
		t.Errorf("GetTerminalRun(%s) = %s, want %s.\n\n"+
			"The successor lives on a different shard. Asking each shard for "+
			"GetTerminalRun(%s) cannot find it: the shard that HOLDS the successor "+
			"does not hold %s, so its head lookup misses and it returns nil before "+
			"ever querying continued_from. The walk therefore stops at the first hop "+
			"that crosses a shard -- which is the exact failure this method's doc "+
			"comment says the fan-out avoids.",
			run1, got.ID, run2, run1, run1)
	}
}
