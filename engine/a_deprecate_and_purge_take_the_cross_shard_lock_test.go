package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// cleat#3547, second half. Deploy, deprecate and purge of one (name, version) take the same
// cross-shard lock (workflow_def_fanout_lock.go), because each writes every shard in turn and the
// per-shard row locks order none of them across shards. These tests force the interleaving with a hook
// between the per-shard writes, as the deploy race test does, and assert the shards AGREE afterwards.
// Before the lock was taken here each ended with the shards disagreeing.

// gcEligibleOn reads a version's deprecation flag straight from shard i's database.
func (rs *realShards) gcEligibleOn(t *testing.T, i int, name string, version int) bool {
	t.Helper()
	var v bool
	if err := rs.dbs[i].QueryRow(`SELECT gc_eligible FROM workflow_defs WHERE name = $1 AND version = $2`, name, version).Scan(&v); err != nil {
		t.Fatalf("UNMEASURED: read gc_eligible of %s v%d on shard %q: %v", name, version, rs.names[i], err)
	}
	return v
}

func TestTwoDeprecationsOfOneVersionLeaveEveryShardHoldingTheSameFlag(t *testing.T) {
	rs := newRealShards(t, "cleat_3547_depr", "a", "b")
	d := rs.shardOf(raceDefName)
	o := otherThan(d, 2)
	ctx := context.Background()
	if err := rs.deploy(ctx, raceDefName, 1, defBytesA, false); err != nil {
		t.Fatalf("UNMEASURED: deploy the version to deprecate: %v", err)
	}
	st, gated := rs.gatedSharded(t, raceDefName)
	st.defLockWait = 30 * time.Second
	const gate = 2 * time.Second

	var mu sync.Mutex
	var events []string
	log := func(s string) { mu.Lock(); events = append(events, s); mu.Unlock() }
	name := func(deprecated bool) string {
		if deprecated {
			return "A(deprecate)"
		}
		return "B(restore)"
	}
	aWroteD, aDone, bDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once

	// A deprecates, B restores. A writes the designated shard; B writes both; A writes the other
	// shard last: designated holds B's flag, the other holds A's.
	gated[d].afterDeprecate = func(dep bool) {
		log(name(dep) + " wrote the designated shard")
		if dep {
			once.Do(func() { close(aWroteD) })
		}
	}
	gated[o].beforeDeprecate = func(dep bool) {
		if !dep {
			return
		}
		select {
		case <-bDone:
			log("A resumed after B had finished")
		case <-time.After(gate):
			log("A waited out the gate: B was held back")
		}
	}
	gated[o].afterDeprecate = func(dep bool) { log(name(dep) + " wrote the other shard") }

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = st.MarkVersionDeprecated(ctx, raceDefName, 1, true); close(aDone) }()
	go func() {
		defer wg.Done()
		select {
		case <-aWroteD:
		case <-aDone:
		}
		errs[1] = st.MarkVersionDeprecated(ctx, raceDefName, 1, false)
		close(bDone)
	}()
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("UNMEASURED: write %c returned %v", 'A'+i, e)
		}
	}

	order := strings.Join(events, " -> ")
	atD, atO := rs.gcEligibleOn(t, d, raceDefName, 1), rs.gcEligibleOn(t, o, raceDefName, 1)
	if atD != atO {
		t.Fatalf("the shards disagree about whether the version is deprecated: designated=%v other=%v\norder: %s", atD, atO, order)
	}
	if !strings.Contains(order, "waited out the gate") {
		t.Errorf("UNMEASURED: the shards agree, but the second write was never held back, so this run did not "+
			"exercise the race (order: %s)", order)
	}
}

func TestADeployCannotLandBetweenThePurgesOfTwoShards(t *testing.T) {
	rs := newRealShards(t, "cleat_3547_purge", "a", "b")
	d := rs.shardOf(raceDefName)
	o := otherThan(d, 2)
	ctx := context.Background()
	if err := rs.deploy(ctx, raceDefName, 1, defBytesA, false); err != nil {
		t.Fatalf("UNMEASURED: deploy the version to purge: %v", err)
	}
	st, gated := rs.gatedSharded(t, raceDefName)
	st.defLockWait = 30 * time.Second
	const gate = 2 * time.Second

	var mu sync.Mutex
	var events []string
	log := func(s string) { mu.Lock(); events = append(events, s); mu.Unlock() }
	purgedO, purgeDone, deployDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once

	// A purge removes the other shards first and the designated one last. Between the two a deploy
	// of the same version runs. Without the lock it finds the designated copy (an upsert), writes the
	// other shard afresh, and the purge then removes the designated copy: the other shard is left
	// holding a definition the designated shard no longer has.
	gated[o].afterPurge = func() {
		log("purge removed the other shard")
		once.Do(func() { close(purgedO) })
		select {
		case <-deployDone:
			log("purge resumed after the deploy had finished")
		case <-time.After(gate):
			log("purge waited out the gate: the deploy was held back")
		}
	}

	var wg sync.WaitGroup
	var purgeErr, deployErr error
	wg.Add(2)
	go func() { defer wg.Done(); purgeErr = st.PurgeWorkflowDef(ctx, raceDefName, 1); close(purgeDone) }()
	go func() {
		defer wg.Done()
		select {
		case <-purgedO:
		case <-purgeDone:
		}
		deployErr = st.DeployWorkflowDef(ctx, &WorkflowDef{Name: raceDefName, Version: 1, WASMBytes: defBytesA, ABIVersion: 1, MinVersion: 1})
		close(deployDone)
	}()
	wg.Wait()
	if purgeErr != nil || deployErr != nil {
		t.Fatalf("UNMEASURED: purge returned %v, deploy returned %v", purgeErr, deployErr)
	}

	order := strings.Join(events, " -> ")
	atD, atO := rs.defCount(t, d, raceDefName), rs.defCount(t, o, raceDefName)
	if atD != atO {
		t.Fatalf("the shards disagree about whether the version exists: designated holds %d copies, the other %d\norder: %s", atD, atO, order)
	}
	if !strings.Contains(order, "waited out the gate") {
		t.Errorf("UNMEASURED: the shards agree, but the deploy was never held back, so this run did not "+
			"exercise the race (order: %s)", order)
	}
}

func TestADeprecateOrPurgeThatCannotTakeTheLockInTimeNamesWhatHoldsItAndWritesNothing(t *testing.T) {
	rs := newRealShards(t, "cleat_3547_dptimeout", "a", "b")
	d := rs.shardOf(raceDefName)
	ctx := context.Background()
	if err := rs.deploy(ctx, raceDefName, 1, defBytesA, false); err != nil {
		t.Fatalf("UNMEASURED: deploy: %v", err)
	}
	st, _ := rs.gatedSharded(t, raceDefName)
	const wait = 400 * time.Millisecond
	st.defLockWait = wait

	ops := []struct {
		name string
		run  func() error
	}{
		{"deprecate", func() error { return st.MarkVersionDeprecated(ctx, raceDefName, 1, true) }},
		{"purge", func() error { return st.PurgeWorkflowDef(ctx, raceDefName, 1) }},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			release, err := st.getShard(raceDefName).Store.(defFanOutLocker).lockWorkflowDefFanOut(ctx, raceDefName, 1, time.Minute)
			if err != nil {
				t.Fatalf("UNMEASURED: take the lock: %v", err)
			}
			// A watchdog, so an op that ignores its bound fails this test instead of hanging it.
			watchdog := time.AfterFunc(10*time.Second, release)
			start := time.Now()
			err = op.run()
			took := time.Since(start)
			watchdog.Stop()
			release()

			var held *WorkflowDefDeployInProgressError
			if !errors.As(err, &held) || !errors.Is(err, ErrWorkflowDefDeployInProgress) {
				t.Fatalf("%s that could not take the lock returned %T %v, want *WorkflowDefDeployInProgressError", op.name, err, err)
			}
			if held.Name != raceDefName || held.Version != 1 || held.Waited != wait {
				t.Errorf("the error names %q v%d waited %s, want %q v1 waited %s", held.Name, held.Version, held.Waited, raceDefName, wait)
			}
			if !strings.Contains(err.Error(), "Nothing was written") {
				t.Errorf("the message does not say nothing was written: %v", err)
			}
			if took < wait || took >= 10*time.Second {
				t.Errorf("%s gave up after %s, want about the %s bound", op.name, took, wait)
			}
			for i := range rs.names {
				if n := rs.defCount(t, i, raceDefName); n != 1 {
					t.Errorf("shard %q holds %d copies after a %s that never got the lock, want 1", rs.names[i], n, op.name)
				}
				if rs.gcEligibleOn(t, i, raceDefName, 1) {
					t.Errorf("shard %q was marked deprecated by a %s that never got the lock", rs.names[i], op.name)
				}
			}
		})
	}
	if got := rs.advisoryLocksOn(t, d); got != 0 {
		t.Errorf("%d lock(s) still held on shard %q after both attempts returned", got, rs.names[d])
	}
}

func TestADeprecateOrPurgeIsRefusedWhenTheDesignatedShardCannotLockAndASingleShardIsNot(t *testing.T) {
	rs := newRealShards(t, "cleat_3547_dpnolock", "a", "b")
	ctx := context.Background()
	if err := rs.deploy(ctx, raceDefName, 1, defBytesA, false); err != nil {
		t.Fatalf("UNMEASURED: deploy: %v", err)
	}
	plain := func(idx ...int) *ShardedStore {
		configs := make([]ShardConfig, len(idx))
		stores := make([]WorkflowStore, len(idx))
		closers := make([]func() error, len(idx))
		for k, i := range idx {
			configs[k], stores[k], closers[k] = ShardConfig{Name: rs.names[i]}, withoutALocker{NewPostgresStore(rs.dbs[i])}, func() error { return nil }
		}
		st, err := NewShardedStore(configs, stores, closers)
		if err != nil {
			t.Fatalf("UNMEASURED: NewShardedStore: %v", err)
		}
		return st
	}
	two := plain(0, 1)

	for _, c := range []struct {
		verb string
		run  func(*ShardedStore) error
	}{
		{"deprecate", func(s *ShardedStore) error { return s.MarkVersionDeprecated(ctx, raceDefName, 1, true) }},
		{"purge", func(s *ShardedStore) error { return s.PurgeWorkflowDef(ctx, raceDefName, 1) }},
	} {
		err := c.run(two)
		if err == nil || !strings.Contains(err.Error(), "cannot take the cross-shard deploy lock") ||
			!strings.Contains(err.Error(), "a "+c.verb+" to 2 shards") || !strings.Contains(err.Error(), "PostgreSQL only") {
			t.Fatalf("a two-shard %s without a locker returned %v, want a refusal that says why", c.verb, err)
		}
		for i := range rs.names {
			if n := rs.defCount(t, i, raceDefName); n != 1 {
				t.Errorf("shard %q holds %d copies after a refused %s, want 1", rs.names[i], n, c.verb)
			}
			if rs.gcEligibleOn(t, i, raceDefName, 1) {
				t.Errorf("shard %q was marked deprecated by a refused %s", rs.names[i], c.verb)
			}
		}
	}

	// One shard has no cross-shard order to keep, so it is not refused.
	if err := plain(0).MarkVersionDeprecated(ctx, raceDefName, 1, true); err != nil {
		t.Errorf("a single-shard deprecate was refused for lacking a lock it does not need: %v", err)
	}
}
