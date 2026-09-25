// Regression coverage for the race cleat-review found in #2141:
// sweepStaleWorkflowRefsMSSQL used to read the in-flight set BEFORE the
// candidate set, so a workflow that started and wrote its first blob ref in
// the gap between the two reads was a candidate (present in
// workflow_blob_refs by the second read) invisible to an in-flight set
// captured before it existed -- and its ref was deleted on the very sweep
// that should have protected it. See background.go's doc comment on
// sweepStaleWorkflowRefsMSSQL for the fix (read candidates first) and the
// residual RetryWorkflow window that fix does not close.
//
// Two tests, two ways of triggering the same window:
//
//   - TestSweepStaleWorkflowRefsMSSQL_DeterministicInterleave uses
//     sweepStaleWorkflowRefsMSSQLTestHook to land a write in the exact gap
//     between the two reads, every run, in well under a second.
//   - TestSweepStaleWorkflowRefsMSSQL_RealConcurrencyKeepsANewWorkflowsRef (cleat-review's own
//     scratch reproduction, ported here) hits the same window through real
//     goroutine/database concurrency with no hook at all -- the case that
//     the deterministic test's hook could in principle be hiding something
//     from. cleat-review measured this losing 48 of 1201 refs (4%) on a
//     200-tenant SQL Server run and 3 of 771 on a 1-tenant run, both against
//     the pre-fix ordering.
package blobstore

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/plugintest"
)

func TestSweepStaleWorkflowRefsMSSQL_DeterministicInterleave(t *testing.T) {
	// MSSQL-only: NewPluginTestBackends(t) only returns an MSSQL backend
	// when CLEAT_TEST_MSSQL is set (and Fatals, rather than skipping, if it
	// is set but unreachable -- see MSSQLTestDB), so this is "not
	// requested", never "requested but unavailable". Without it, a
	// PostgreSQL-only job's loop below finds no MSSQL backend, runs zero
	// subtests and asserts nothing -- a vacuous pass, not a skip event, and
	// invisible to both scripts/check-skips.sh and
	// scripts/check-skip-budget.sh. cleat#2181 (e).
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server-only deterministic-interleave test")
	}
	for _, be := range testutil.NewPluginTestBackends(t) {
		if be.Dialect != testutil.DialectMSSQL {
			continue // the race is specific to sweepStaleWorkflowRefsMSSQL's two-read shape
		}
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			baseCtx := context.Background()
			ctx := plugin.AcrossAllTenants(baseCtx,
				"blobstore race test: cleanupExpired runs over every tenant's refs, as Run does")
			dialect := plugin.Dialect(string(be.Dialect))
			p := &Plugin{dialect: dialect}

			// SetupMinimalSchema (core: workflow_defs, workflow_instances) must
			// run before CrossTenantConn: on MSSQL that call now routes through
			// MSSQLAdminDB, which Fatals if RLS is enforced on the core tables
			// but the cleat_admin role migration 012 creates does not exist yet
			// -- true of this database until the schema below has applied.
			// cleat#2226.
			testutil.SetupMinimalSchema(t, be.DB, be.Dialect)
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("blobstore migrations on %s: %v", be.Name, err)
			}
			fixtureDB := be.CrossTenantConn(t, context.Background(),
				"blobstore race fixture: seeds a tenant and a workflow the hook writes into")
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}
			p.logger = slog.New(slog.NewTextHandler(io.Discard, nil))

			tenant := uuid.New()
			if _, err := plugintest.ExecRebound(t, ctx, fixtureDB, dialect,
				`INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)`,
				tenant.String(), "blobstore-race-"+tenant.String()); err != nil {
				t.Fatalf("seed admin.tenants on %s: %v", be.Name, err)
			}
			defName := "blobstore-race-def-" + uuid.New().String()
			raceRunID := "blobstore-race-" + uuid.New().String()
			shaRace := make([]byte, 32)
			shaRace[0] = 0x66

			defer func() {
				bg := context.Background()
				plugintest.ExecRebound(t, bg, fixtureDB, dialect, `DELETE FROM workflow_blob_refs WHERE workflow_id = $1`, raceRunID)
				plugintest.ExecRebound(t, bg, fixtureDB, dialect, `DELETE FROM workflow_instances WHERE id = $1`, raceRunID)
				plugintest.ExecRebound(t, bg, fixtureDB, dialect, `DELETE FROM workflow_defs WHERE name = $1`, defName)
				plugintest.ExecRebound(t, bg, fixtureDB, dialect, `DELETE FROM admin.tenants WHERE tenant_id = $1`, tenant.String())
			}()

			if _, err := plugintest.ExecRebound(t, ctx, fixtureDB, dialect,
				`INSERT INTO workflow_defs (name, version, wasm_bytes, tenant_id) VALUES ($1, $2, $3, $4)`,
				defName, 1, []byte{0}, tenant); err != nil {
				t.Fatalf("insert workflow_defs on %s: %v", be.Name, err)
			}

			// Workflow_blob_refs starts EMPTY. sweepStaleWorkflowRefsMSSQL's
			// first read (candidates) sees nothing -- the hook below then
			// creates a workflow that is both a candidate (it writes a ref
			// before the second read) and in-flight (it exists before the
			// second read too), so the pre-fix ordering (in-flight read
			// first) would have missed it entirely and this test could not
			// have told the two orderings apart. The candidate-first fix
			// still has to see it via the SECOND (in-flight) read, which is
			// exactly what this test is checking.
			hookRan := false
			sweepStaleWorkflowRefsMSSQLTestHook = func() {
				hookRan = true
				if _, err := plugintest.ExecRebound(t, ctx, fixtureDB, dialect,
					`INSERT INTO workflow_instances (id, def_name, def_version, tenant_id, status) VALUES ($1, $2, $3, $4, 'running')`,
					raceRunID, defName, 1, tenant); err != nil {
					t.Errorf("hook: insert workflow_instances on %s: %v", be.Name, err)
					return
				}
				if _, err := plugintest.ExecRebound(t, ctx, fixtureDB, dialect,
					`INSERT INTO workflow_blob_refs (workflow_id, sha256) VALUES ($1, $2)`,
					raceRunID, shaRace); err != nil {
					t.Errorf("hook: insert workflow_blob_refs on %s: %v", be.Name, err)
				}
			}
			defer func() { sweepStaleWorkflowRefsMSSQLTestHook = nil }()

			n, err := p.sweepStaleWorkflowRefsMSSQL(baseCtx)
			if err != nil {
				t.Fatalf("sweepStaleWorkflowRefsMSSQL on %s: %v", be.Name, err)
			}
			if !hookRan {
				t.Fatalf("hook never ran on %s -- test did not exercise the interleave window at all", be.Name)
			}
			if n != 0 {
				t.Errorf("sweepStaleWorkflowRefsMSSQL deleted %d rows on %s, want 0 -- "+
					"it must not delete the ref the hook just wrote", n, be.Name)
			}

			var survived int
			if err := plugintest.QueryRowRebound(t, ctx, fixtureDB, dialect,
				`SELECT COUNT(*) FROM workflow_blob_refs WHERE workflow_id = $1`,
				raceRunID).Scan(&survived); err != nil {
				t.Fatalf("count surviving ref on %s: %v", be.Name, err)
			}
			if survived != 1 {
				t.Errorf("on %s, the interleaved workflow's ref count = %d, want 1 -- "+
					"a workflow that started (and wrote its ref) between the candidate "+
					"read and the in-flight read must survive the sweep that raced it "+
					"(cleat#2141)", be.Name, survived)
			}
		})
	}
}

// TestSweepStaleWorkflowRefsMSSQL_RealConcurrencyKeepsANewWorkflowsRef is cleat-review's own
// reproduction (plugins/blobstore/zz_review_race_test.go, run against
// bf55fbba), ported in as the real-concurrency counterpart to the
// deterministic test above: no hook, just a writer goroutine racing the
// sweep for real. REVIEW_TENANTS scales it up (cleat-review used 200 to get
// a 4% loss rate against the pre-fix ordering); the CI default is smaller so
// the test finishes quickly, but still reproduces reliably -- see the
// reliability measurement in this PR's description.
func TestSweepStaleWorkflowRefsMSSQL_RealConcurrencyKeepsANewWorkflowsRef(t *testing.T) {
	nTenants, _ := strconv.Atoi(os.Getenv("REVIEW_TENANTS"))
	if nTenants == 0 {
		nTenants = 8
	}
	raceDuration := 2 * time.Second
	if d, err := time.ParseDuration(os.Getenv("REVIEW_RACE_DURATION")); err == nil && d > 0 {
		raceDuration = d
	}
	// MSSQL-only -- see the identical guard and reasoning on
	// TestSweepStaleWorkflowRefsMSSQL_DeterministicInterleave above.
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server-only real-concurrency test")
	}
	for _, be := range testutil.NewPluginTestBackends(t) {
		if be.Dialect != testutil.DialectMSSQL {
			continue // the race is specific to sweepStaleWorkflowRefsMSSQL, MSSQL-only
		}
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			baseCtx := context.Background()
			ctx := plugin.AcrossAllTenants(baseCtx, "review: sweep as Run does")
			dialect := plugin.Dialect(string(be.Dialect))
			p := &Plugin{dialect: dialect}
			// Schema before CrossTenantConn -- see the sibling test above.
			testutil.SetupMinimalSchema(t, be.DB, be.Dialect)
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("blobstore migrations on %s: %v", be.Name, err)
			}
			fixtureDB := be.CrossTenantConn(t, context.Background(), "review fixture")
			writerDB := be.CrossTenantConn(t, context.Background(), "review writer")
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}
			p.logger = slog.New(slog.NewTextHandler(io.Discard, nil))

			prefix := "rv2141-" + uuid.NewString()[:8] + "-"
			var tenants []uuid.UUID
			for i := 0; i < nTenants; i++ {
				tid := uuid.New()
				tenants = append(tenants, tid)
				if _, err := plugintest.ExecRebound(t, ctx, fixtureDB, dialect,
					`INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)`,
					tid.String(), prefix+tid.String()); err != nil {
					t.Fatalf("seed admin.tenants on %s: %v", be.Name, err)
				}
			}
			defName := prefix + "def"
			if _, err := plugintest.ExecRebound(t, ctx, fixtureDB, dialect,
				`INSERT INTO workflow_defs (name, version, wasm_bytes, tenant_id) VALUES ($1, 1, $2, $3)`,
				defName, []byte{0}, tenants[0]); err != nil {
				t.Fatalf("insert workflow_defs on %s: %v", be.Name, err)
			}
			tA := tenants[0]
			defer func() {
				bg := context.Background()
				plugintest.ExecRebound(t, bg, fixtureDB, dialect, `DELETE FROM workflow_blob_refs WHERE workflow_id LIKE $1`, prefix+"%")
				plugintest.ExecRebound(t, bg, fixtureDB, dialect, `DELETE FROM workflow_instances WHERE id LIKE $1`, prefix+"%")
				plugintest.ExecRebound(t, bg, fixtureDB, dialect, `DELETE FROM workflow_defs WHERE name = $1`, defName)
				plugintest.ExecRebound(t, bg, fixtureDB, dialect, `DELETE FROM admin.tenants WHERE name LIKE $1`, prefix+"%")
			}()

			live := func(id string, tid uuid.UUID) error {
				if _, err := plugintest.ExecRebound(t, ctx, writerDB, dialect,
					`INSERT INTO workflow_instances (id, def_name, def_version, tenant_id, status) VALUES ($1, $2, 1, $3, 'running')`,
					id, defName, tid); err != nil {
					return err
				}
				sha := make([]byte, 32)
				_, _ = rand.Read(sha)
				_, err := plugintest.ExecRebound(t, ctx, writerDB, dialect,
					`INSERT INTO workflow_blob_refs (workflow_id, sha256) VALUES ($1, $2)`, id, sha)
				return err
			}

			stop := make(chan struct{})
			var wg sync.WaitGroup
			var mu sync.Mutex
			var ids []string
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; ; i++ {
					select {
					case <-stop:
						return
					default:
					}
					id := fmt.Sprintf("%srace-%d", prefix, i)
					if err := live(id, tA); err != nil {
						t.Errorf("writer: %v", err)
						return
					}
					mu.Lock()
					ids = append(ids, id)
					mu.Unlock()
				}
			}()
			sweeps := 0
			deadline := time.Now().Add(raceDuration)
			for time.Now().Before(deadline) {
				if _, _, _, err := p.cleanupExpired(ctx, baseCtx); err != nil {
					t.Errorf("sweep: %v", err)
					break
				}
				sweeps++
			}
			close(stop)
			wg.Wait()

			var survived int
			if err := plugintest.QueryRowRebound(t, ctx, fixtureDB, dialect,
				`SELECT COUNT(*) FROM workflow_blob_refs WHERE workflow_id LIKE $1`,
				prefix+"race-%").Scan(&survived); err != nil {
				t.Fatalf("count surviving refs on %s: %v", be.Name, err)
			}
			lost := len(ids) - survived
			t.Logf("%d sweeps over %d tenants; %d running workflows created during sweeps; %d lost their blob ref",
				sweeps, nTenants, len(ids), lost)
			if lost > 0 {
				t.Errorf("on %s, %d of %d in-flight workflows lost their blob ref to a racing sweep (cleat#2141)",
					be.Name, lost, len(ids))
			}
		})
	}
}
