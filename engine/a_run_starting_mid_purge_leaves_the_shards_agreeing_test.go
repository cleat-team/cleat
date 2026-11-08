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

// TestARunStartingOnTheNameOwnShardMidPurgeLeavesTheShardsAgreeing: every shard has been asked about the
// purge and none has finished it when a run starts for the version on the name's own shard. Either the
// start is held behind the purge and fails because the version is gone, or the purge is refused with
// nothing removed anywhere; never one shard without the version and the other with it.
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

	id := rs.idOnShard(t, d, "mid-purge")
	startErr := make(chan error, 1)
	var blocked, startedDone, hookRan bool
	var earlyErr error
	// The other shard is committed first. At that point the name's own shard has been asked and not
	// finished, which is the window.
	gated[o].afterPurge = func() {
		hookRan = true
		go func() {
			_, _, err := rs.store.StartNewRun(ctx, id, raceDefName, 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
			startErr <- err
		}()
		blocked, startedDone, earlyErr = rs.waitUntilBlockedOrDone(t, d, startErr, 5*time.Second)
	}

	purgeErr := st.PurgeWorkflowDef(ctx, raceDefName, 1)
	if !hookRan {
		t.Fatalf("UNMEASURED: the hook never ran (purge returned %v)", purgeErr)
	}
	if !blocked && !startedDone {
		t.Fatalf("UNMEASURED: the start neither returned nor waited on a lock within 5 s")
	}
	atD, atO := rs.defCount(t, d, raceDefName), rs.defCount(t, o, raceDefName)
	if atD != atO {
		t.Fatalf("the shards disagree about whether the version exists: designated holds %d copies, the other %d "+
			"(the run start returned early=%v with %v; purge returned: %v)", atD, atO, startedDone, earlyErr, purgeErr)
	}
	// The start was held behind the name's own shard's open delete, so the purge went through and the
	// start then found no version.
	if !blocked {
		t.Errorf("the run start was not held behind the purge (it returned %v at once)", earlyErr)
	}
	if purgeErr != nil {
		t.Errorf("the purge failed: %v", purgeErr)
	}
	startRes := earlyErr // already collected when the start returned before the purge finished
	if !startedDone {
		startRes = <-startErr
	}
	if startRes == nil || !strings.Contains(startRes.Error(), "workflow_payloads_def_fkey") {
		t.Errorf("the run that waited returned %v, want the definition foreign key refusing it", startRes)
	}
	if atD != 0 {
		t.Errorf("both shards should have lost the version, got %d copies", atD)
	}
}

// TestARunAlreadyStartedMakesThePurgeRefuseEverywhere: the other order. A run exists on the name's own
// shard before the purge reaches it, so that shard's DELETE refuses, and every shard that had already
// run its DELETE is rolled back. Nothing is removed anywhere.
func TestARunAlreadyStartedMakesThePurgeRefuseEverywhere(t *testing.T) {
	rs := newRealShards(t, "cleat_3687_refuse", "a", "b")
	d := rs.shardOf(raceDefName)
	o := otherThan(d, 2)
	ctx := context.Background()
	if err := rs.deploy(ctx, raceDefName, 1, defBytesA, false); err != nil {
		t.Fatalf("UNMEASURED: deploy the version to purge: %v", err)
	}
	st, _ := rs.gatedSharded(t, raceDefName)
	rs.startRunOn(ctx, t, d, raceDefName)

	err := st.PurgeWorkflowDef(ctx, raceDefName, 1)
	if err == nil || !strings.Contains(err.Error(), "refused, no shard was written") {
		t.Fatalf("a purge over a shard holding a run returned %v, want a refusal saying no shard was written", err)
	}
	for _, i := range []int{d, o} {
		if n := rs.defCount(t, i, raceDefName); n != 1 {
			t.Errorf("shard %q holds %d copies after a refused purge, want 1", rs.names[i], n)
		}
	}
	if got := rs.advisoryLocksOn(t, d); got != 0 {
		t.Errorf("%d lock(s) still held after the purge returned", got)
	}
}

// TestARunStartingBehindAPurgesOpenDeleteWaitsAndThenFails: the purge holds its DELETE on the name's
// own shard and has not finished. A run for that version waits for the purge and then fails cleanly
// because the version is gone, and the shards agree. A run for ANOTHER version of the name is not
// held back.
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
	var sameBlocked, sameDone, otherDone, hookRan bool
	var otherErr, sameEarly error
	// The name's own shard is committed last; by then every shard's DELETE is open.
	gated[d].afterPurge = func() {
		hookRan = true
		// The other version first: it does not take the purged row's lock, so it returns at once.
		otherCh = startOn(2, "wait-other")
		_, otherDone, otherErr = rs.waitUntilBlockedOrDone(t, d, otherCh, 5*time.Second)
		sameCh = startOn(1, "wait-same")
		sameBlocked, sameDone, sameEarly = rs.waitUntilBlockedOrDone(t, d, sameCh, 5*time.Second)
	}

	if err := st.PurgeWorkflowDef(ctx, raceDefName, 1); err != nil {
		t.Fatalf("UNMEASURED: purge returned %v", err)
	}
	if !hookRan {
		t.Fatalf("UNMEASURED: the hook never ran")
	}
	if !otherDone {
		t.Errorf("a run for another version of the same name was held back; it should not wait")
	}
	if otherErr != nil {
		t.Errorf("the run for another version failed: %v", otherErr)
	}
	if !sameBlocked || sameDone {
		t.Errorf("the run for the version being purged was not held behind the purge's open delete (blocked=%v returned early=%v with %v)", sameBlocked, sameDone, sameEarly)
	}
	sameRes := sameEarly // already collected when the start returned before the purge finished
	if !sameDone {
		sameRes = <-sameCh
	}
	if sameRes == nil || !strings.Contains(sameRes.Error(), "workflow_payloads_def_fkey") {
		t.Errorf("the run for the purged version returned %v, want the definition foreign key refusing it", sameRes)
	}
	if atD, atO := rs.defCount(t, d, raceDefName), rs.defCount(t, o, raceDefName); atD != atO || atD != 1 {
		t.Errorf("after purging v1 each shard should hold only v2: designated=%d other=%d copies of the name, want 1 and 1", atD, atO)
	}
}
