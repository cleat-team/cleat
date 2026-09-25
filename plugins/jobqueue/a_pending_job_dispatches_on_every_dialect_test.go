// TestPollPendingDispatchesOnEveryDialect pins cleat#2257: pollPending
// (background.go) scanned the input column into a bare *json.RawMessage.
// json.RawMessage is a named []byte type, and database/sql's convertAssign
// fast path does not convert a driver string into one -- go-mssqldb returns
// NVARCHAR as string, so this failed to scan on every row, NULL included.
// The row was logged and skipped, so a pending job was never claimed, never
// dispatched, and StartWorkflow was never called -- on SQL Server, no job in
// this queue is ever dispatched at all. Fixed via plugin.JSONColumn, same as
// #2256 did for the enqueue INSERT and the route scans.
//
// This goes through the real pollPending function against a real database,
// not the fake-driver suite (jobqueue_behavioral_test.go's TestPollPending),
// which pattern-matches the query string and would accept SQL no database
// would -- the same reasoning jobqueue_abandonment_sweep_multidb_test.go's
// package comment gives for that file.
package jobqueue

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/plugintest"
)

func TestPollPendingDispatchesOnEveryDialect(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			baseCtx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))
			p := &Plugin{dialect: dialect}

			// Before CrossTenantConn -- see
			// TestSweepAbandonedJobs_MultiBackend's identical comment in
			// jobqueue_abandonment_sweep_multidb_test.go.
			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			if err := plugin.RunMigrations(baseCtx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("jobqueue migrations on %s: %v", be.Name, err)
			}
			fixtureDB := be.CrossTenantConn(t, context.Background(),
				"jobqueue pollPending fixture: seeds a pending row for a tenant it invents")
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}
			p.logger = slog.New(slog.NewTextHandler(io.Discard, nil))

			fakeEnv := newFakeEnvironment()
			p.env = &fakeEnv.Environment

			tenant := uuid.New()
			job := uuid.New()
			defName := "poll-test-" + uuid.New().String()
			wantInput := `{"n":1}`

			tenant = seedTenant(t, fixtureDB, baseCtx, be.Dialect, dialect, tenant)

			defer func() {
				if _, err := plugintest.ExecRebound(t, context.Background(), fixtureDB, dialect,
					`DELETE FROM task_queue WHERE tenant_id = $1`, tenant); err != nil {
					t.Errorf("cleanup task_queue on %s: %v", be.Name, err)
				}
				cleanupTenant(t, fixtureDB, be.Dialect, tenant)
			}()

			if _, err := plugintest.ExecRebound(t, baseCtx, fixtureDB, dialect, `
				INSERT INTO task_queue (tenant_id, queue_name, job_id, payload, status, def_name, input)
				VALUES ($1, $2, $3, $4, 'pending', $5, $6)
			`, tenant, "poll-test", job,
				plugin.JSONColumn{Raw: json.RawMessage(`{}`)}, defName,
				plugin.JSONColumn{Raw: json.RawMessage(wantInput)}); err != nil {
				t.Fatalf("insert pending job on %s: %v", be.Name, err)
			}

			// ASSERT THE EFFECT, NOT THE ABSENCE OF AN ERROR: pollPending logs
			// and continues past a row it cannot scan, so a broken scan looks
			// exactly like a queue with nothing pending in it -- precisely how
			// this bug went unnoticed. Read every one of (claimed, dispatched,
			// StartWorkflow's own recorded input, and the row's final status)
			// rather than just checking err == nil.
			ctx := plugin.AcrossAllTenants(baseCtx,
				"jobqueue pollPending test: the poll operates across every tenant's queue, as Run does")
			claimed, dispatched, failed, err := p.pollPending(ctx)
			if err != nil {
				t.Fatalf("pollPending on %s: %v", be.Name, err)
			}
			// claimed is NOT asserted == 1: pollPending is deliberately
			// cross-tenant (AcrossAllTenants above, matching Run), and this
			// package's other real-DB tests -- TestAJobListWorksOnEveryDialect
			// enqueues two jobs to prove handleEnqueue/handleListJobs and
			// never dispatches or removes them, by design -- can leave their
			// own pending rows sitting in this same shared database. Those
			// get claimed here too. What this test actually pins is scoped to
			// its own job below (defName, input, and the row's own status),
			// which ambient rows from sibling tests cannot satisfy by
			// coincidence.
			if claimed < 1 {
				t.Errorf("pollPending on %s: claimed = %d, want at least 1 -- the row was never scanned", be.Name, claimed)
			}
			if dispatched < 1 {
				t.Errorf("pollPending on %s: dispatched = %d, want at least 1", be.Name, dispatched)
			}
			if failed != 0 {
				t.Errorf("pollPending on %s: failed = %d, want 0", be.Name, failed)
			}

			fakeEnv.mu.Lock()
			calls := fakeEnv.wfCalls
			fakeEnv.mu.Unlock()
			// Found by def_name, not calls[0]: ambient rows from sibling
			// tests (see the claimed comment above) can add their own
			// StartWorkflow calls to this same fake environment.
			var ourCall *startWorkflowCall
			for i := range calls {
				if calls[i].defName == defName {
					ourCall = &calls[i]
					break
				}
			}
			if ourCall == nil {
				t.Fatalf("pollPending on %s: no StartWorkflow call for def_name %q among %d calls",
					be.Name, defName, len(calls))
			}
			// Compared as parsed JSON, not raw bytes: Postgres's jsonb column
			// re-serializes on the way back out (a space after ":"), which is
			// a real, harmless reformatting -- not the bug this test pins.
			var gotParsed, wantParsed any
			if err := json.Unmarshal(ourCall.input, &gotParsed); err != nil {
				t.Fatalf("pollPending on %s: dispatched input %q is not valid JSON: %v", be.Name, ourCall.input, err)
			}
			if err := json.Unmarshal([]byte(wantInput), &wantParsed); err != nil {
				t.Fatalf("invalid wantInput: %v", err)
			}
			if !reflect.DeepEqual(gotParsed, wantParsed) {
				t.Errorf("pollPending on %s: dispatched input = %s, want %s -- this is exactly "+
					"the scan this test pins", be.Name, ourCall.input, wantInput)
			}

			var status string
			if err := plugintest.QueryRowRebound(t, baseCtx, fixtureDB, dialect,
				`SELECT status FROM task_queue WHERE tenant_id = $1 AND queue_name = $2 AND job_id = $3`,
				tenant, "poll-test", job).Scan(&status); err != nil {
				t.Fatalf("read back on %s: %v", be.Name, err)
			}
			if status != "dispatched" {
				t.Errorf("on %s status = %q, want \"dispatched\"", be.Name, status)
			}
		})
	}
}
