package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// cleat#4340, the ShardedStore site. A run's shard knows only its own rows, so its NotOpen error cannot name a successor that lives on another shard; ShardedStore replaces it with the chain's end
// found across shards (every continuation gets a fresh id and getShard hashes the id, so a chain crosses shards routinely).
type refusingShard struct {
	WorkflowStore
	successors map[string]string
	refuse     func(id string) error
}

func (s *refusingShard) DeliverSignal(_ context.Context, id, _, _ string) error { return s.refuse(id) }
func (s *refusingShard) DeliverSignalIdempotent(_ context.Context, id, _, _, _ string) (bool, error) {
	return false, s.refuse(id)
}
func (s *refusingShard) successorOfRun(_ context.Context, id string) (string, error) {
	return s.successors[id], nil
}

func TestAShardedRefusalNamesTheChainEndAcrossShards(t *testing.T) {
	succ := map[string]string{}
	mk := func(refuse func(string) error) *refusingShard {
		return &refusingShard{successors: succ, refuse: refuse}
	}
	var a, b, c string
	// The shard-local answer is deliberately WRONG: it names only the next link, as a shard that cannot see past its own rows would.
	local := func(id string) error {
		if id == "" {
			return nil
		}
		return &WorkflowNotOpenError{WorkflowID: id, Status: statusDone, ContinuedTo: succ[id]}
	}
	sh := []*refusingShard{mk(local), mk(local)}
	sharded, err := NewShardedStore([]ShardConfig{{Name: "s0"}, {Name: "s1"}}, []WorkflowStore{sh[0], sh[1]}, []func() error{nil, nil})
	if err != nil {
		t.Fatal(err)
	}
	// three ids on three hops of the chain, a -> b -> c, with a and b on different shards
	for i := 0; i < 2000 && c == ""; i++ {
		id := fmt.Sprintf("run-%d", i)
		switch {
		case a == "":
			a = id
		case b == "" && sharded.getShard(id) != sharded.getShard(a):
			b = id
		case b != "" && c == "":
			c = id
		}
	}
	succ[a], succ[b] = b, c
	if a == "" || b == "" || c == "" {
		t.Fatal("could not build a chain across two shards")
	}

	check := func(label string, err error) {
		var ne *WorkflowNotOpenError
		if !errors.As(err, &ne) {
			t.Fatalf("%s = %v, want a *WorkflowNotOpenError", label, err)
		}
		if ne.ContinuedTo != c {
			t.Errorf("%s names %q, want the chain's END %q (the shard-local answer was %q)", label, ne.ContinuedTo, c, b)
		}
	}
	check("DeliverSignal", sharded.DeliverSignal(context.Background(), a, "x", "{}"))
	_, err = sharded.DeliverSignalIdempotent(context.Background(), a, "x", "{}", "k")
	check("DeliverSignalIdempotent", err)

	// A run that did not continue names nothing, and any other error passes through untouched.
	delete(succ, c)
	var ne *WorkflowNotOpenError
	if err := sharded.DeliverSignal(context.Background(), c, "x", "{}"); !errors.As(err, &ne) || ne.ContinuedTo != "" {
		t.Errorf("a run with no successor gave %v, want a NotOpen naming none", err)
	}
	sh[0].refuse = func(string) error { return ErrWorkflowNotFound }
	sh[1].refuse = func(string) error { return ErrWorkflowNotFound }
	if err := sharded.DeliverSignal(context.Background(), a, "x", "{}"); !errors.Is(err, ErrWorkflowNotFound) {
		t.Errorf("not-found came back as %v", err)
	}
}
