package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// cleat#3547. Two operators deploy different bytes to ONE (name, version) at the same moment over
// real sharded databases. Replacement is specified behaviour on one store (the later deploy wins), so
// the property is not that the second deploy is refused but that the shards AGREE afterwards. The
// lock that makes them agree is in workflow_def_fanout_lock.go; read its header for why and for what
// it does not cover.
//
// The interleaving is forced by a hook between the per-shard writes rather than left to timing:
//
//	A writes the designated shard; B writes it (replacing A's bytes); B writes the other shard;
//	A writes the other shard (replacing B's bytes)  =>  designated holds B, the other holds A.
//
// Before the lock this ended with the shards disagreeing six runs out of six.

// gatedShard lets a test run code immediately before and after one shard's DeployWorkflowDef.
type gatedShard struct {
	*PostgresStore
	before func(def *WorkflowDef)
	after  func(def *WorkflowDef)
	// The same for the other two writes that take the lock (cleat#3547, second half).
	beforeDeprecate, afterDeprecate func(deprecated bool)
	afterPurge                      func()
}

func (g *gatedShard) MarkVersionDeprecated(ctx context.Context, name string, version int, deprecated bool) error {
	if g.beforeDeprecate != nil {
		g.beforeDeprecate(deprecated)
	}
	err := g.PostgresStore.MarkVersionDeprecated(ctx, name, version, deprecated)
	if g.afterDeprecate != nil {
		g.afterDeprecate(deprecated)
	}
	return err
}

func (g *gatedShard) PurgeWorkflowDef(ctx context.Context, name string, version int) error {
	err := g.PostgresStore.PurgeWorkflowDef(ctx, name, version)
	if g.afterPurge != nil {
		g.afterPurge()
	}
	return err
}

func (g *gatedShard) DeployWorkflowDef(ctx context.Context, def *WorkflowDef) error {
	if g.before != nil {
		g.before(def)
	}
	err := g.PostgresStore.DeployWorkflowDef(ctx, def)
	if g.after != nil {
		g.after(def)
	}
	return err
}

// whichDeploy names the deploy a write belongs to by the last byte of its module.
func whichDeploy(def *WorkflowDef) string {
	if def.WASMBytes[len(def.WASMBytes)-1] == defBytesA[len(defBytesA)-1] {
		return "A"
	}
	return "B"
}

const raceDefName = "shard-defs-race"

// gatedSharded builds a ShardedStore over rs's databases with every shard wrapped, and checks it
// designates the same shard rs does so the test's idea of "first" is the store's.
func (rs *realShards) gatedSharded(t *testing.T, name string) (*ShardedStore, []*gatedShard) {
	t.Helper()
	gated := make([]*gatedShard, len(rs.names))
	stores := make([]WorkflowStore, len(rs.names))
	configs := make([]ShardConfig, len(rs.names))
	closers := make([]func() error, len(rs.names))
	for i, n := range rs.names {
		gated[i] = &gatedShard{PostgresStore: NewPostgresStore(rs.dbs[i])}
		stores[i], configs[i], closers[i] = gated[i], ShardConfig{Name: n}, func() error { return nil }
	}
	st, err := NewShardedStore(configs, stores, closers)
	if err != nil {
		t.Fatalf("UNMEASURED: NewShardedStore: %v", err)
	}
	if got := st.getShard(name).Config.Name; got != rs.names[rs.shardOf(name)] {
		t.Fatalf("UNMEASURED: the gated store designates %q, the plain one %q", got, rs.names[rs.shardOf(name)])
	}
	return st, gated
}

// advisoryLocksOn counts the deploy locks held in shard i's database right now.
func (rs *realShards) advisoryLocksOn(t *testing.T, i int) int {
	t.Helper()
	var n int
	if err := rs.dbs[i].QueryRow(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND classid = $1::oid
		AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`, int64(defFanOutLockNamespace)).Scan(&n); err != nil {
		t.Fatalf("UNMEASURED: read pg_locks on shard %q: %v", rs.names[i], err)
	}
	return n
}

func TestConcurrentDeploysOfOneVersionLeaveEveryShardHoldingTheSameBytes(t *testing.T) {
	rs := newRealShards(t, "cleat_3547_race", "a", "b")
	d := rs.shardOf(raceDefName)
	o := otherThan(d, 2)
	st, gated := rs.gatedSharded(t, raceDefName)
	// Longer than the gate below, so the deploy that is held back is still queueing when the other
	// one lets go, and the wait is not what ends it.
	st.defLockWait = 30 * time.Second
	const gate = 2 * time.Second

	var mu sync.Mutex
	var events []string
	log := func(s string) { mu.Lock(); events = append(events, s); mu.Unlock() }
	aWroteD, aDone, bDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var aWroteDOnce sync.Once

	gated[d].after = func(def *WorkflowDef) {
		log(whichDeploy(def) + " wrote the designated shard")
		if whichDeploy(def) == "A" {
			aWroteDOnce.Do(func() { close(aWroteD) })
		}
	}
	// A pauses before its write to the other shard until B has finished, or until the gate. Without
	// the lock B finishes first and A then overwrites it there; with the lock B cannot start until A
	// has finished, so A waits the gate out. Time is only how long A waits for a B the lock is holding
	// back, never what is asserted.
	gated[o].before = func(def *WorkflowDef) {
		if whichDeploy(def) != "A" {
			return
		}
		select {
		case <-bDone:
			log("A resumed after B had finished")
		case <-time.After(gate):
			log("A waited out the gate: B was held back")
		}
	}
	gated[o].after = func(def *WorkflowDef) { log(whichDeploy(def) + " wrote the other shard") }

	ctx := context.Background()
	deploy := func(wasm []byte) error {
		return st.DeployWorkflowDef(ctx, &WorkflowDef{Name: raceDefName, Version: 1, WASMBytes: wasm, ABIVersion: 1, MinVersion: 1})
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = deploy(defBytesA); close(aDone) }()
	go func() {
		defer wg.Done()
		// If A failed before writing anything there is nothing to race, and waiting for it would hang.
		select {
		case <-aWroteD:
		case <-aDone:
		}
		errs[1] = deploy(defBytesB)
		close(bDone)
	}()
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("UNMEASURED: deploy %c returned %v", 'A'+i, e)
		}
	}

	order := strings.Join(events, " -> ")
	atD, atO := rs.bytesOn(t, d, raceDefName, 1), rs.bytesOn(t, o, raceDefName, 1)
	if string(atD) != string(atO) {
		t.Fatalf("the shards disagree about one version: designated=%x other=%x\norder: %s", atD, atO, order)
	}
	// Agreement alone would also be what luck produces. The deploy that was held back is what the lock
	// is for, so say whether the interleaving was actually attempted and prevented.
	if !strings.Contains(order, "waited out the gate") {
		t.Errorf("UNMEASURED: the shards agree, but the second deploy was never held back, so this run did not "+
			"exercise the race (order: %s)", order)
	}
}

func TestADeployOfAnotherVersionOrAnotherNameDoesNotQueueBehindTheLock(t *testing.T) {
	rs := newRealShards(t, "cleat_3547_scope", "a", "b")
	st, _ := rs.gatedSharded(t, raceDefName)
	st.defLockWait = 300 * time.Millisecond
	ctx := context.Background()

	d := rs.shardOf(raceDefName)
	locker := st.getShard(raceDefName).Store.(defFanOutLocker)
	release, err := locker.lockWorkflowDefFanOut(ctx, raceDefName, 1, time.Minute)
	if err != nil {
		t.Fatalf("UNMEASURED: take the lock: %v", err)
	}
	defer release()
	if got := rs.advisoryLocksOn(t, d); got != 1 {
		t.Fatalf("UNMEASURED: %d deploy locks held on shard %q after taking one, want 1", got, rs.names[d])
	}

	// The same name at another version, and another name at the same version, are different keys. A
	// second deploy of the held (name, version) is the control: it must queue, or this proves nothing.
	for _, c := range []struct {
		name    string
		version int
		want    string
	}{
		{raceDefName, 2, "another version"},
		{"shard-defs-race-other", 1, "another name"},
	} {
		start := time.Now()
		err := st.DeployWorkflowDef(ctx, &WorkflowDef{Name: c.name, Version: c.version, WASMBytes: defBytesA, ABIVersion: 1, MinVersion: 1})
		if err != nil {
			t.Errorf("a deploy of %s queued behind a lock it should not share: %v", c.want, err)
		}
		if took := time.Since(start); took > 200*time.Millisecond {
			t.Errorf("a deploy of %s took %s, which is the lock wait (300ms) rather than a write", c.want, took)
		}
	}
	// The watchdog is for the same reason as in the next test: a deploy that ignores its bound must
	// fail this test, not hang it.
	watchdog := time.AfterFunc(10*time.Second, release)
	err = st.DeployWorkflowDef(ctx, &WorkflowDef{Name: raceDefName, Version: 1, WASMBytes: defBytesA, ABIVersion: 1, MinVersion: 1})
	watchdog.Stop()
	if !errors.Is(err, ErrWorkflowDefDeployInProgress) {
		t.Errorf("UNMEASURED: a deploy of the held (name, version) returned %v, so the lock is not held against it", err)
	}
}

func TestADeployThatCannotTakeTheLockInTimeNamesWhatHoldsItAndWritesNothing(t *testing.T) {
	rs := newRealShards(t, "cleat_3547_timeout", "a", "b")
	st, _ := rs.gatedSharded(t, raceDefName)
	const wait = 400 * time.Millisecond
	st.defLockWait = wait
	ctx := context.Background()

	release, err := st.getShard(raceDefName).Store.(defFanOutLocker).lockWorkflowDefFanOut(ctx, raceDefName, 1, time.Minute)
	if err != nil {
		t.Fatalf("UNMEASURED: take the lock: %v", err)
	}
	// A watchdog, so a deploy that ignores its bound fails this test instead of hanging it: the holder
	// is released after 10 s and the test then reports that the bound was not honoured.
	watchdog := time.AfterFunc(10*time.Second, release)
	start := time.Now()
	err = st.DeployWorkflowDef(ctx, &WorkflowDef{Name: raceDefName, Version: 1, WASMBytes: defBytesA, ABIVersion: 1, MinVersion: 1})
	took := time.Since(start)
	watchdog.Stop()

	var held *WorkflowDefDeployInProgressError
	if !errors.As(err, &held) || !errors.Is(err, ErrWorkflowDefDeployInProgress) {
		t.Fatalf("a deploy that could not take the lock returned %T %v, want *WorkflowDefDeployInProgressError", err, err)
	}
	if held.Name != raceDefName || held.Version != 1 || held.Waited != wait {
		t.Errorf("the error names %q v%d waited %s, want %q v1 waited %s", held.Name, held.Version, held.Waited, raceDefName, wait)
	}
	for _, want := range []string{`"` + raceDefName + `"`, "version 1", "another deploy", "Nothing was written"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not say %q: %v", want, err)
		}
	}
	if took < wait || took >= 10*time.Second {
		t.Errorf("the deploy gave up after %s, want about the %s bound", took, wait)
	}
	for i := range rs.names {
		if n := rs.defCount(t, i, raceDefName); n != 0 {
			t.Errorf("shard %q holds %d copies after a deploy that never got the lock, want 0", rs.names[i], n)
		}
	}

	// The other direction: once the holder lets go the same deploy goes through.
	release()
	if err := st.DeployWorkflowDef(ctx, &WorkflowDef{Name: raceDefName, Version: 1, WASMBytes: defBytesA, ABIVersion: 1, MinVersion: 1}); err != nil {
		t.Errorf("the deploy still failed after the lock was released: %v", err)
	}
}

func TestACancelledDeployMidFanOutLeavesNoLockHeld(t *testing.T) {
	rs := newRealShards(t, "cleat_3547_cancel", "a", "b")
	d := rs.shardOf(raceDefName)
	o := otherThan(d, 2)
	st, gated := rs.gatedSharded(t, raceDefName)
	st.defLockWait = 3 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel after the designated shard is written and before the other is: the lock is held and the
	// fan-out is part way.
	gated[o].before = func(*WorkflowDef) { cancel() }

	err := st.DeployWorkflowDef(ctx, &WorkflowDef{Name: raceDefName, Version: 1, WASMBytes: defBytesA, ABIVersion: 1, MinVersion: 1})
	if err == nil {
		t.Fatalf("UNMEASURED: the cancelled deploy returned nil, so it was not cancelled mid-fan-out")
	}
	if n := rs.defCount(t, d, raceDefName); n != 1 {
		t.Fatalf("UNMEASURED: the designated shard holds %d copies, want 1, so the cancel did not land mid-fan-out", n)
	}
	if got := rs.advisoryLocksOn(t, d); got != 0 {
		t.Errorf("%d deploy lock(s) still held on shard %q after the deploy returned", got, rs.names[d])
	}

	// And the next deploy proceeds within the bound, which is what a leaked lock would break.
	gated[o].before = nil
	start := time.Now()
	if err := st.DeployWorkflowDef(context.Background(), &WorkflowDef{Name: raceDefName, Version: 1, WASMBytes: defBytesB, ABIVersion: 1, MinVersion: 1}); err != nil {
		t.Fatalf("the deploy after a cancelled one failed: %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the deploy after a cancelled one took %s, which is waiting on a lock", took)
	}
	if string(rs.bytesOn(t, d, raceDefName, 1)) != string(rs.bytesOn(t, o, raceDefName, 1)) {
		t.Errorf("the shards disagree after the repairing deploy")
	}
}

// withoutALocker hides every method the embedded interface does not declare, which includes the
// unexported lock method a *PostgresStore carries.
type withoutALocker struct{ WorkflowStore }

func TestAMultiShardDeployIsRefusedWhenTheDesignatedShardCannotLockAndASingleShardIsNot(t *testing.T) {
	rs := newRealShards(t, "cleat_3547_nolock", "a", "b")
	plain := func(names ...string) *ShardedStore {
		configs := make([]ShardConfig, len(names))
		stores := make([]WorkflowStore, len(names))
		closers := make([]func() error, len(names))
		for i, n := range names {
			idx := 0
			for j, rn := range rs.names {
				if rn == n {
					idx = j
				}
			}
			configs[i], stores[i], closers[i] = ShardConfig{Name: n}, withoutALocker{NewPostgresStore(rs.dbs[idx])}, func() error { return nil }
		}
		st, err := NewShardedStore(configs, stores, closers)
		if err != nil {
			t.Fatalf("UNMEASURED: NewShardedStore: %v", err)
		}
		return st
	}
	if _, ok := (WorkflowStore(withoutALocker{NewPostgresStore(rs.dbs[0])})).(defFanOutLocker); ok {
		t.Fatalf("UNMEASURED: the wrapper still carries the lock method")
	}
	ctx := context.Background()
	def := &WorkflowDef{Name: raceDefName, Version: 1, WASMBytes: defBytesA, ABIVersion: 1, MinVersion: 1}

	err := plain("a", "b").DeployWorkflowDef(ctx, def)
	if err == nil || !strings.Contains(err.Error(), "cannot take the cross-shard deploy lock") || !strings.Contains(err.Error(), "PostgreSQL only") {
		t.Fatalf("a two-shard deploy without a locker returned %v, want a refusal that says why", err)
	}
	for i := range rs.names {
		if n := rs.defCount(t, i, raceDefName); n != 0 {
			t.Errorf("shard %q holds %d copies after a refused deploy, want 0", rs.names[i], n)
		}
	}

	// One shard has no cross-shard order to keep, so it needs no lock and is not refused.
	if err := plain("a").DeployWorkflowDef(ctx, def); err != nil {
		t.Errorf("a single-shard store was refused for lacking a lock it does not need: %v", err)
	}
}
