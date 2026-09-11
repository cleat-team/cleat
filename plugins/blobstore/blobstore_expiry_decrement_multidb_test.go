// Phase 2 of the TTL sweep, against real databases, asserting the DECREMENT
// rather than the absence of an error.
//
// cleat#1148 / cleat#1142. Until this test, phase 2 decremented ref_count on
// PostgreSQL only, for two different reasons and with two different symptoms:
//
//   - SQL Server: the MSSQL arm put a DELETE inside a CTE. T-SQL requires a
//     WITH body to be a SELECT, so the statement never parsed. cleanupExpired
//     returns on the first error, which also made phase 3 unreachable -- so
//     orphaned blob_content was never collected either. One log line an hour.
//
//   - MySQL: the arm was valid, and background.go ran the DELETE before it. The
//     UPDATE counts the index rows matching the expiry predicate, and the DELETE
//     had just removed them, so the join was empty. No error, no log line, zero
//     reported, and ref_count never reaching 0 means phase 3 finds nothing to
//     collect. SILENT, which is worse than the SQL Server case.
//
// The existing arm test (blobstore_dialect_arms_multidb_test.go) could not have
// caught either. It executes a statement and asks whether a server accepts it:
// the MySQL statement is accepted and the defect is the ORDER of two statements,
// which no single-statement check can see.
package blobstore

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

func TestExpiredIndexEntriesDecrementRefCount_MultiBackend(t *testing.T) {
	// Three contents. The first keeps a live reference, the second loses its
	// only one, the third has nothing expiring. Distinct counts, so an
	// off-by-one or a decrement applied to the wrong row cannot pass.
	var (
		shaLive    = make([]byte, 32) // ref_count 3 -> 1, two of three refs expired
		shaOrphan  = make([]byte, 32) // ref_count 1 -> 0, sole ref soft-deleted
		shaUntouch = make([]byte, 32) // ref_count 2 -> 2, nothing expired
	)
	shaLive[0], shaOrphan[0], shaUntouch[0] = 0x11, 0x22, 0x33

	for _, be := range testutil.NewPluginTestBackends(t) {
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			ctx := context.Background()
			dialect := plugin.Dialect(be.Dialect)
			p := &Plugin{dialect: dialect}

			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("blobstore migrations on %s: %v", be.Name, err)
			}
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}
			p.logger = slog.New(slog.NewTextHandler(io.Discard, nil))

			tenant := uuid.New()
			// A DEFER, NOT t.Cleanup: `defer be.Cleanup()` above closes the
			// pool when this function returns, and t.Cleanup runs after that --
			// so these deletes would execute against a closed database, fail,
			// and be discarded. The rows then survive into the next run and the
			// seed fails on a duplicate key, which is how the first
			// falsification of this test went red on all three dialects for a
			// reason that had nothing to do with the mutation.
			defer func() {
				bg := context.Background()
				if _, err := be.DB.ExecContext(bg, plugin.Rebind(
					`DELETE FROM blob_index WHERE tenant_id = $1`, dialect), tenant); err != nil {
					t.Errorf("cleanup blob_index on %s: %v", be.Name, err)
				}
				for _, sha := range [][]byte{shaLive, shaOrphan, shaUntouch} {
					if _, err := be.DB.ExecContext(bg, plugin.Rebind(
						`DELETE FROM blob_content WHERE sha256 = $1`, dialect), sha); err != nil {
						t.Errorf("cleanup blob_content on %s: %v", be.Name, err)
					}
				}
			}()

			for _, c := range []struct {
				sha []byte
				ref int
			}{{shaLive, 3}, {shaOrphan, 1}, {shaUntouch, 2}} {
				if _, err := be.DB.ExecContext(ctx, plugin.Rebind(
					`INSERT INTO blob_content (sha256, size, data, ref_count, storage_backend)
					 VALUES ($1, 0, $2, $3, 'memory')`, dialect),
					c.sha, []byte{}, c.ref); err != nil {
					t.Fatalf("seed blob_content on %s: %v", be.Name, err)
				}
			}

			past := time.Now().UTC().Add(-time.Hour)
			for _, ix := range []struct {
				key       string
				sha       []byte
				expiresAt any
				deletedAt any
			}{
				{"live-expired-a", shaLive, past, nil},
				{"live-expired-b", shaLive, past, nil},
				{"live-current", shaLive, nil, nil},
				{"orphan-softdeleted", shaOrphan, nil, past},
				{"untouched-a", shaUntouch, nil, nil},
				{"untouched-b", shaUntouch, nil, nil},
			} {
				if _, err := be.DB.ExecContext(ctx, plugin.Rebind(
					`INSERT INTO blob_index (`+quotedKeyColumn(dialect)+`, tenant_id, sha256, size, expires_at, deleted_at)
					 VALUES ($1, $2, $3, 0, $4, $5)`, dialect),
					ix.key, tenant, ix.sha, ix.expiresAt, ix.deletedAt); err != nil {
					t.Fatalf("seed blob_index %q on %s: %v", ix.key, be.Name, err)
				}
			}

			if _, _, _, err := p.cleanupExpired(ctx); err != nil {
				t.Fatalf("cleanupExpired on %s: %v", be.Name, err)
			}

			// ASSERT THE EFFECT. A statement that never ran and a sweep with
			// nothing to do produce the same return value, which is how this
			// survived on two dialects.
			for _, want := range []struct {
				name string
				sha  []byte
				ref  int
				gone bool
			}{
				{"a content with one live reference left", shaLive, 1, false},
				{"a content whose only reference was soft-deleted", shaOrphan, 0, true},
				{"a content with nothing expiring", shaUntouch, 2, false},
			} {
				var ref int
				err := be.DB.QueryRowContext(ctx, plugin.Rebind(
					`SELECT ref_count FROM blob_content WHERE sha256 = $1`, dialect), want.sha).Scan(&ref)
				switch {
				case want.gone && err == nil:
					t.Errorf("on %s, %s still has ref_count %d.\n\n"+
						"Phase 3 collects blob_content with ref_count <= 0. It found nothing "+
						"because phase 2 never decremented -- the two phases fail together, "+
						"so unbounded storage growth is the symptom of either (cleat#1148).",
						be.Name, want.name, ref)
				case want.gone && err == sql.ErrNoRows:
					// Collected, which is the point of decrementing at all.
				case want.gone:
					t.Fatalf("read back on %s: %v", be.Name, err)
				case err != nil:
					t.Fatalf("read back %s on %s: %v", want.name, be.Name, err)
				case ref != want.ref:
					t.Errorf("on %s, %s has ref_count %d, want %d.\n\n"+
						"Phase 2 must subtract one per expired or soft-deleted index row. "+
						"An unchanged count means the decrement did not run at all (cleat#1148).",
						be.Name, want.name, ref, want.ref)
				}
			}

			var remaining int
			if err := be.DB.QueryRowContext(ctx, plugin.Rebind(
				`SELECT COUNT(*) FROM blob_index WHERE tenant_id = $1`, dialect), tenant).Scan(&remaining); err != nil {
				t.Fatalf("count blob_index on %s: %v", be.Name, err)
			}
			if remaining != 3 {
				t.Errorf("on %s, %d index rows remain, want 3 (the two never-expiring and the one still current)",
					be.Name, remaining)
			}
		})
	}
}

// quotedKeyColumn quotes blob_index's `key` column, which is reserved in MySQL
// and T-SQL. The plugin's own statements quote it the same way.
func quotedKeyColumn(d plugin.Dialect) string {
	switch d {
	case plugin.DialectMySQL:
		return "`key`"
	case plugin.DialectMSSQL:
		return "[key]"
	default:
		return "key"
	}
}
