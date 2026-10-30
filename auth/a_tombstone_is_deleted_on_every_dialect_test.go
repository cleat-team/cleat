package auth

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/google/uuid"
)

// cleat#3552, B3b-2b. TenantStore.DeleteTombstone on real databases. The route over HTTP, and the claim that its
// reply equals the row, are in cmd/cleat-worker; this is what the store does that the route cannot show.

func seedTombstone(t *testing.T, db *sql.DB, dialect string, id uuid.UUID, createdBy, seq, hash any) {
	t.Helper()
	ph := func(n int) string {
		if dialect == DialectMSSQL {
			return "@p" + string(rune('0'+n))
		}
		return "$" + string(rune('0'+n))
	}
	if _, err := db.Exec(`INSERT INTO admin.tenant_tombstones (dropped_tenant_id, name, org_id, created_by, dropped_by, audit_chain_seq, audit_chain_hash)
		VALUES (`+ph(1)+`,`+ph(2)+`,`+ph(3)+`,`+ph(4)+`,`+ph(5)+`,`+ph(6)+`,`+ph(7)+`)`,
		id.String(), "tomb-"+id.String(), uuid.NewString(), createdBy, "operator:seed", seq, hash); err != nil {
		t.Fatalf("seed tombstone (%s): %v", dialect, err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM admin.tenant_tombstones WHERE dropped_tenant_id = `+ph(1), id.String())
	})
}

func tombstoneCount(t *testing.T, db *sql.DB, dialect string, id uuid.UUID) int {
	t.Helper()
	q := `SELECT count(*) FROM admin.tenant_tombstones WHERE dropped_tenant_id = $1`
	if dialect == DialectMSSQL {
		q = `SELECT count(*) FROM admin.tenant_tombstones WHERE dropped_tenant_id = @p1`
	}
	var n int
	if err := db.QueryRow(q, id.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestATombstoneIsDeletedOnPostgresAndSQLServer(t *testing.T) {
	for _, dialect := range []string{DialectPostgres, DialectMSSQL} {
		dialect := dialect
		t.Run(dialect, func(t *testing.T) {
			db := testutil.TestDB(t, testutil.Dialect(dialect))
			ts, err := NewTenantStoreForDialect(db, dialect)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()

			// A tombstone with no creator and no chain comes back as exactly that.
			bare := uuid.New()
			seedTombstone(t, db, dialect, bare, nil, nil, nil)
			got, err := ts.DeleteTombstone(ctx, bare)
			if err != nil {
				t.Fatalf("DeleteTombstone: %v", err)
			}
			if got.TenantID != bare || got.CreatedBy != "" || got.AuditChainSeq != nil || got.AuditChainHash != nil || got.DroppedBy != "operator:seed" || got.OrgID == uuid.Nil {
				t.Errorf("a bare tombstone came back as %+v", got)
			}
			if tombstoneCount(t, db, dialect, bare) != 0 {
				t.Errorf("the tombstone is still there")
			}
			// A repeat finds nothing.
			if _, err := ts.DeleteTombstone(ctx, bare); !errors.Is(err, ErrTombstoneNotFound) {
				t.Errorf("a repeat = %v; want ErrTombstoneNotFound", err)
			}

			// The id of a LIVE tenant has no tombstone, and the tenant is left alone.
			live := seedTenant(t, db, dialect, "tomb-live-"+dialect)
			if _, err := ts.DeleteTombstone(ctx, live); !errors.Is(err, ErrTombstoneNotFound) {
				t.Errorf("a live tenant's id = %v; want ErrTombstoneNotFound", err)
			}
			if _, err := ts.DeleteTombstone(ctx, uuid.Nil); !errors.Is(err, ErrTombstoneNotFound) {
				t.Errorf("the nil id = %v; want ErrTombstoneNotFound", err)
			}

			// A chain head comes back whole: the hash is char(64), so a padded read would not equal it.
			full := uuid.New()
			hash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
			seedTombstone(t, db, dialect, full, "clew-service", int64(7), hash)
			got, err = ts.DeleteTombstone(ctx, full)
			if err != nil || got.CreatedBy != "clew-service" || got.AuditChainSeq == nil || *got.AuditChainSeq != 7 || got.AuditChainHash == nil || *got.AuditChainHash != hash {
				t.Errorf("a full tombstone = %+v, %v", got, err)
			}

			// Concurrent deletes of one tombstone: exactly one gets it, and the rest are told there is none.
			// Several rounds, because one racy round caught a milliseconds-wide race only part of the time the last
			// time this repo measured one.
			for round := 0; round < 6; round++ {
				id := uuid.New()
				seedTombstone(t, db, dialect, id, "racer", int64(round), hash)
				const racers = 8
				var wg sync.WaitGroup
				var mu sync.Mutex
				wins, nones, others := 0, 0, 0
				for i := 0; i < racers; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						_, err := ts.DeleteTombstone(ctx, id)
						mu.Lock()
						defer mu.Unlock()
						switch {
						case err == nil:
							wins++
						case errors.Is(err, ErrTombstoneNotFound):
							nones++
						default:
							others++
						}
					}()
				}
				wg.Wait()
				if wins != 1 || nones != racers-1 || others != 0 {
					t.Fatalf("round %d: %d racers gave %d wins, %d not-found, %d other; want 1, %d, 0", round, racers, wins, nones, others, racers-1)
				}
			}
		})
	}
}

// MySQL is single-tenant: there is nothing to drop and nothing to delete, and it says so rather than
// answering "not found", which would read as a tombstone that simply is not there.
func TestATombstoneCannotBeDeletedOnMySQL(t *testing.T) {
	db := testutil.TestDB(t, testutil.Dialect(DialectMySQL))
	ts, err := NewTenantStoreForDialect(db, DialectMySQL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ts.DeleteTombstone(context.Background(), uuid.New())
	if err == nil || errors.Is(err, ErrTombstoneNotFound) {
		t.Errorf("DeleteTombstone on MySQL = %v; want an error that is not ErrTombstoneNotFound", err)
	}
}
