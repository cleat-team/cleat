package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// cleat#3687, what holding the purge's DELETEs open leaves and how long it can last.

// TestAPurgeWhoseCommitFailsAfterAnotherShardCommittedNamesItAndDeployingAgainRepairsIt pins the
// window that is LEFT, not a fix for it. The commits are one after another across databases, so a
// commit that fails on the name's own shard after the other shard has committed splits the purge. The
// test holds that the error says exactly what was written and what was not, says what repairs it, and
// that the repair works.
func TestAPurgeWhoseCommitFailsAfterAnotherShardCommittedNamesItAndDeployingAgainRepairsIt(t *testing.T) {
	rs := newRealShards(t, "cleat_3687_commit", "a", "b")
	d := rs.shardOf(raceDefName)
	o := otherThan(d, 2)
	ctx := context.Background()
	if err := rs.deploy(ctx, raceDefName, 1, defBytesA, false); err != nil {
		t.Fatalf("UNMEASURED: deploy the version to purge: %v", err)
	}
	st, gated := rs.gatedSharded(t, raceDefName)
	injected := errors.New("injected commit failure")
	gated[d].failCommit = func() error { return injected }

	err := st.PurgeWorkflowDef(ctx, raceDefName, 1)
	var split *ShardedDefWriteError
	if !errors.As(err, &split) || !errors.Is(err, injected) {
		t.Fatalf("a purge whose last commit failed returned %T %v, want a *ShardedDefWriteError wrapping the commit's error", err, err)
	}
	if len(split.Succeeded) != 1 || split.Succeeded[0] != rs.names[o] || len(split.Failed) != 1 || split.Failed[0] != rs.names[d] {
		t.Errorf("the error says written=%v failed=%v, want written=[%s] failed=[%s]", split.Succeeded, split.Failed, rs.names[o], rs.names[d])
	}
	for _, want := range []string{"did not reach every shard", "written to [" + rs.names[o] + "]", "failed on [" + rs.names[d] + "]",
		"deploy the version again to restore them"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not say %q: %v", want, err)
		}
	}
	// The state the message describes: the committed shard lost the version, the failed one kept it.
	if n := rs.defCount(t, o, raceDefName); n != 0 {
		t.Errorf("the shard whose commit went through holds %d copies, want 0", n)
	}
	if n := rs.defCount(t, d, raceDefName); n != 1 {
		t.Errorf("the shard whose commit failed holds %d copies, want 1 (its transaction was rolled back)", n)
	}
	if got := rs.advisoryLocksOn(t, d); got != 0 {
		t.Errorf("%d lock(s) still held after the purge returned", got)
	}

	// The repair the message names.
	gated[d].failCommit = nil
	if err := st.DeployWorkflowDef(ctx, &WorkflowDef{Name: raceDefName, Version: 1, WASMBytes: defBytesA, ABIVersion: 1, MinVersion: 1}); err != nil {
		t.Fatalf("deploying again, as the message says, failed: %v", err)
	}
	if string(rs.bytesOn(t, d, raceDefName, 1)) != string(rs.bytesOn(t, o, raceDefName, 1)) {
		t.Errorf("the shards still disagree after deploying again")
	}
}

// A commit that fails on the OTHER shard, which commits first, leaves the name's own shard unattempted
// and rolled back: nothing is removed anywhere.
func TestAPurgeWhoseFirstCommitFailsRemovesNothingFromAnyShard(t *testing.T) {
	rs := newRealShards(t, "cleat_3687_firstcommit", "a", "b")
	d := rs.shardOf(raceDefName)
	o := otherThan(d, 2)
	ctx := context.Background()
	if err := rs.deploy(ctx, raceDefName, 1, defBytesA, false); err != nil {
		t.Fatalf("UNMEASURED: deploy the version to purge: %v", err)
	}
	st, gated := rs.gatedSharded(t, raceDefName)
	gated[o].failCommit = func() error { return errors.New("injected commit failure") }

	err := st.PurgeWorkflowDef(ctx, raceDefName, 1)
	if err == nil || !strings.Contains(err.Error(), "injected commit failure") {
		t.Fatalf("a purge whose first commit failed returned %v, want that failure", err)
	}
	for i := range rs.names {
		if n := rs.defCount(t, i, raceDefName); n != 1 {
			t.Errorf("shard %q holds %d copies, want 1: nothing should have been removed", rs.names[i], n)
		}
	}
	if got := rs.advisoryLocksOn(t, d); got != 0 {
		t.Errorf("%d lock(s) still held after the purge returned", got)
	}
}

// TestASlowShardCannotKeepAnotherShardsDeleteOpenBeyondTheBound: the name's own shard, purged last,
// takes longer than the bound to answer while the other shard's DELETE is open and holding its row.
// The purge gives up at the bound, removes nothing, and the run that was waiting on the other shard's
// row proceeds then, not after the slow shard recovers.
func TestASlowShardCannotKeepAnotherShardsDeleteOpenBeyondTheBound(t *testing.T) {
	rs := newRealShards(t, "cleat_3687_bound", "a", "b")
	d := rs.shardOf(raceDefName)
	o := otherThan(d, 2)
	ctx := context.Background()
	if err := rs.deploy(ctx, raceDefName, 1, defBytesA, false); err != nil {
		t.Fatalf("UNMEASURED: deploy the version to purge: %v", err)
	}
	st, gated := rs.gatedSharded(t, raceDefName)
	const bound = 700 * time.Millisecond
	st.defLockWait = bound

	type started struct {
		took time.Duration
		err  error
	}
	waiter := make(chan started, 1)
	// Runs when the other shard's DELETE is open and the name's own shard is about to be asked. A
	// watchdog ends the "slow" shard after 10 s so a purge that ignores its bound fails instead of
	// hanging the test.
	gated[d].duringPurgeBegin = func(pctx context.Context) {
		id := rs.idOnShard(t, o, "bound-wait")
		go func() {
			t0 := time.Now()
			_, _, err := rs.store.StartNewRun(ctx, id, raceDefName, 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
			waiter <- started{time.Since(t0), err}
		}()
		select {
		case <-pctx.Done():
		case <-time.After(10 * time.Second):
		}
	}

	t0 := time.Now()
	err := st.PurgeWorkflowDef(ctx, raceDefName, 1)
	took := time.Since(t0)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "gave up after 700ms") {
		t.Fatalf("a purge held up by a slow shard returned %v, want it to give up at the bound", err)
	}
	if !strings.Contains(err.Error(), "nothing that had not already committed was removed") {
		t.Errorf("the message does not say nothing was removed: %v", err)
	}
	if took < bound || took >= 5*time.Second {
		t.Errorf("the purge gave up after %s, want about the %s bound", took, bound)
	}
	for i := range rs.names {
		if n := rs.defCount(t, i, raceDefName); n != 1 {
			t.Errorf("shard %q holds %d copies after a purge that gave up, want 1", rs.names[i], n)
		}
	}
	select {
	case r := <-waiter:
		if r.err != nil {
			t.Errorf("the run that waited behind the open delete failed: %v", r.err)
		}
		if r.took < bound/2 || r.took >= 5*time.Second {
			t.Errorf("the waiting run took %s, want it held for about the bound and then released", r.took)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the run behind the open delete was still waiting 10 s after the purge gave up: the row stayed locked")
	}
	if got := rs.advisoryLocksOn(t, d); got != 0 {
		t.Errorf("%d lock(s) still held after the purge returned", got)
	}
}

// With three or more shards, a commit that fails must stop the commits after it. fanOutDefWrite's
// order for a purge is the other shards in turn and the name's own shard last, and it carries on past
// a failure on one of the others; for holds that are still open that would remove the definition from
// a shard whose neighbour kept it, for no reason. Every shard not yet committed is rolled back.
func TestAPurgeWhoseFirstCommitFailsOnThreeShardsRemovesNothingFromAnyShard(t *testing.T) {
	rs := newRealShards(t, "cleat_3687_three", "a", "b", "c")
	d := rs.shardOf(raceDefName)
	ctx := context.Background()
	if err := rs.deploy(ctx, raceDefName, 1, defBytesA, false); err != nil {
		t.Fatalf("UNMEASURED: deploy the version to purge: %v", err)
	}
	st, gated := rs.gatedSharded(t, raceDefName)
	// The commit order is the other shards in list order, then the name's own shard.
	first := -1
	for i := range rs.names {
		if i != d {
			first = i
			break
		}
	}
	gated[first].failCommit = func() error { return errors.New("injected commit failure") }

	err := st.PurgeWorkflowDef(ctx, raceDefName, 1)
	if err == nil || !strings.Contains(err.Error(), "injected commit failure") {
		t.Fatalf("a purge whose first commit failed returned %v, want that failure", err)
	}
	for i := range rs.names {
		if n := rs.defCount(t, i, raceDefName); n != 1 {
			t.Errorf("shard %q holds %d copies, want 1: a failed commit must stop the ones after it (error: %v)", rs.names[i], n, err)
		}
	}
	if got := rs.advisoryLocksOn(t, d); got != 0 {
		t.Errorf("%d lock(s) still held after the purge returned", got)
	}
}

// A failure on the SECOND commit on three shards: the first shard committed, the failing one and the
// ones after it did not, and the error says which is which, including the shard not attempted.
func TestAPurgeWhoseSecondCommitFailsOnThreeShardsNamesWhatWasWrittenFailedAndNotAttempted(t *testing.T) {
	rs := newRealShards(t, "cleat_3687_three2", "a", "b", "c")
	d := rs.shardOf(raceDefName)
	ctx := context.Background()
	if err := rs.deploy(ctx, raceDefName, 1, defBytesA, false); err != nil {
		t.Fatalf("UNMEASURED: deploy the version to purge: %v", err)
	}
	st, gated := rs.gatedSharded(t, raceDefName)
	var others []int
	for i := range rs.names {
		if i != d {
			others = append(others, i)
		}
	}
	gated[others[1]].failCommit = func() error { return errors.New("injected commit failure") }

	err := st.PurgeWorkflowDef(ctx, raceDefName, 1)
	var split *ShardedDefWriteError
	if !errors.As(err, &split) {
		t.Fatalf("returned %T %v, want a *ShardedDefWriteError", err, err)
	}
	if len(split.Succeeded) != 1 || split.Succeeded[0] != rs.names[others[0]] ||
		len(split.Failed) != 1 || split.Failed[0] != rs.names[others[1]] ||
		len(split.NotAttempted) != 1 || split.NotAttempted[0] != rs.names[d] {
		t.Errorf("error says written=%v failed=%v not attempted=%v, want written=[%s] failed=[%s] not attempted=[%s]",
			split.Succeeded, split.Failed, split.NotAttempted, rs.names[others[0]], rs.names[others[1]], rs.names[d])
	}
	for i, want := range map[int]int{others[0]: 0, others[1]: 1, d: 1} {
		if n := rs.defCount(t, i, raceDefName); n != want {
			t.Errorf("shard %q holds %d copies, want %d", rs.names[i], n, want)
		}
	}
}

// The bound covers holding the deletes open, not the commits. Once every DELETE is held and the purge
// has decided to commit, the deadline running out must not split it. The hook stalls just before the
// name's own shard commits, past the bound, with the other shard already committed.
func TestTheBoundExpiringBetweenTwoCommitsDoesNotSplitThePurge(t *testing.T) {
	rs := newRealShards(t, "cleat_3687_bounded", "a", "b")
	d := rs.shardOf(raceDefName)
	o := otherThan(d, 2)
	ctx := context.Background()
	if err := rs.deploy(ctx, raceDefName, 1, defBytesA, false); err != nil {
		t.Fatalf("UNMEASURED: deploy the version to purge: %v", err)
	}
	st, gated := rs.gatedSharded(t, raceDefName)
	const bound = 500 * time.Millisecond
	st.defLockWait = bound
	stalled := false
	gated[d].afterPurge = func() { // just before the name's own shard commits
		stalled = true
		time.Sleep(3 * bound)
	}

	err := st.PurgeWorkflowDef(ctx, raceDefName, 1)
	if !stalled {
		t.Fatalf("UNMEASURED: the hook never ran")
	}
	if err != nil {
		t.Errorf("a purge whose bound ran out between two commits returned %v, want it to finish", err)
	}
	if atD, atO := rs.defCount(t, d, raceDefName), rs.defCount(t, o, raceDefName); atD != 0 || atO != 0 {
		t.Errorf("the shards hold designated=%d other=%d copies, want 0 and 0: the purge must not be split by the clock", atD, atO)
	}
}
