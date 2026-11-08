package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// cleat#3687. A purge removes a version from several shards in turn, and a run starts on whichever
// shard its id hashes to without taking any definition lock, so a start can land between the purge's
// writes. workflow_payloads_def_fkey is NO ACTION: a shard holding a run of the version refuses the
// DELETE. When the purge had already deleted the version from another shard and committed, that left
// the shards disagreeing about whether the version exists.
//
// The interleaving is forced with a hook that runs when one shard's purge write has been made, as in
// the deploy and deprecate race tests, so no timing decides what is asserted.

// TestARunStartingOnTheNameOwnShardMidPurgeLeavesTheShardsAgreeing: the purge has removed the version
// from the other shard when a run starts on the name's own shard. Either the purge is refused with
// nothing removed anywhere, or both shards lose the version; not one of each.
func TestARunStartingOnTheNameOwnShardMidPurgeLeavesTheShardsAgreeing(t *testing.T) {
	rs := newRealShards(t, "cleat_3687_start", "a", "b")
	d := rs.shardOf(raceDefName)
	o := otherThan(d, 2)
	ctx := context.Background()
	if err := rs.deploy(ctx, raceDefName, 1, defBytesA, false); err != nil {
		t.Fatalf("UNMEASURED: deploy the version to purge: %v", err)
	}
	st, gated := rs.gatedSharded(t, raceDefName)
	st.defLockWait = 30 * time.Second

	var runID string
	gated[o].afterPurge = func() { runID = rs.startRunOn(ctx, t, d, raceDefName) }

	err := st.PurgeWorkflowDef(ctx, raceDefName, 1)
	if runID == "" {
		t.Fatalf("UNMEASURED: no run was started mid-purge, so this run did not exercise the window (purge returned %v)", err)
	}
	atD, atO := rs.defCount(t, d, raceDefName), rs.defCount(t, o, raceDefName)
	if atD != atO {
		t.Fatalf("the shards disagree about whether the version exists: designated holds %d copies, the other %d (purge returned: %v)", atD, atO, err)
	}
	// The run is on the designated shard, so the purge cannot have removed the version there: it must
	// have been refused as a whole.
	if err == nil || !strings.Contains(err.Error(), "no shard was written") {
		t.Errorf("a purge that a run started under returned %v, want a refusal saying no shard was written", err)
	}
	if atD != 1 {
		t.Errorf("both shards should still hold the version the run needs, got %d", atD)
	}
}

// TestARunStartingBehindAPurgesOpenDeleteWaitsAndThenFails: the purge has run its DELETE on the name's
// own shard and not finished. A run for that version must not slip in ahead of the delete's outcome:
// it waits for the purge and then fails cleanly because the version is gone, and the shards agree.
// A run for ANOTHER version of the name is not held back.
func TestARunStartingBehindAPurgesOpenDeleteWaitsAndThenFails(t *testing.T) {
	rs := newRealShards(t, "cleat_3687_wait", "a", "b")
	d := rs.shardOf(raceDefName)
	o := otherThan(d, 2)
	ctx := context.Background()
	for _, v := range []int{1, 2} {
		if err := rs.deploy(ctx, raceDefName, v, defBytesA, false); err != nil {
			t.Fatalf("UNMEASURED: deploy v%d: %v", v, err)
		}
	}
	st, gated := rs.gatedSharded(t, raceDefName)
	st.defLockWait = 30 * time.Second
	// Time is only how long the purge lingers to see whether a start gets through; it is never what is
	// asserted, only whether the start had returned by then.
	const gate = 2 * time.Second

	startOn := func(version int, label string) <-chan error {
		id := rs.idOnShard(t, d, label)
		ch := make(chan error, 1)
		go func() {
			_, _, err := rs.store.StartNewRun(ctx, id, raceDefName, version, json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
			ch <- err
		}()
		return ch
	}
	var sameCh, otherCh <-chan error
	var sameErr, otherErr error
	var sameBeforeEnd, otherBeforeEnd bool
	// The designated shard is purged last, so by the time this runs the other shard's delete is done too.
	gated[d].afterPurge = func() {
		sameCh, otherCh = startOn(1, "wait-same"), startOn(2, "wait-other")
		select {
		case sameErr = <-sameCh:
			sameBeforeEnd = true
		case <-time.After(gate):
		}
		select {
		case otherErr = <-otherCh:
			otherBeforeEnd = true
		case <-time.After(gate):
		}
	}

	if err := st.PurgeWorkflowDef(ctx, raceDefName, 1); err != nil {
		t.Fatalf("UNMEASURED: purge returned %v", err)
	}
	if sameCh == nil {
		t.Fatalf("UNMEASURED: the hook never ran")
	}
	if !sameBeforeEnd {
		sameErr = <-sameCh // the purge is over now, so the start has its answer
	}
	if !otherBeforeEnd {
		otherErr = <-otherCh
	}
	if sameBeforeEnd {
		t.Errorf("a run for the version being purged returned before the purge finished; it should wait behind the purge's open delete")
	}
	if !otherBeforeEnd {
		t.Errorf("a run for another version of the same name was held back; it should not wait")
	}
	if sameErr == nil || !strings.Contains(sameErr.Error(), "workflow_payloads_def_fkey") {
		t.Errorf("the run for the purged version returned %v, want the definition foreign key refusing it", sameErr)
	}
	if otherErr != nil {
		t.Errorf("the run for another version failed: %v", otherErr)
	}
	if atD, atO := rs.defCount(t, d, raceDefName), rs.defCount(t, o, raceDefName); atD != atO || atD != 1 {
		t.Errorf("after purging v1 each shard should hold only v2: designated=%d other=%d copies of the name, want 1 and 1", atD, atO)
	}
}
