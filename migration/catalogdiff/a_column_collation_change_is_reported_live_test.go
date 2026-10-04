package catalogdiff

// Live controls for the collation introduced in cleat#2882, on the two
// dialects whose scratch-database helpers this package already has.
//
// WHY THIS EXISTS BESIDE THE IN-MEMORY CASE, because they fail differently and
// only one of them would have caught the defect this change could have had.
// TestACollationChangeIsReported builds two Catalogs by hand and proves the
// comparison SEES a collation that is already on the struct -- it is the
// known-positive for the rendered token. This one proves the SNAPSHOT puts it
// there in the first place: a field the three dialect queries never populate
// would be the empty string on every real snapshot, and the in-memory case
// would still pass, because it never asks a database anything. The two are the
// "does the check look" and "is there anything to look at" halves.
//
// It follows this package's own convention for a snapshot-column control
// (the_mysql_snapshot_sees_the_attributes_it_selects_test.go's header): build a
// pair of databases differing in exactly one attribute, ESTABLISH the
// difference from the catalogue independently of the snapshot, and require Diff
// to report it -- so the snapshots are not both the evidence and the subject.
//
// PostgreSQL has no case here, and that is a stated gap rather than an implied
// one: this package builds its Postgres scratch databases through the migration
// chain (catalogdiff_test.go) rather than through a DDL helper of the shape the
// other two use. The Postgres collation read is exercised by the existing
// snapshot tests running the modified query, and their columns do carry a
// collation (`C` on this chain). A Postgres case of this shape would need the
// chain builder, which is a larger change than this one.

import (
	"context"
	"database/sql"
	"testing"
)

func TestAColumnCollationChangeIsReportedLiveMySQL(t *testing.T) {
	mysqlDiffCase(t, "column-collation",
		`CREATE TABLE t (s VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL)`,
		`CREATE TABLE t (s VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci NOT NULL)`,
		"collation=utf8mb4_bin",
		func(t *testing.T, a, b *sql.DB) string {
			const q = `SELECT collation_name FROM information_schema.columns
			           WHERE table_schema = DATABASE() AND table_name = 't' AND column_name = 's'`
			var ca, cb string
			if err := a.QueryRow(q).Scan(&ca); err != nil {
				t.Fatalf("read A's collation: %v", err)
			}
			if err := b.QueryRow(q).Scan(&cb); err != nil {
				t.Fatalf("read B's collation: %v", err)
			}
			if ca == cb {
				t.Fatalf("both databases report collation %q, so a zero diff would prove nothing", ca)
			}
			return "A=" + ca + " B=" + cb
		})
}

func TestAColumnCollationChangeIsReportedLiveMSSQL(t *testing.T) {
	mssqlDiffCase(t, "column-collation",
		`CREATE TABLE t (s NVARCHAR(64) COLLATE Latin1_General_BIN NOT NULL)`,
		`CREATE TABLE t (s NVARCHAR(64) COLLATE Latin1_General_CI_AS NOT NULL)`,
		"collation=Latin1_General_BIN",
		func(t *testing.T, a, b *sql.DB) string {
			const q = `SELECT c.collation_name
			           FROM sys.columns c JOIN sys.tables tb ON tb.object_id = c.object_id
			           WHERE tb.name = 't' AND c.name = 's'`
			ctx := context.Background()
			var ca, cb string
			if err := a.QueryRowContext(ctx, q).Scan(&ca); err != nil {
				t.Fatalf("read A's collation: %v", err)
			}
			if err := b.QueryRowContext(ctx, q).Scan(&cb); err != nil {
				t.Fatalf("read B's collation: %v", err)
			}
			if ca == cb {
				t.Fatalf("both databases report collation %q, so a zero diff would prove nothing", ca)
			}
			return "A=" + ca + " B=" + cb
		})
}
