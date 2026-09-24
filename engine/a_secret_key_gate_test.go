package engine

// The secret-key write gate (cleat#1991, option A), on all three dialects.
//
// What these pin is specs/CleatKeyRotation.tla with WriteGate = "registry":
//
//   - a writer refuses key version v while any LIVE registered worker cannot open
//     v (S1's "a new write meeting old workers");
//   - the worker's {register; boot-check} span and the writer's {read registry;
//     write} span never interleave. The model's S1 counterexample for
//     WriteGate = "observed" is exactly that interleaving; the two ordering tests
//     below are the two orders the lock leaves, and each asserts the outcome the
//     model requires of it.
//
// Tests that need two sides at once hold one at a hook and run the other, and
// decide "was it excluded" by what the second side OBSERVED, not by how long it
// took. The only waits are on the direction where waiting cannot make a passing
// test fail: giving a broken implementation time to show that it did not block.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func (e *rotationEnv) registry() *WorkerRegistry {
	return &WorkerRegistry{DB: e.owner, Dialect: Dialect(e.dialect)}
}

// clearWorkers empties admin.workers for this test and again when it ends.
// The registry is cluster-global by design, so a row another test left behind
// would be a live worker as far as the gate is concerned.
func (e *rotationEnv) clearWorkers(t *testing.T) {
	t.Helper()
	del := func() {
		if _, err := e.owner.Exec(`DELETE FROM ` + e.registry().table()); err != nil {
			t.Fatalf("clear the worker registry: %v", err)
		}
	}
	del()
	// Bounded: if a mutation (or a bug) leaves a registration transaction open,
	// an unbounded DELETE would wait on its row lock and turn a failing test
	// into a hung one.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = e.owner.ExecContext(ctx, `DELETE FROM `+e.registry().table())
	})
}

// registerWorker registers one live worker that opens the given versions.
func (e *rotationEnv) registerWorker(t *testing.T, host string, versions ...int) string {
	t.Helper()
	id := uuid.NewString()
	err := e.registry().Register(context.Background(), WorkerRegistration{
		WorkerID: id, Hostname: host, PID: 4242, SecretKeyVersions: versions})
	if err != nil {
		t.Fatalf("register %s: %v", host, err)
	}
	return id
}

// workerWithNoKeySet is a worker row from before the column existed: NULL.
func (e *rotationEnv) workerWithNoKeySet(t *testing.T, host string) string {
	t.Helper()
	id := e.registerWorker(t, host)
	d := Dialect(e.dialect)
	if _, err := e.owner.Exec(`UPDATE `+e.registry().table()+
		` SET secret_key_versions = NULL WHERE worker_id = `+d.placeholder(1), id); err != nil {
		t.Fatalf("null the key set: %v", err)
	}
	return id
}

func (e *rotationEnv) backdate(t *testing.T, id string, age time.Duration) {
	t.Helper()
	d := Dialect(e.dialect)
	if _, err := e.owner.Exec(`UPDATE `+e.registry().table()+
		` SET last_heartbeat_at = `+d.intervalExpr(1)+` WHERE worker_id = `+d.placeholder(2),
		int(age/time.Second), id); err != nil {
		t.Fatalf("back-date %s: %v", id, err)
	}
}

func (e *rotationEnv) registeredHosts(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.owner.QueryRow(`SELECT count(*) FROM ` + e.registry().table()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *rotationEnv) rowExists(t *testing.T, tenant uuid.UUID, name string) bool {
	t.Helper()
	probe := NewSecretStoreWithRing(e.owner, string(e.dialect), nil)
	ctx := e.ctx(tenant)
	exists, _, err := probe.SecretMeta(ctx, tenant.String(), name)
	if err != nil {
		t.Fatalf("SecretMeta %q: %v", name, err)
	}
	return exists
}

// assertGateFree reads the LOCK's state from the database's own catalog. A test
// that only tries to take the gate again proves nothing on MySQL, where GET_LOCK
// is session-scoped and re-entrant: the pooled connection that leaked the lock is
// the one most likely to be handed to the next writer, and it re-acquires its own
// lock without complaint. Asking the catalog has no such blind spot.
func (e *rotationEnv) assertGateFree(t *testing.T, when string) {
	t.Helper()
	var held bool
	switch e.dialect {
	case "mysql":
		var holder sql.NullInt64
		if err := e.owner.QueryRow(`SELECT IS_USED_LOCK(?)`, keyGateName).Scan(&holder); err != nil {
			t.Fatalf("IS_USED_LOCK: %v", err)
		}
		held = holder.Valid
	case "mssql":
		var n int
		if err := e.owner.QueryRow(`SELECT count(*) FROM sys.dm_tran_locks WHERE resource_type = 'APPLICATION' AND resource_database_id = DB_ID()`).Scan(&n); err != nil {
			t.Fatalf("sys.dm_tran_locks: %v", err)
		}
		held = n > 0
	default:
		var n int
		if err := e.owner.QueryRow(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND ((classid::bigint << 32) | objid::bigint) = $1`, keyGatePGKey).Scan(&n); err != nil {
			t.Fatalf("pg_locks: %v", err)
		}
		held = n > 0
	}
	if held {
		t.Fatalf("%s: the secret-key gate is still held", when)
	}
}

func shortWaits(t *testing.T, writer, worker time.Duration) {
	t.Helper()
	w0, k0 := keyGateWriterWait, keyGateWorkerWait
	keyGateWriterWait, keyGateWorkerWait = writer, worker
	t.Cleanup(func() { keyGateWriterWait, keyGateWorkerWait = w0, k0 })
}

// The refusal itself, and that it clears once the worker can open the version.
// Nothing may have been written by the refused attempt.
func TestAWriteIsRefusedWhileALiveWorkerCannotOpenItsVersion(t *testing.T) {
	forEachDialect(t, func(t *testing.T, e *rotationEnv) {
		e.clearWorkers(t)
		tenant := e.tenants[0]
		const name = "cleat-1991-gate-refuses"
		e.claim(t, tenant, name)

		e.registerWorker(t, "web-new", 1, 2)
		old := e.registerWorker(t, "web-old", 1)

		ring := ringOf(t, rotV2, rotV1)
		err := e.store(ring).PutSecret(e.ctx(tenant), tenant.String(), name, "sk-v2")
		var gate *SecretKeyGateError
		if !errors.As(err, &gate) {
			t.Fatalf("err = %v, want a *SecretKeyGateError", err)
		}
		if gate.Version != 2 || len(gate.Blocking) != 1 || gate.Blocking[0].WorkerID != old {
			t.Fatalf("blocked by %+v at version %d, want exactly the worker still on {1} at version 2",
				gate.Blocking, gate.Version)
		}
		for _, want := range []string{"web-old", "version 2", "opens versions [1]"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal must name what an operator needs; missing %q in:\n%s", want, err)
			}
		}
		if e.rowExists(t, tenant, name) {
			t.Fatal("a refused write left a row behind")
		}

		// The old worker is rolled forward: it re-registers with both keys.
		if err := e.registry().Deregister(context.Background(), old); err != nil {
			t.Fatal(err)
		}
		e.registerWorker(t, "web-old", 1, 2)
		e.put(t, ring, tenant, name, "sk-v2")
		if got := e.rawKeyVersion(t, tenant, name); got != 2 {
			t.Fatalf("key_version = %d after the gate cleared, want 2", got)
		}
	})
}

// A registry row with no key set is a worker older than the column, and holds
// version 1 only. So a rolling upgrade of a deployment that never rotates -- every
// write at version 1 -- is not blocked by workers that have not been upgraded yet.
func TestARegistryRowWithNoKeySetMeansVersionOneOnly(t *testing.T) {
	forEachDialect(t, func(t *testing.T, e *rotationEnv) {
		e.clearWorkers(t)
		tenant := e.tenants[0]
		const name = "cleat-1991-gate-null"
		e.claim(t, tenant, name)
		e.workerWithNoKeySet(t, "pre-upgrade")

		e.put(t, ringOf(t, rotV1), tenant, name, "sk-still-v1")
		if got := e.rawKeyVersion(t, tenant, name); got != 1 {
			t.Fatalf("key_version = %d, want 1", got)
		}

		err := e.store(ringOf(t, rotV2, rotV1)).PutSecret(e.ctx(tenant), tenant.String(), name, "sk-v2")
		var gate *SecretKeyGateError
		if !errors.As(err, &gate) || gate.Version != 2 {
			t.Fatalf("err = %v, want a refusal at version 2 -- a worker that predates rotation cannot open it", err)
		}
		if !strings.Contains(err.Error(), "predates key rotation") {
			t.Errorf("the refusal should say why an unlabelled worker blocks:\n%s", err)
		}
	})
}

// A live worker with NO key blocks every write, and the message says what to do:
// it would fail on the first workflow that resolves the secret.
func TestAKeylessWorkerBlocksEveryWriteAndTheMessageSaysWhatToDo(t *testing.T) {
	forEachDialect(t, func(t *testing.T, e *rotationEnv) {
		e.clearWorkers(t)
		tenant := e.tenants[0]
		const name = "cleat-1991-gate-keyless"
		e.claim(t, tenant, name)
		e.registerWorker(t, "no-key-host") // no versions

		err := e.store(ringOf(t, rotV1)).PutSecret(e.ctx(tenant), tenant.String(), name, "sk")
		var gate *SecretKeyGateError
		if !errors.As(err, &gate) {
			t.Fatalf("err = %v, want a *SecretKeyGateError", err)
		}
		for _, want := range []string{"no-key-host", "no master key configured", "CLEAT_SECRET_MASTER_KEY", "wait for it to stop"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("missing %q in:\n%s", want, err)
			}
		}
		if e.rowExists(t, tenant, name) {
			t.Fatal("a refused write left a row behind")
		}
	})
}

// A worker whose heartbeat is older than the live window is not a worker as far as
// a writer is concerned -- and the same row, fresh, is. The second half is the
// known-positive: without it the first could pass because nothing was ever read.
func TestAStaleWorkerDoesNotBlockAWriteAndAFreshOneDoes(t *testing.T) {
	forEachDialect(t, func(t *testing.T, e *rotationEnv) {
		e.clearWorkers(t)
		tenant := e.tenants[0]
		const name = "cleat-1991-gate-stale"
		e.claim(t, tenant, name)
		ring := ringOf(t, rotV2, rotV1)
		id := e.registerWorker(t, "dead-host", 1)

		if err := e.store(ring).PutSecret(e.ctx(tenant), tenant.String(), name, "x"); err == nil {
			t.Fatal("a fresh worker on {1} must block a write at version 2")
		}
		e.backdate(t, id, SecretKeyLiveWindow+time.Minute)
		e.put(t, ring, tenant, name, "sk-v2")
		if got := e.rawKeyVersion(t, tenant, name); got != 2 {
			t.Fatalf("key_version = %d, want 2", got)
		}
	})
}

// Reseal writes under the same gate, row by row: with a worker still on {1} it
// refuses and changes nothing; once the worker can open 2 it converges.
func TestResealIsRefusedWhileALiveWorkerCannotOpenTheCurrentVersion(t *testing.T) {
	forEachDialect(t, func(t *testing.T, e *rotationEnv) {
		e.clearWorkers(t)
		tenant := e.tenants[0]
		const name = "cleat-1991-gate-reseal"
		e.claim(t, tenant, name)
		e.put(t, ringOf(t, rotV1), tenant, name, "sk-original")

		old := e.registerWorker(t, "web-old", 1)
		ring := ringOf(t, rotV2, rotV1)
		res, err := e.store(ring).ResealSecrets(context.Background(), false)
		var gate *SecretKeyGateError
		if !errors.As(err, &gate) {
			t.Fatalf("ResealSecrets err = %v (report %+v), want a *SecretKeyGateError", err, res)
		}
		if got := e.rawKeyVersion(t, tenant, name); got != 1 {
			t.Fatalf("key_version = %d after a refused reseal, want 1 -- nothing may move", got)
		}

		if err := e.registry().Deregister(context.Background(), old); err != nil {
			t.Fatal(err)
		}
		e.registerWorker(t, "web-old", 1, 2)
		res, err = e.store(ring).ResealSecrets(context.Background(), false)
		if err != nil {
			t.Fatalf("ResealSecrets: %v", err)
		}
		if got := e.rawKeyVersion(t, tenant, name); got != 2 {
			t.Fatalf("key_version = %d after the gate cleared, want 2 (%+v)", got, res)
		}
	})
}

// THE MODEL'S TWO ORDERS, and the first is its S1 counterexample.
//
// Order 1 -- WRITER FIRST. A writer is inside its span (lock held, registry not
// yet read). A worker starts {register; boot-check}. The worker must not get
// through until the writer has committed, and then its boot check must SEE the
// row the writer wrote. Without the lock the worker's check runs while the writer
// is paused, finds nothing, passes -- and the writer then writes a row the worker
// cannot open. That is the state S1 forbids.
func TestAWorkerBootingDuringAWriteSeesTheRowTheWriterWrote(t *testing.T) {
	forEachDialect(t, func(t *testing.T, e *rotationEnv) {
		e.clearWorkers(t)
		shortWaits(t, 20*time.Second, 20*time.Second)
		tenant := e.tenants[0]
		const name = "cleat-1991-gate-order1"
		e.claim(t, tenant, name)
		ring := ringOf(t, rotV2, rotV1)
		writer := e.store(ring)

		atHook := make(chan struct{})
		release := make(chan struct{})
		var once bool
		writer.beforeGateCheck = func() {
			if once {
				return
			}
			once = true
			close(atHook)
			<-release
		}
		writerDone := make(chan error, 1)
		go func() { writerDone <- writer.PutSecret(e.ctx(tenant), tenant.String(), name, "sk-v2") }()
		<-atHook // the writer holds the gate and has read nothing

		// The booting worker holds only {1}. Its check is the real one.
		bootStore := e.store(ringOf(t, rotV1))
		var sawUnopenable bool
		workerDone := make(chan error, 1)
		go func() {
			workerDone <- e.registry().RegisterUnderKeyGate(context.Background(),
				WorkerRegistration{WorkerID: uuid.NewString(), Hostname: "booting", PID: 1, SecretKeyVersions: []int{1}},
				func(ctx context.Context) error {
					chk, err := bootStore.CheckKeyRing(ctx)
					if err != nil {
						return err
					}
					mineHits := 0
					for v, n := range chk.Unopenable {
						if v == 2 {
							mineHits += n
						}
					}
					sawUnopenable = mineHits > 0
					if sawUnopenable {
						return errors.New("refusing: rows on a version this worker cannot open")
					}
					return nil
				})
		}()

		// If the worker is NOT excluded it finishes now, having read a table with no
		// row in it. Waiting is only ever how a broken implementation gets caught.
		select {
		case err := <-workerDone:
			t.Fatalf("the worker's span completed (%v) while a writer held the gate: nothing excluded it, "+
				"so its boot check ran before the row existed", err)
		case <-time.After(400 * time.Millisecond):
		}

		close(release)
		if err := <-writerDone; err != nil {
			t.Fatalf("the writer: %v", err)
		}
		if err := <-workerDone; err == nil {
			t.Fatal("the worker was admitted, but the writer had written version 2 and it holds only {1}")
		}
		if !sawUnopenable {
			t.Fatal("the worker's boot check did not see the row the writer wrote")
		}
		if n := e.registeredHosts(t); n != 0 {
			t.Fatalf("a worker that refused to start left %d registry row(s) behind; each would block writers", n)
		}
	})
}

// Order 2 -- WORKER FIRST. A worker is inside its span: its row is inserted, its
// check has not finished. A writer starts. It must wait, and once the worker has
// committed the writer's registry read must SEE that worker and refuse a version
// the worker cannot open.
func TestAWriteDuringABootSeesTheWorkerThatWasBooting(t *testing.T) {
	forEachDialect(t, func(t *testing.T, e *rotationEnv) {
		e.clearWorkers(t)
		shortWaits(t, 20*time.Second, 20*time.Second)
		tenant := e.tenants[0]
		const name = "cleat-1991-gate-order2"
		e.claim(t, tenant, name)

		inCheck := make(chan struct{})
		release := make(chan struct{})
		workerDone := make(chan error, 1)
		go func() {
			workerDone <- e.registry().RegisterUnderKeyGate(context.Background(),
				WorkerRegistration{WorkerID: uuid.NewString(), Hostname: "booting-old", PID: 1, SecretKeyVersions: []int{1}},
				func(ctx context.Context) error {
					close(inCheck)
					<-release
					return nil
				})
		}()
		<-inCheck

		ring := ringOf(t, rotV2, rotV1)
		writerDone := make(chan error, 1)
		go func() {
			writerDone <- e.store(ring).PutSecret(e.ctx(tenant), tenant.String(), name, "sk-v2")
		}()
		select {
		case err := <-writerDone:
			t.Fatalf("the writer finished (%v) while a worker was mid-boot: it read a registry that did "+
				"not yet hold the booting worker", err)
		case <-time.After(400 * time.Millisecond):
		}

		close(release)
		if err := <-workerDone; err != nil {
			t.Fatalf("the worker: %v", err)
		}
		err := <-writerDone
		var gate *SecretKeyGateError
		if !errors.As(err, &gate) || len(gate.Blocking) != 1 || gate.Blocking[0].Hostname != "booting-old" {
			t.Fatalf("writer err = %v, want a refusal naming the worker that was booting", err)
		}
		if e.rowExists(t, tenant, name) {
			t.Fatal("the writer wrote a row the booting worker cannot open")
		}
	})
}

// Two workers booting at once do not exclude each other on the dialects that can
// share the lock. MySQL's GET_LOCK is exclusive-only, so there they queue -- which
// is why this asserts only that both finish, and that the shared modes are the
// ones on PostgreSQL and SQL Server.
func TestTwoBootingWorkersBothGetThrough(t *testing.T) {
	forEachDialect(t, func(t *testing.T, e *rotationEnv) {
		e.clearWorkers(t)
		shortWaits(t, 10*time.Second, 10*time.Second)
		errs := make(chan error, 2)
		for i := 0; i < 2; i++ {
			go func(i int) {
				errs <- e.registry().RegisterUnderKeyGate(context.Background(),
					WorkerRegistration{WorkerID: fmt.Sprintf("two-%d-%s", i, uuid.NewString()), Hostname: "h", PID: i,
						SecretKeyVersions: []int{1}},
					func(context.Context) error { time.Sleep(50 * time.Millisecond); return nil })
			}(i)
		}
		for i := 0; i < 2; i++ {
			if err := <-errs; err != nil {
				t.Fatalf("a booting worker: %v", err)
			}
		}
		if n := e.registeredHosts(t); n != 2 {
			t.Fatalf("%d registered, want 2", n)
		}
	})
}

// A boot check that fails must not leave the gate held. On PostgreSQL and SQL
// Server the lock dies with the transaction. On MySQL GET_LOCK is SESSION-scoped:
// COMMIT does not release it and a pooled connection carries it back into the
// pool, so a lock that is not explicitly released stalls the next writer for its
// whole wait. Every exit path is exercised: an error from the check, a panic in
// it, and a check that succeeds.
func TestAFailedBootCheckDoesNotLeaveTheGateHeld(t *testing.T) {
	forEachDialect(t, func(t *testing.T, e *rotationEnv) {
		e.clearWorkers(t)
		shortWaits(t, 3*time.Second, 3*time.Second)
		tenant := e.tenants[0]
		const name = "cleat-1991-gate-release"
		e.claim(t, tenant, name)
		ring := ringOf(t, rotV1)

		regs := func(check func(context.Context) error) (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("recovered: %v", r)
				}
			}()
			return e.registry().RegisterUnderKeyGate(context.Background(),
				WorkerRegistration{WorkerID: uuid.NewString(), Hostname: "x", PID: 1, SecretKeyVersions: []int{1}}, check)
		}

		for label, check := range map[string]func(context.Context) error{
			"errors": func(context.Context) error { return errors.New("boot check refused") },
			"panics": func(context.Context) error { panic("boom") },
		} {
			if err := regs(check); err == nil {
				t.Fatalf("%s: the span reported success", label)
			}
			e.assertGateFree(t, "after a boot check that "+label)
			start := time.Now()
			err := e.store(ring).PutSecret(e.ctx(tenant), tenant.String(), name, "sk-"+label)
			if err != nil {
				var busy *KeyGateBusyError
				if errors.As(err, &busy) {
					t.Fatalf("after a boot check that %s, the next writer timed out after %s waiting for the "+
						"gate: it was left held", label, time.Since(start).Round(time.Millisecond))
				}
				t.Fatalf("after a boot check that %s, the next writer: %v", label, err)
			}
			if n := e.registeredHosts(t); n != 0 {
				t.Fatalf("after a boot check that %s, %d registry row(s) remain", label, n)
			}
		}
	})
}

// The writer's half of the same hygiene: a write that panics inside its span must
// not leave the gate held. Nothing else covers this path -- the worker side has a
// second layer (its registration is withdrawn on any non-success exit) and a
// writer does not.
func TestAPanickingWriteDoesNotLeaveTheGateHeld(t *testing.T) {
	forEachDialect(t, func(t *testing.T, e *rotationEnv) {
		e.clearWorkers(t)
		shortWaits(t, 3*time.Second, 3*time.Second)
		tenant := e.tenants[0]
		const name = "cleat-1991-gate-writer-panic"
		e.claim(t, tenant, name)
		ring := ringOf(t, rotV1)

		bad := e.store(ring)
		bad.beforeGateCheck = func() { panic("boom inside the span") }
		func() {
			defer func() { _ = recover() }()
			_ = bad.PutSecret(e.ctx(tenant), tenant.String(), name, "sk")
			t.Error("the panic did not propagate; the hook did not run")
		}()

		e.assertGateFree(t, "after a write that panicked")
		start := time.Now()
		err := e.store(ring).PutSecret(e.ctx(tenant), tenant.String(), name, "sk-after")
		var busy *KeyGateBusyError
		if errors.As(err, &busy) {
			t.Fatalf("after a write that panicked, the next writer timed out after %s: the gate was left held",
				time.Since(start).Round(time.Millisecond))
		}
		if err != nil {
			t.Fatalf("the next writer: %v", err)
		}
	})
}

// Lock waits are bounded and the error says who is being waited for, on every
// dialect and in both directions.
func TestALockWaitTimesOutWithAnErrorThatNamesWhatItWaitedFor(t *testing.T) {
	forEachDialect(t, func(t *testing.T, e *rotationEnv) {
		e.clearWorkers(t)
		shortWaits(t, 1*time.Second, 1*time.Second)
		tenant := e.tenants[0]
		const name = "cleat-1991-gate-busy"
		e.claim(t, tenant, name)
		ring := ringOf(t, rotV1)

		// A worker mid-boot holds the gate shared (exclusive on MySQL); a writer waits.
		inCheck := make(chan struct{})
		release := make(chan struct{})
		workerDone := make(chan error, 1)
		go func() {
			workerDone <- e.registry().RegisterUnderKeyGate(context.Background(),
				WorkerRegistration{WorkerID: uuid.NewString(), Hostname: "slow-boot", PID: 1, SecretKeyVersions: []int{1}},
				func(context.Context) error { close(inCheck); <-release; return nil })
		}()
		<-inCheck
		err := e.store(ring).PutSecret(e.ctx(tenant), tenant.String(), name, "sk")
		var busy *KeyGateBusyError
		if !errors.As(err, &busy) || busy.Mode != keyGateExclusive {
			t.Fatalf("writer err = %v, want a *KeyGateBusyError in exclusive mode", err)
		}
		if !strings.Contains(err.Error(), "workers that are booting") {
			t.Errorf("the writer's timeout should say it is waiting for booting workers:\n%s", err)
		}
		close(release)
		if err := <-workerDone; err != nil {
			t.Fatal(err)
		}

		// A writer mid-write holds it exclusively; a booting worker waits.
		atHook := make(chan struct{})
		free := make(chan struct{})
		w := e.store(ring)
		w.beforeGateCheck = func() { close(atHook); <-free }
		writerDone := make(chan error, 1)
		go func() { writerDone <- w.PutSecret(e.ctx(tenant), tenant.String(), name, "sk") }()
		<-atHook
		err = e.registry().RegisterUnderKeyGate(context.Background(),
			WorkerRegistration{WorkerID: uuid.NewString(), Hostname: "blocked", PID: 1, SecretKeyVersions: []int{1}},
			func(context.Context) error { return nil })
		if !errors.As(err, &busy) || busy.Mode != keyGateShared {
			t.Fatalf("worker err = %v, want a *KeyGateBusyError in shared mode", err)
		}
		if !strings.Contains(err.Error(), "a set-secret or reseal-secrets is running") {
			t.Errorf("the worker's timeout should say a secret write is running:\n%s", err)
		}
		close(free)
		if err := <-writerDone; err != nil {
			t.Fatal(err)
		}
	})
}

// A registration for a worker id that is already registered REPLACES it: this is
// how a worker that lapsed re-registers with a fresh key set, and a plain INSERT
// would fail on the primary key of a row that was stale but not yet swept.
func TestReRegisteringReplacesTheRowAndItsKeySet(t *testing.T) {
	forEachDialect(t, func(t *testing.T, e *rotationEnv) {
		e.clearWorkers(t)
		id := uuid.NewString()
		for _, versions := range [][]int{{1}, {1, 2}} {
			err := e.registry().RegisterUnderKeyGate(context.Background(),
				WorkerRegistration{WorkerID: id, Hostname: "h", PID: 1, SecretKeyVersions: versions},
				func(context.Context) error { return nil })
			if err != nil {
				t.Fatalf("register %v: %v", versions, err)
			}
		}
		if n := e.registeredHosts(t); n != 1 {
			t.Fatalf("%d rows for one worker id, want 1", n)
		}
		sets, err := e.registry().liveKeySets(context.Background(), e.owner, time.Minute)
		if err != nil || len(sets) != 1 || len(sets[0].Versions) != 2 {
			t.Fatalf("live key sets = %+v, %v; want one worker holding {1,2}", sets, err)
		}
	})
}

// The pure decision and the column's encoding, no database.
func TestTheGateDecisionAndTheKeySetEncoding(t *testing.T) {
	w := func(known bool, vs ...int) WorkerKeys { return WorkerKeys{WorkerID: "w", Known: known, Versions: vs} }
	cases := []struct {
		name    string
		v       int
		workers []WorkerKeys
		blocked int
	}{
		{"no workers registered", 2, nil, 0},
		{"everyone opens it", 2, []WorkerKeys{w(true, 1, 2), w(true, 2)}, 0},
		{"one worker does not", 2, []WorkerKeys{w(true, 1, 2), w(true, 1)}, 1},
		{"unlabelled worker opens version 1", 1, []WorkerKeys{w(false)}, 0},
		{"unlabelled worker does not open version 2", 2, []WorkerKeys{w(false)}, 1},
		{"a keyless worker opens nothing", 1, []WorkerKeys{w(true)}, 1},
	}
	for _, c := range cases {
		if got := len(mayWrite(c.v, c.workers)); got != c.blocked {
			t.Errorf("%s: %d blocking, want %d", c.name, got, c.blocked)
		}
	}
	if got := encodeKeyVersions([]int{2, 1}); got != "1,2" {
		t.Errorf("encode = %q, want ascending", got)
	}
	if got := encodeKeyVersions(nil); got != "" {
		t.Errorf("encode(nil) = %q, want the empty string (a worker with no key), not NULL", got)
	}
	for _, corrupt := range []string{"1,x", "0", "-3", "1;2"} {
		if got := decodeKeyVersions(corrupt); got != nil {
			t.Errorf("decode(%q) = %v: a corrupt value must open NOTHING, so it blocks writes", corrupt, got)
		}
	}
}

// EVERY STATEMENT THAT WRITES key_version RUNS INSIDE gatedWrite.
//
// The gate protects a write only if the write goes through it, and a caller that
// runs the statement directly is the exact shape of a mechanism that exists and
// is wired to nothing (CLAUDE.md, Is this result real, #5). This reads the source
// with go/parser and asks, for every reference to a statement builder that writes
// the column, whether it sits inside a function literal handed to gatedWrite.
func TestEverySecretWriteRunsInsideTheGate(t *testing.T) {
	writers := map[string]bool{
		"putSecretUpdateStmt": true, "putSecretInsertStmt": true, "resealSecretStmt": true,
	}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob: %v (%d files)", err, len(files))
	}
	var ungated []string
	seen := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		u, n := ungatedKeyVersionWrites(t, f, string(src), writers)
		ungated = append(ungated, u...)
		seen += n
	}
	if seen < 3 {
		t.Fatalf("saw %d uses of the statement builders, want at least 3: the scan is not looking at the code", seen)
	}
	if len(ungated) > 0 {
		t.Fatalf("a statement that writes key_version is not inside gatedWrite: %s", strings.Join(ungated, "; "))
	}

	// Known-positive: the same scan must report a write that bypasses the gate.
	bad := `package p
func (s *S) Bad() error { return s.execTenantScoped(nil, func(q querier) error { _, e := q.ExecContext(nil, putSecretUpdateStmt("x")); return e }) }
func (s *S) Good() error { return s.gatedWrite(nil, 1, func(q querier) error { _, e := q.ExecContext(nil, putSecretUpdateStmt("x")); return e }) }
`
	u, n := ungatedKeyVersionWrites(t, "synthetic.go", bad, writers)
	if n != 2 || len(u) != 1 || !strings.Contains(u[0], "Bad") {
		t.Fatalf("known-positive: %d uses, ungated %v; want 2 uses with exactly Bad reported", n, u)
	}
}

// ungatedKeyVersionWrites returns the uses of a writer builder (other than its
// own declaration) that are not inside a function literal passed to gatedWrite.
func ungatedKeyVersionWrites(t *testing.T, name, src string, writers map[string]bool) (ungated []string, uses int) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || writers[fd.Name.Name] {
			continue
		}
		var gated func(n ast.Node, inGate bool)
		gated = func(n ast.Node, inGate bool) {
			ast.Inspect(n, func(x ast.Node) bool {
				switch c := x.(type) {
				case *ast.CallExpr:
					if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "gatedWrite" {
						for _, a := range c.Args {
							if lit, ok := a.(*ast.FuncLit); ok {
								gated(lit.Body, true)
							} else {
								gated(a, inGate)
							}
						}
						return false
					}
					if id, ok := c.Fun.(*ast.Ident); ok && writers[id.Name] {
						uses++
						if !inGate {
							ungated = append(ungated, fmt.Sprintf("%s: %s uses %s outside gatedWrite",
								name, fd.Name.Name, id.Name))
						}
					}
				}
				return true
			})
		}
		gated(fd.Body, false)
	}
	return ungated, uses
}
