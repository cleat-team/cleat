package main

// A worker that lapsed re-registers AND re-runs the secrets check before it
// resumes (cleat#1991).
//
// While a worker's membership loop is stalled nothing is watching for it: a writer
// can judge it gone (its heartbeat is older than the live window) and write a key
// version it cannot open. The row may not even have been swept, in which case the
// heartbeat succeeds and says nothing -- which is why a LAPSE, and not only a
// swept row, has to trigger the re-check. These tests pin that, and that the
// answer is acted on: a definite "cannot open" stops the worker, a read that
// failed does not.

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/google/uuid"
)

type lapseEnv struct {
	db     *sql.DB
	reg    *engine.WorkerRegistry
	w      *Worker
	cancel context.CancelFunc
	id     string
}

func newLapseEnv(t *testing.T, ring *engine.KeyRing, storeDB *sql.DB) *lapseEnv {
	t.Helper()
	db := testutil.SuiteTestDB(t, "cleat_worker")
	if storeDB == nil {
		storeDB = db
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reg := &engine.WorkerRegistry{DB: db, Dialect: engine.DialectPostgres}
	id := fmt.Sprintf("lapse-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = reg.Deregister(context.Background(), id) })
	w := &Worker{
		id:             id,
		ctx:            ctx,
		cancel:         cancel,
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:        newTestPrometheus(),
		workerRegistry: reg,
		secrets:        engine.NewSecretStoreWithRing(storeDB, "postgres", ring),
	}
	return &lapseEnv{db: db, reg: reg, w: w, cancel: cancel, id: id}
}

func (e *lapseEnv) publishedKeys(t *testing.T) (value string, present bool) {
	t.Helper()
	var v sql.NullString
	err := e.db.QueryRow(`SELECT secret_key_versions FROM admin.workers WHERE worker_id = $1`, e.id).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false
	}
	if err != nil {
		t.Fatalf("read the published key set: %v", err)
	}
	return v.String, true
}

func ringV1(t *testing.T) *engine.KeyRing {
	t.Helper()
	r, err := engine.NewKeyRing(engine.VersionedKey{Version: 1, Key: []byte("0123456789abcdef0123456789abcdef")})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A lapse re-registers even though the heartbeat SUCCEEDS, and publishes the
// worker's current key set. The control -- the same tick with no lapse -- leaves
// the (deliberately stale) published set alone, so the change below is the lapse
// and not the tick.
func TestALapsedWorkerRegistersAgainAndPublishesItsKeys(t *testing.T) {
	e := newLapseEnv(t, ringV1(t), nil)
	ctx := context.Background()
	if err := e.reg.Register(ctx, engine.WorkerRegistration{
		WorkerID: e.id, Hostname: "h", PID: 1, SecretKeyVersions: []int{9}}); err != nil {
		t.Fatal(err)
	}
	const hold = 10 * time.Second

	e.w.membershipLastBeat = time.Now()
	e.w.membershipTick(hold)
	if got, ok := e.publishedKeys(t); !ok || got != "9" {
		t.Fatalf("CONTROL: an ordinary tick changed the published set to %q (present=%v); "+
			"only a lapse may re-register", got, ok)
	}

	e.w.membershipLastBeat = time.Now().Add(-time.Hour)
	e.w.membershipTick(hold)
	if got, ok := e.publishedKeys(t); !ok || got != "1" {
		t.Fatalf("after a lapse the published set is %q (present=%v), want %q: a worker that was "+
			"not being watched has not re-registered", got, ok, "1")
	}
	if time.Since(e.w.membershipLastBeat) > time.Minute {
		t.Fatalf("membershipLastBeat was not refreshed after a successful tick")
	}
}

// A lapsed worker that finds a stored secret it cannot open STOPS. Serving would
// fail the first workflow that resolves the secret. The registration is withdrawn
// so it does not block writers.
func TestALapsedWorkerThatCannotOpenAStoredSecretStopsInsteadOfServing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, e *lapseEnv)
		// after says what the registry may hold once the worker has refused.
		// A refused re-registration ROLLS BACK, so where the old row was still
		// there it is still there, exactly as it was, until the worker's shutdown
		// path deregisters it; where it had been swept nothing may reappear.
		after func(t *testing.T, e *lapseEnv)
	}{
		{"row still present but the loop stalled", func(t *testing.T, e *lapseEnv) {
			e.w.membershipLastBeat = time.Now().Add(-time.Hour)
		}, func(t *testing.T, e *lapseEnv) {
			if got, _ := e.publishedKeys(t); got != "1,7" {
				t.Fatalf("the registry holds %q after a refusal, want the OLD row {1,7} untouched: "+
					"the refused registration must publish nothing", got)
			}
		}},
		{"row swept while it stalled", func(t *testing.T, e *lapseEnv) {
			if err := e.reg.Deregister(context.Background(), e.id); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, e *lapseEnv) {
			if _, present := e.publishedKeys(t); present {
				t.Fatal("the refused worker left a registration behind; it would block writers")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newLapseEnv(t, ringV1(t), nil)
			ctx := context.Background()
			if err := e.reg.Register(ctx, engine.WorkerRegistration{
				WorkerID: e.id, Hostname: "h", PID: 1, SecretKeyVersions: []int{1, 7}}); err != nil {
				t.Fatal(err)
			}

			// A stored secret sealed under version 2: this worker holds only 1. Raw
			// SQL, because writing it through PutSecret would (correctly) be
			// refused by the gate this feature adds.
			name := "cleat-1991-lapse-" + uuid.NewString()[:8]
			tenant := engine.DefaultTenantUUID
			if _, err := e.db.Exec(`INSERT INTO tenant_secrets (tenant_id, name, ciphertext, key_version)
				VALUES ($1, $2, 'x', 2)`, tenant, name); err != nil {
				t.Fatalf("seed a version-2 secret: %v", err)
			}
			t.Cleanup(func() { _, _ = e.db.Exec(`DELETE FROM tenant_secrets WHERE name = $1`, name) })

			// CONTROL: no lapse, row present -> the tick does not even run the check,
			// so nothing stops the worker. Without this the assertion below could pass
			// for any reason that cancels the context.
			e.w.membershipLastBeat = time.Now()
			e.w.membershipTick(10 * time.Second)
			if e.w.ctx.Err() != nil {
				t.Fatal("CONTROL: an ordinary tick stopped the worker")
			}

			tc.setup(t, e)
			e.w.membershipTick(10 * time.Second)
			if e.w.ctx.Err() == nil {
				t.Fatal("a lapsed worker that cannot open a stored secret kept serving")
			}
			tc.after(t, e)
		})
	}
}

// A read that FAILS is not a definite answer, and a serving worker must not stop
// on it: a database that cannot be read is failing everything else too, and a
// stopped worker is not evidence of safety. (At STARTUP the same failure refuses,
// because there the alternative is to begin serving unchecked.)
func TestALapsedWorkersFailedCheckIsNotADefiniteRefusal(t *testing.T) {
	// A store whose database cannot be reached: the check errors, it does not answer.
	dead, err := sql.Open("postgres", "host=127.0.0.1 port=1 user=x dbname=x sslmode=disable connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dead.Close() })

	e := newLapseEnv(t, ringV1(t), dead)
	e.w.membershipLastBeat = time.Now().Add(-time.Hour)
	e.w.membershipTick(10 * time.Second)
	if e.w.ctx.Err() != nil {
		t.Fatal("a check that could not READ stopped the worker; only a definite 'cannot open' may")
	}
	if _, present := e.publishedKeys(t); present {
		t.Fatal("a failed check left a registration behind")
	}
}
