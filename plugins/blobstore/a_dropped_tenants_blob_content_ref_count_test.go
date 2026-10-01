package blobstore

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/plugintest"
)

// TestADroppedTenantsBlobContentRefCountIsCorrected is cleat#2250.
//
// admin.drop_tenant iterates admin.plugin_tables WHERE tenant_scoped and
// hard-deletes matching rows (migrations/postgres/001_schema.sql,
// migrations/mssql/003_procedures.sql). blobstore declares only blob_index
// as TenantScoped (migrations.go) -- blob_content is content-addressed and
// shared across tenants by design, so it is never in that registry and
// admin.drop_tenant's loop never reaches it.
//
// Before this fix, the ONLY place anything decremented blob_content.ref_count
// was deleteChunksReturning (queries.go), called from cleanupExpired
// (background.go) as phase 2 of the TTL sweep. That statement counts
// blob_index rows it is ITSELF deleting (expired or soft-deleted). A
// blob_index row that admin.drop_tenant already hard-deleted was never
// soft-deleted and is gone before phase 2 runs, so it was invisible to that
// join -- not decremented, not even counted as a miss. This is the same
// shape as the MySQL ordering bug the file header of
// blobstore_expiry_decrement_multidb_test.go documents (DELETE before
// UPDATE makes the join empty), except here the "DELETE" is
// admin.drop_tenant and there was no later statement that could see what it
// removed even in principle. Measured directly before the fix: a shared
// blob's ref_count stayed over-counted forever, and a blob unique to the
// dropped tenant never reached ref_count <= 0, so it was never collected.
//
// The fix is phase 2b in cleanupExpired: reconcileRefCounts (queries.go)
// recomputes ref_count from a direct count of blob_index rows, for every
// blob_content row, closing the class of bug (any future path that removes a
// blob_index row outside this package's own SQL) rather than patching
// admin.drop_tenant specifically -- which is core, generic across every
// plugin's tenant-scoped tables, and has no business knowing blobstore's
// ref-counting scheme.
//
// PostgreSQL and SQL Server only for the drop itself -- MySQL has no
// admin.drop_tenant at all (see TestADroppedTenantsWebhookIngestRowsGoWithIt's
// header), so there is no drop path for it to leak through on that dialect.
func TestADroppedTenantsBlobContentRefCountIsCorrected(t *testing.T) {
	var (
		shaShared     = make([]byte, 32) // referenced by victim AND bystander
		shaOnlyVictim = make([]byte, 32) // referenced only by the dropped tenant
		shaBystander  = make([]byte, 32) // control: untouched by the drop
	)
	shaShared[0], shaOnlyVictim[0], shaBystander[0] = 0xAA, 0xBB, 0xCC

	for _, be := range testutil.NewPluginTestBackends(t) {
		if be.Dialect == testutil.DialectMySQL {
			continue
		}
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			baseCtx := context.Background()
			ctx := plugin.AcrossAllTenants(baseCtx,
				"cleat#2250: measuring admin.drop_tenant's effect on blob_content.ref_count")
			dialect := plugin.Dialect(be.Dialect)
			quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			p := &Plugin{dialect: dialect, logger: quiet}
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("blobstore migrations on %s: %v", be.Name, err)
			}
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}

			victim := uuid.New()
			bystander := uuid.New()

			fixtureDB := be.CrossTenantConn(t, baseCtx,
				"cleat#2250: seeding blob_content/blob_index across two tenants around a drop")

			defer func() {
				bg := context.Background()
				for _, tn := range []uuid.UUID{victim, bystander} {
					if _, err := plugintest.ExecRebound(t, bg, fixtureDB, dialect,
						`DELETE FROM blob_index WHERE tenant_id = $1`, tn); err != nil {
						t.Errorf("cleanup blob_index on %s: %v", be.Name, err)
					}
				}
				for _, sha := range [][]byte{shaShared, shaOnlyVictim, shaBystander} {
					if _, err := plugintest.ExecRebound(t, bg, fixtureDB, dialect,
						`DELETE FROM blob_content WHERE sha256 = $1`, sha); err != nil {
						t.Errorf("cleanup blob_content on %s: %v", be.Name, err)
					}
				}
			}()

			for _, tn := range []uuid.UUID{victim, bystander} {
				if _, err := plugintest.ExecRebound(t, baseCtx, be.DB, dialect,
					`INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)`,
					tn, "cleat-2250-"+tn.String()[:8]); err != nil {
					t.Fatalf("seed admin.tenants for %s: %v", tn, err)
				}
			}

			for _, c := range []struct {
				sha []byte
				ref int
			}{{shaShared, 2}, {shaOnlyVictim, 1}, {shaBystander, 1}} {
				if _, err := plugintest.ExecRebound(t, baseCtx, fixtureDB, dialect,
					`INSERT INTO blob_content (sha256, size, data, ref_count, storage_backend)
					 VALUES ($1, 0, $2, $3, 'memory')`,
					c.sha, []byte{}, c.ref); err != nil {
					t.Fatalf("seed blob_content on %s: %v", be.Name, err)
				}
			}

			for _, ix := range []struct {
				tenant uuid.UUID
				key    string
				sha    []byte
			}{
				{victim, "victim-shared", shaShared},
				{victim, "victim-only", shaOnlyVictim},
				{bystander, "bystander-shared", shaShared},
				{bystander, "bystander-only", shaBystander},
			} {
				if _, err := plugintest.ExecRebound(t, baseCtx, fixtureDB, dialect,
					`INSERT INTO blob_index (`+quotedKeyColumn(dialect)+`, tenant_id, sha256, size)
					 VALUES ($1, $2, $3, 0)`,
					ix.key, ix.tenant, ix.sha); err != nil {
					t.Fatalf("seed blob_index %q on %s: %v", ix.key, be.Name, err)
				}
			}

			readRef := func(sha []byte) (int, error) {
				t.Helper()
				var ref int
				err := plugintest.QueryRowRebound(t, baseCtx, fixtureDB, dialect,
					`SELECT ref_count FROM blob_content WHERE sha256 = $1`, sha).Scan(&ref)
				return ref, err
			}
			countIndex := func(tn uuid.UUID) int {
				t.Helper()
				var n int
				if err := plugintest.QueryRowRebound(t, baseCtx, fixtureDB, dialect,
					`SELECT count(*) FROM blob_index WHERE tenant_id = $1`, tn).Scan(&n); err != nil {
					t.Fatalf("count blob_index for %s: %v", tn, err)
				}
				return n
			}

			// Preconditions, checked rather than assumed (cleat#1265).
			if ref, err := readRef(shaShared); err != nil || ref != 2 {
				t.Fatalf("PRECONDITION FAILED: shaShared ref_count=%d err=%v, want 2", ref, err)
			}
			if ref, err := readRef(shaOnlyVictim); err != nil || ref != 1 {
				t.Fatalf("PRECONDITION FAILED: shaOnlyVictim ref_count=%d err=%v, want 1", ref, err)
			}
			if got := countIndex(victim); got != 2 {
				t.Fatalf("PRECONDITION FAILED: victim has %d blob_index rows, want 2", got)
			}
			if got := countIndex(bystander); got != 2 {
				t.Fatalf("PRECONDITION FAILED: bystander has %d blob_index rows, want 2", got)
			}

			// The call under test.
			switch be.Dialect {
			case testutil.DialectMSSQL:
				if _, err := be.DB.ExecContext(baseCtx, `EXEC admin.drop_tenant @tenant_id = @p1`, victim); err != nil {
					t.Fatalf("admin.drop_tenant(victim): %v", err)
				}
			default:
				if _, err := be.DB.ExecContext(baseCtx, `SELECT admin.drop_tenant($1, 'public')`, victim); err != nil {
					t.Fatalf("admin.drop_tenant(victim): %v", err)
				}
			}

			// Prove the drop actually ran to completion, not merely that
			// nothing raised (cleat#1265).
			var tenantRow int
			if err := plugintest.QueryRowRebound(t, baseCtx, be.DB, dialect,
				`SELECT count(*) FROM admin.tenants WHERE tenant_id = $1`,
				victim).Scan(&tenantRow); err != nil {
				t.Fatalf("count admin.tenants: %v", err)
			}
			if tenantRow != 0 {
				t.Fatalf("PRECONDITION FAILED: admin.drop_tenant left the victim's admin.tenants row behind")
			}

			// blob_index is TenantScoped, so this part is uncontroversial --
			// drop_tenant is supposed to remove it, and does.
			if got := countIndex(victim); got != 0 {
				t.Errorf("victim's blob_index rows survived admin.drop_tenant (count=%d)", got)
			}
			if got := countIndex(bystander); got != 2 {
				t.Errorf("dropping the victim changed the bystander's blob_index rows (count=%d, want 2)", got)
			}

			// admin.drop_tenant itself does not touch blob_content -- the fix
			// lives in the GC sweep's reconcile phase, not in drop_tenant, so
			// immediately after the drop both counts are still stale. This is
			// an expected window (closed within one cleanupInterval tick, not
			// claimed to be instantaneous), logged rather than asserted on.
			if ref, err := readRef(shaShared); err == nil {
				t.Logf("on %s, immediately after the drop: shaShared.ref_count=%d (stale until the next sweep)", be.Name, ref)
			}

			// The real GC sweep (not a re-implementation of its SQL) must
			// correct both counts: shaShared down to 1 (only bystander's
			// reference remains) and shaOnlyVictim down to 0 and then
			// collected by phase 3, since nothing holds a live reference to it
			// any more.
			if _, _, _, err := p.cleanupExpired(ctx, baseCtx); err != nil {
				t.Fatalf("cleanupExpired on %s: %v", be.Name, err)
			}

			if ref, err := readRef(shaShared); err != nil {
				t.Errorf("read shaShared ref_count after GC on %s: %v", be.Name, err)
			} else if ref != 1 {
				t.Errorf("on %s, shaShared.ref_count=%d after the GC sweep, want 1 (only bystander's "+
					"reference) -- the survivor's shared content is over-counted by the dropped "+
					"tenant's removed reference (cleat#2250)", be.Name, ref)
			}

			if ref, err := readRef(shaOnlyVictim); err == nil {
				t.Errorf("on %s, shaOnlyVictim still has ref_count=%d after the GC sweep, want it "+
					"collected -- content unique to the dropped tenant can never be reclaimed (cleat#2250)",
					be.Name, ref)
			} else if err != sql.ErrNoRows {
				t.Fatalf("read shaOnlyVictim after GC on %s: %v", be.Name, err)
			}

			// Control: the bystander's own content, touched by nothing above,
			// must be unaffected by either the drop or the GC sweep.
			if ref, err := readRef(shaBystander); err != nil || ref != 1 {
				t.Errorf("shaBystander ref_count=%d err=%v after drop+GC, want 1 (untouched control)", ref, err)
			}
		})
	}
}
