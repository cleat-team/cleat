package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// cleat#4500 (follow-up to cleat#4381's measurement): a signal, a terminate and a cancel addressed to a continued run's SUCCESSOR by the successor's own id were refused, or for the
// cooperative cancel accepted and dropped, whenever the successor's row was not on the shard its id hashes to. A continue inserts the successor on the shard of the run it continues,
// so that was about half of two-shard chains. These rows run over TWO REAL PostgreSQL databases (a ShardedStore over one database behind two pools cannot show it: the row is visible
// through both), one row per verb, and each asserts the EFFECT on the shard that holds the run and the ABSENCE of it on the other, not only that no error came back.

// chainWhoseSuccessorHashesElsewhere builds first -> second with second's own id hashing to the shard that does NOT hold the chain, and returns the shard that does.
func chainWhoseSuccessorHashesElsewhere(t *testing.T, ctx context.Context, rs *realShards, tag string) (first, second string, holder, other int) {
	t.Helper()
	for i := 0; i < 80; i++ {
		first = newIntentWorkflow(t, ctx, rs.store, fmt.Sprintf("%s-%d", tag, i))
		second = continueChain(t, ctx, rs.store, first)
		holder = rs.shardOf(first)
		if h := rs.shardOf(second); h != holder {
			return first, second, holder, h
		}
	}
	t.Fatalf("UNMEASURED: no chain whose successor's id hashes to the other shard in 80 tries")
	return
}

func (rs *realShards) storeOn(i int) WorkflowStore {
	st, ok := rs.store.ShardStore(rs.names[i])
	if !ok {
		panic("no shard named " + rs.names[i])
	}
	return st
}

// signalsOn counts the signal rows stored for a run on shard i, read straight from that database.
func (rs *realShards) signalsOn(t *testing.T, i int, run string) int {
	t.Helper()
	var n int
	if err := rs.dbs[i].QueryRow(`SELECT count(*) FROM workflow_signals WHERE workflow_id = $1`, run).Scan(&n); err != nil {
		t.Fatalf("count signals for %s on shard %s: %v", run, rs.names[i], err)
	}
	return n
}

func TestAVerbToASuccessorByItsOwnIDReachesTheShardThatHoldsIt(t *testing.T) {
	ctx := context.Background()
	rs := newRealShards(t, "cleat_4500_verbs", "a", "b")

	type verb struct {
		name string
		do   func(t *testing.T, id string) error
		// want checks the EFFECT on the holder shard and its absence on the other.
		want func(t *testing.T, id string, holder, other int)
	}
	signalStored := func(t *testing.T, id string, holder, other int) {
		t.Helper()
		if n := rs.signalsOn(t, holder, id); n != 1 {
			t.Errorf("the successor's row is on shard %s and holds %d signal(s), want 1", rs.names[holder], n)
		}
		if n := rs.signalsOn(t, other, id); n != 0 {
			t.Errorf("the other shard %s holds %d signal(s) for the run, want 0", rs.names[other], n)
		}
	}
	statusIs := func(want string) func(t *testing.T, id string, holder, other int) {
		return func(t *testing.T, id string, holder, other int) {
			t.Helper()
			wf, err := rs.storeOn(holder).GetWorkflowByID(ctx, id)
			if err != nil || wf == nil {
				t.Fatalf("read the successor on its own shard %s: %v %v", rs.names[holder], wf, err)
			}
			if wf.Status != want {
				t.Errorf("the successor's status on shard %s is %q, want %q", rs.names[holder], wf.Status, want)
			}
			if o, _ := rs.storeOn(other).GetWorkflowByID(ctx, id); o != nil {
				t.Errorf("the other shard %s has a row for the successor", rs.names[other])
			}
		}
	}
	cancelFlagged := func(t *testing.T, id string, holder, other int) {
		t.Helper()
		got, _, err := rs.storeOn(holder).CheckCancellation(ctx, id)
		if err != nil || !got {
			t.Errorf("cancellation requested on the shard that holds the successor = %v (err %v), want true: the cancel was accepted and dropped", got, err)
		}
	}
	verbs := []verb{
		{"DeliverSignal", func(t *testing.T, id string) error { return rs.store.DeliverSignal(ctx, id, "S", `"x"`) }, signalStored},
		{"DeliverSignalReceipt", func(t *testing.T, id string) error {
			r, err := rs.store.DeliverSignalReceipt(ctx, id, "S", `"x"`, "")
			if err == nil && r.DeliveredTo != id {
				t.Errorf("the receipt says delivered to %q, want the addressed run %q", r.DeliveredTo, id)
			}
			return err
		}, signalStored},
		{"DeliverSignalIdempotent", func(t *testing.T, id string) error {
			replay, err := rs.store.DeliverSignalIdempotent(ctx, id, "S", `"x"`, "k-"+id)
			if err == nil && replay {
				t.Errorf("the first delivery under a key reads as a replay")
			}
			return err
		}, signalStored},
		{"TerminateWorkflow", func(t *testing.T, id string) error { return rs.store.TerminateWorkflow(ctx, id, "r") }, statusIs("terminated")},
		{"TerminateWorkflowReceipt", func(t *testing.T, id string) error {
			r, err := rs.store.TerminateWorkflowReceipt(ctx, id, "r")
			if err == nil && r.SettledRun != id {
				t.Errorf("the receipt says it settled %q, want %q", r.SettledRun, id)
			}
			return err
		}, statusIs("terminated")},
		{"CancelWorkflow", func(t *testing.T, id string) error { return rs.store.CancelWorkflow(ctx, id, "r") }, statusIs("cancelled")},
		{"CancelWorkflowReceipt", func(t *testing.T, id string) error {
			r, err := rs.store.CancelWorkflowReceipt(ctx, id, "r")
			if err == nil && r.SettledRun != id {
				t.Errorf("the receipt says it settled %q, want %q", r.SettledRun, id)
			}
			return err
		}, statusIs("cancelled")},
		{"RequestCancellation", func(t *testing.T, id string) error { return rs.store.RequestCancellation(ctx, id, "r") }, cancelFlagged},
		{"RequestCancellationReceipt", func(t *testing.T, id string) error {
			r, err := rs.store.RequestCancellationReceipt(ctx, id, "r")
			if err == nil && r.SettledRun != id {
				t.Errorf("the receipt says it settled %q, want %q", r.SettledRun, id)
			}
			return err
		}, cancelFlagged},
	}
	for _, v := range verbs {
		v := v
		t.Run(v.name, func(t *testing.T) {
			first, second, holder, other := chainWhoseSuccessorHashesElsewhere(t, ctx, rs, "v-"+v.name)
			t.Logf("chain %s -> %s: the successor's row is on shard %s, its own id hashes to shard %s", first[:12], second[:12], rs.names[holder], rs.names[other])
			if err := v.do(t, second); err != nil {
				t.Fatalf("%s addressed to the successor by its own id: %v", v.name, err)
			}
			v.want(t, second, holder, other)
		})
	}
}

// The verbs must still say not-found for a run no shard holds, and the cooperative cancel, which a store answers with success when nothing matched, must say it too.
func TestAVerbToARunNoShardHoldsSaysNotFoundIncludingTheCooperativeCancel(t *testing.T) {
	ctx := context.Background()
	rs := newRealShards(t, "cleat_4500_nosuch", "a", "b")
	const id = "no-such-run-4500"
	cases := map[string]func() error{
		"DeliverSignal":           func() error { return rs.store.DeliverSignal(ctx, id, "S", `"x"`) },
		"TerminateWorkflow":       func() error { return rs.store.TerminateWorkflow(ctx, id, "r") },
		"CancelWorkflow":          func() error { return rs.store.CancelWorkflow(ctx, id, "r") },
		"RequestCancellation":     func() error { return rs.store.RequestCancellation(ctx, id, "r") },
		"CancelWorkflowReceipt":   func() error { _, err := rs.store.CancelWorkflowReceipt(ctx, id, "r"); return err },
		"TerminateWorkflowRcpt":   func() error { _, err := rs.store.TerminateWorkflowReceipt(ctx, id, "r"); return err },
		"RequestCancellationRcpt": func() error { _, err := rs.store.RequestCancellationReceipt(ctx, id, "r"); return err },
		"DeliverSignalReceipt":    func() error { _, err := rs.store.DeliverSignalReceipt(ctx, id, "S", `"x"`, ""); return err },
		"DeliverSignalIdempotent": func() error { _, err := rs.store.DeliverSignalIdempotent(ctx, id, "S", `"x"`, "k"); return err },
	}
	for name, do := range cases {
		if err := do(); !errors.Is(err, ErrWorkflowNotFound) {
			t.Errorf("%s on a run no shard holds = %v, want ErrWorkflowNotFound", name, err)
		}
	}
}

// The control that keeps the rows above honest: a run addressed by the FIRST run's id, which worked before, still works and still lands on the successor once.
func TestAVerbToTheFirstRunOfAChainStillReachesTheSuccessorOnTheHoldingShard(t *testing.T) {
	ctx := context.Background()
	rs := newRealShards(t, "cleat_4500_first", "a", "b")
	first, second, holder, other := chainWhoseSuccessorHashesElsewhere(t, ctx, rs, "first")
	if err := rs.store.DeliverSignal(ctx, first, "S", `"x"`); err != nil {
		t.Fatalf("a signal to the first run's id: %v", err)
	}
	if n := rs.signalsOn(t, holder, second); n != 1 {
		t.Errorf("the successor on shard %s holds %d signal(s) after a signal to the first run's id, want 1", rs.names[holder], n)
	}
	if n := rs.signalsOn(t, other, second) + rs.signalsOn(t, other, first); n != 0 {
		t.Errorf("the other shard holds %d signal(s) for the chain, want 0", n)
	}
	if err := rs.store.RequestCancellation(ctx, first, "r"); err != nil {
		t.Fatalf("a cooperative cancel to the first run's id: %v", err)
	}
	if got, _, err := rs.storeOn(holder).CheckCancellation(ctx, second); err != nil || !got {
		t.Errorf("cancellation requested on the successor after a cancel to the first run's id = %v (err %v), want true", got, err)
	}
}
