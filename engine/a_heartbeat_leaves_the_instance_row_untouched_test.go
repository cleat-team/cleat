package engine

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// cleat#3245 B2: on PostgreSQL heartbeat_at lives only on workflow_leases.
// Every write path that renews or clears it writes the lease row, and the
// workflow_instances copy is never written -- which is what stops a heartbeat
// rewriting the instance tuple every few seconds. The two readers,
// CountStalledWorkflows and StaleSetShape, read the lease row.
//
// The write-side subtests plant a sentinel in the instance column first and
// assert it survives, while the lease column holds the value the path is
// meant to write. Asserting NULL would not do: the release paths CLEAR
// heartbeat_at, so a path still writing NULL to workflow_instances would pass
// a NULL check -- measured, on develop's tree, for reap, retry and re-replay. The reader tests make the two copies disagree and check which one
// was read.

func instanceHeartbeat(t *testing.T, db *sql.DB, id string) sql.NullTime {
	t.Helper()
	var v sql.NullTime
	if err := db.QueryRow(`SELECT heartbeat_at FROM workflow_instances WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatalf("read workflow_instances.heartbeat_at: %v", err)
	}
	return v
}

// leaseHeartbeatAge returns how long ago workflow_leases.heartbeat_at was, or
// ok=false when it is NULL.
func leaseHeartbeatAge(t *testing.T, db *sql.DB, id string) (age time.Duration, ok bool) {
	t.Helper()
	var secs sql.NullFloat64
	if err := db.QueryRow(`SELECT EXTRACT(EPOCH FROM now() - heartbeat_at) FROM workflow_leases WHERE id = $1`, id).Scan(&secs); err != nil {
		t.Fatalf("read workflow_leases.heartbeat_at: %v", err)
	}
	if !secs.Valid {
		return 0, false
	}
	return time.Duration(secs.Float64 * float64(time.Second)), true
}

func TestAHeartbeatLeavesTheInstanceRowUntouched(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()
	const worker = "b2-worker"

	claim := func(t *testing.T, id string) int64 {
		t.Helper()
		wf, err := store.ClaimWorkflow(ctx, worker)
		if err != nil {
			t.Fatalf("ClaimWorkflow: %v", err)
		}
		if wf == nil || wf.ID != id {
			t.Fatalf("ClaimWorkflow claimed %v, want %s", wf, id)
		}
		return wf.Generation
	}
	ageLease := func(t *testing.T, id string) {
		t.Helper()
		if _, err := db.Exec(`UPDATE workflow_leases SET heartbeat_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
			t.Fatalf("age the lease heartbeat: %v", err)
		}
	}
	wantFresh := func(t *testing.T, id string) {
		t.Helper()
		if age, ok := leaseHeartbeatAge(t, db, id); !ok || age > time.Minute {
			t.Errorf("workflow_leases.heartbeat_at age=%v set=%v, want renewed just now", age, ok)
		}
	}
	wantCleared := func(t *testing.T, id string) {
		t.Helper()
		if age, ok := leaseHeartbeatAge(t, db, id); ok {
			t.Errorf("workflow_leases.heartbeat_at is %v old, want NULL after a release", age)
		}
	}

	paths := []struct {
		name string
		run  func(t *testing.T, id string)
	}{
		{"claim", func(t *testing.T, id string) {
			claim(t, id)
			wantFresh(t, id)
		}},
		{"heartbeat", func(t *testing.T, id string) {
			gen := claim(t, id)
			ageLease(t, id)
			ok, err := store.Heartbeat(ctx, id, worker, gen)
			if err != nil || !ok {
				t.Fatalf("Heartbeat = %v, %v; want true, nil", ok, err)
			}
			wantFresh(t, id)
		}},
		{"heartbeat with a lost fence", func(t *testing.T, id string) {
			gen := claim(t, id)
			ageLease(t, id)
			ok, err := store.Heartbeat(ctx, id, worker, gen+1)
			if err != nil || ok {
				t.Fatalf("Heartbeat with a stale generation = %v, %v; want false, nil", ok, err)
			}
			if age, set := leaseHeartbeatAge(t, db, id); !set || age < 30*time.Minute {
				t.Errorf("a heartbeat that lost its fence renewed the lease (age=%v)", age)
			}
		}},
		{"batch heartbeat", func(t *testing.T, id string) {
			gen := claim(t, id)
			ageLease(t, id)
			lost, err := store.HeartbeatBatchFenced(ctx, worker, []GenerationKey{{WorkflowID: id, Generation: gen}})
			if err != nil || len(lost) != 0 {
				t.Fatalf("HeartbeatBatchFenced = %v, %v; want no lost runs", lost, err)
			}
			wantFresh(t, id)
		}},
		{"reap", func(t *testing.T, id string) {
			claim(t, id)
			ageLease(t, id)
			n, err := store.ReapStaleInstances(ctx, time.Minute, 10)
			if err != nil || n != 1 {
				t.Fatalf("ReapStaleInstances = %d, %v; want 1 reclaimed", n, err)
			}
			wantCleared(t, id)
		}},
		{"retry", func(t *testing.T, id string) {
			gen := claim(t, id)
			if err := store.MoveToDeadLetterQueue(ctx, id, worker, gen, "boom", "E_B2", "op", nil); err != nil {
				t.Fatalf("MoveToDeadLetterQueue: %v", err)
			}
			if err := store.RetryWorkflow(ctx, id); err != nil {
				t.Fatalf("RetryWorkflow: %v", err)
			}
			wantCleared(t, id)
		}},
		{"admin re-replay", func(t *testing.T, id string) {
			gen := claim(t, id)
			if err := store.FailWorkflow(ctx, id, worker, gen, "boom", "E_B2", "op", nil); err != nil {
				t.Fatalf("FailWorkflow: %v", err)
			}
			if err := store.AdminReReplay(ctx, id, gen, "operator"); err != nil {
				t.Fatalf("AdminReReplay: %v", err)
			}
			wantCleared(t, id)
		}},
	}
	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			if _, err := db.Exec(`DELETE FROM workflow_instances`); err != nil {
				t.Fatalf("clear workflow_instances: %v", err)
			}
			id := startPayloadReaderRun(t, store, "b2-"+p.name)
			sentinel := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
			diverge(t, db, "workflow_instances", `heartbeat_at = $2`, id, sentinel)
			p.run(t, id)
			if v := instanceHeartbeat(t, db, id); !v.Valid || !v.Time.Equal(sentinel) {
				t.Errorf("workflow_instances.heartbeat_at = %v (set=%v), want the planted %v untouched -- a write path still puts heartbeat_at on workflow_instances", v.Time, v.Valid, sentinel)
			}
		})
	}
}

// staleLeaseFreshInstance claims a run, then gives the lease row an hour-old
// heartbeat and creation time and the instance row a fresh heartbeat: a
// reader of workflow_leases sees a stalled run, a reader of
// workflow_instances sees a healthy one.
func staleLeaseFreshInstance(t *testing.T, store *PostgresStore, db *sql.DB, name string) string {
	t.Helper()
	id := startPayloadReaderRun(t, store, name)
	if wf, err := store.ClaimWorkflow(context.Background(), "b2-reader-worker"); err != nil || wf == nil || wf.ID != id {
		t.Fatalf("ClaimWorkflow = %v, %v; want %s", wf, err, id)
	}
	diverge(t, db, "workflow_leases", `heartbeat_at = now() - interval '1 hour', created_at = now() - interval '2 hours'`, id)
	diverge(t, db, "workflow_instances", `heartbeat_at = now()`, id)
	return id
}

func TestCountStalledWorkflowsReadsHeartbeatAtFromWorkflowLeases(t *testing.T) {
	store, db := newRunTestStore(t)
	staleLeaseFreshInstance(t, store, db, "b2-count-stalled")

	n, err := store.CountStalledWorkflows(context.Background(), 30*time.Minute)
	if err != nil {
		t.Fatalf("CountStalledWorkflows: %v", err)
	}
	if n != 1 {
		t.Fatalf("CountStalledWorkflows = %d, want 1 -- 0 means heartbeat_at or created_at was read from workflow_instances, where the run looks healthy", n)
	}
}

func TestStaleSetShapeReadsHeartbeatAtFromWorkflowLeases(t *testing.T) {
	store, db := newRunTestStore(t)
	staleLeaseFreshInstance(t, store, db, "b2-stale-shape")

	shape, err := store.StaleSetShape(context.Background(), 30*time.Minute, time.Minute)
	if err != nil {
		t.Fatalf("StaleSetShape: %v", err)
	}
	if shape.Running != 1 || shape.MissedBeat != 1 {
		t.Fatalf("StaleSetShape running=%d missedBeat=%d, want 1 and 1 -- missedBeat 0 means heartbeat_at was read from workflow_instances", shape.Running, shape.MissedBeat)
	}
}
