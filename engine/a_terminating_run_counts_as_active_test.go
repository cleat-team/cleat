package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// stampDefCreatedAt sets a definition's created_at to an instant the TEST names, taken from the
// host clock, then reads it back through the store. PurgeVersions compares created_at (written
// by the database, on its clock) with a cutoff taken on the host's clock, so a test that purges
// straight after a deploy is comparing two clocks; stamping the definition from the host's own
// clock leaves one (cleat#3563). The read-back is the precondition: a stamp that did not take, or
// took in another time zone, would make the purge below measure nothing.
func stampDefCreatedAt(t *testing.T, store WorkflowStore, name string, version int, at time.Time) {
	t.Helper()
	at = at.UTC()
	switch s := store.(type) {
	case *PostgresStore:
		if _, err := s.db.Exec(`UPDATE workflow_defs SET created_at = $1 WHERE name = $2 AND version = $3`, at, name, version); err != nil {
			t.Fatalf("UNMEASURED: stampDefCreatedAt (postgres): %v", err)
		}
	case *MySQLStore:
		if _, err := s.db.Exec(`UPDATE workflow_defs SET created_at = ? WHERE name = ? AND version = ?`, at, name, version); err != nil {
			t.Fatalf("UNMEASURED: stampDefCreatedAt (mysql): %v", err)
		}
	case *MSSQLStore:
		if _, err := s.db.Exec(`UPDATE workflow_defs SET created_at = @p1 WHERE name = @p2 AND version = @p3`, at, name, version); err != nil {
			t.Fatalf("UNMEASURED: stampDefCreatedAt (mssql): %v", err)
		}
	default:
		t.Fatalf("UNMEASURED: stampDefCreatedAt: unknown store type %T", store)
	}
	defs, err := store.ListWorkflowDefs(context.Background(), name)
	if err != nil || len(defs) != 1 {
		t.Fatalf("UNMEASURED: reading %s back after stamping it: %d definition(s), %v", name, len(defs), err)
	}
	if d := defs[0].CreatedAt.Sub(at); d < -time.Second || d > time.Second {
		t.Fatalf("UNMEASURED: created_at reads back as %s after stamping %s (off by %s), so the purge below would compare the wrong instants",
			defs[0].CreatedAt.UTC().Format(time.RFC3339Nano), at.Format(time.RFC3339Nano), d)
	}
}

// cleat#3506. CountActiveInstances and GetActiveInstanceCountsByVersion counted only
// 'ready' and 'running'. A 'terminating' run is mid-shutdown, running its defer phase,
// so it is not settled (engine/status_vocabulary.go), and the defer phase executes guest
// code, which needs the definition's bytes. The version GC trusts these counts to decide
// that a version is idle and purge it, so a version whose only open run was terminating
// read as idle. Every case reaches 'terminating' through the store (a registered defer,
// then AdminForceComplete), and asserts it did before asking the counts anything,
// because a count of zero is also what no run at all gives.
func TestATerminatingRunCountsAsActive(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			deploy := func(t *testing.T, name string) {
				t.Helper()
				if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
					Name: name, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x0a}, ABIVersion: 1, MinVersion: 1,
				}); err != nil {
					t.Fatalf("DeployWorkflowDef: %v", err)
				}
			}
			startRun := func(t *testing.T, name string) string {
				t.Helper()
				id, existed, err := store.StartNewRun(ctx, "", name, 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
				if err != nil || existed {
					t.Fatalf("UNMEASURED: StartNewRun (existed=%v): %v", existed, err)
				}
				return id
			}
			status := func(t *testing.T, id string) string {
				t.Helper()
				inst, err := store.GetWorkflowByID(ctx, id)
				if err != nil || inst == nil {
					t.Fatalf("UNMEASURED: cannot read run %s: %v", id, err)
				}
				return inst.Status
			}
			// toTerminating owes a defer phase, then force-completes, which leaves the run
			// 'terminating' with the terminal write pending.
			toTerminating := func(t *testing.T, id string) {
				t.Helper()
				if err := store.AppendEventHistoryBatch(ctx, id, []EventRecord{
					{Step: 0, EventType: EventTypeDefer, Service: "cleanup", Op: "release"},
				}); err != nil {
					t.Fatalf("UNMEASURED: register defer: %v", err)
				}
				if err := store.AdminForceComplete(ctx, id, 0, `{"ok":true}`, "operator-1"); err != nil {
					t.Fatalf("UNMEASURED: AdminForceComplete: %v", err)
				}
				if got := status(t, id); got != statusTerminating {
					t.Fatalf("UNMEASURED: run %s is %q, not %q, so the counts below would test nothing", id, got, statusTerminating)
				}
			}

			t.Run("the counts include a terminating run", func(t *testing.T) {
				name := "terminating-counted-" + uuid.New().String()[:8]
				deploy(t, name)
				toTerminating(t, startRun(t, name))

				if n, err := store.CountActiveInstances(ctx, name, 1); err != nil || n != 1 {
					t.Errorf("CountActiveInstances = %d, %v; want 1 -- a terminating run is not settled and still runs its defer phase", n, err)
				}
				counts, err := store.GetActiveInstanceCountsByVersion(ctx)
				if err != nil {
					t.Fatalf("GetActiveInstanceCountsByVersion: %v", err)
				}
				if got := counts[fmt.Sprintf("%s:%d", name, 1)]; got != 1 {
					t.Errorf("GetActiveInstanceCountsByVersion[%s:1] = %d, want 1", name, got)
				}
			})

			t.Run("the GC does not purge a version whose only open run is terminating", func(t *testing.T) {
				name := "terminating-gc-" + uuid.New().String()[:8]
				deploy(t, name)
				toTerminating(t, startRun(t, name))
				if err := store.MarkVersionDeprecated(ctx, name, 1, true); err != nil {
					t.Fatalf("UNMEASURED: MarkVersionDeprecated: %v", err)
				}
				stampDefCreatedAt(t, store, name, 1, time.Now().Add(-time.Hour))

				res, err := PurgeVersions(ctx, store, name, time.Duration(0))
				if err != nil {
					t.Fatalf("PurgeVersions: %v", err)
				}
				t.Logf("PurgeVersions: removed=%d skipped=%d errors=%v", res.VersionsRemoved, res.VersionsSkipped, res.Errors)
				if res.VersionsRemoved != 0 || res.VersionsSkipped != 1 {
					t.Errorf("PurgeVersions removed=%d skipped=%d errors=%v; want removed 0 skipped 1 -- the version's only open run is terminating",
						res.VersionsRemoved, res.VersionsSkipped, res.Errors)
				}
				if _, err := store.LoadWASM(ctx, name, 1); err != nil {
					t.Errorf("the definition is gone after PurgeVersions: %v", err)
				}
			})

			t.Run("control: a version with no run at all is idle and is purged", func(t *testing.T) {
				name := "terminating-control-" + uuid.New().String()[:8]
				deploy(t, name)
				if err := store.MarkVersionDeprecated(ctx, name, 1, true); err != nil {
					t.Fatalf("UNMEASURED: MarkVersionDeprecated: %v", err)
				}
				stampDefCreatedAt(t, store, name, 1, time.Now().Add(-time.Hour))
				res, err := PurgeVersions(ctx, store, name, time.Duration(0))
				if err != nil {
					t.Fatalf("PurgeVersions: %v", err)
				}
				if res.VersionsRemoved != 1 {
					t.Fatalf("UNMEASURED: PurgeVersions removed=%d skipped=%d errors=%v; the control must purge an idle version, or 'not purged' above means nothing",
						res.VersionsRemoved, res.VersionsSkipped, res.Errors)
				}
			})

			// The failure cleat#3563 recorded, made deterministic. A definition whose created_at is
			// after the host's cutoff is not a candidate at all, so PurgeVersions reports it neither
			// removed nor skipped, with no error. That is what a database clock a few tens of
			// milliseconds ahead of the host's looked like to the two subtests above, which is why
			// they stamp created_at from the host clock instead of trusting the database's.
			t.Run("a definition stamped after the cutoff is not a candidate, which is how a clock ahead reads", func(t *testing.T) {
				name := "terminating-ahead-" + uuid.New().String()[:8]
				deploy(t, name)
				if err := store.MarkVersionDeprecated(ctx, name, 1, true); err != nil {
					t.Fatalf("UNMEASURED: MarkVersionDeprecated: %v", err)
				}
				stampDefCreatedAt(t, store, name, 1, time.Now().Add(time.Hour))
				res, err := PurgeVersions(ctx, store, name, time.Duration(0))
				if err != nil {
					t.Fatalf("PurgeVersions: %v", err)
				}
				if res.VersionsRemoved != 0 || res.VersionsSkipped != 0 || len(res.Errors) != 0 {
					t.Errorf("PurgeVersions removed=%d skipped=%d errors=%v; want 0, 0 and none: a definition newer than the cutoff is not a candidate",
						res.VersionsRemoved, res.VersionsSkipped, res.Errors)
				}
				if _, err := store.LoadWASM(ctx, name, 1); err != nil {
					t.Errorf("the definition is gone after PurgeVersions: %v", err)
				}
			})
		})
	}
}
