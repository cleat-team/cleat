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
// ALL THREE DIALECTS HAVE A CASE, and the first version of this file said
// PostgreSQL could not have one. That was wrong twice over, and both halves are
// worth keeping because the second is the interesting one:
//
//   - The obstacle was not real. scratchPostgresDB (catalogdiff_test.go:75)
//     exists and is already used to build a PAIR -- two calls, in
//     TestSnapshotIsIdenticalForTwoBuildsOfTheSameChain. Only a DDL-pair helper
//     was absent, which is ~30 lines, not a larger change than this one.
//   - The justification was false. This header claimed the existing Postgres
//     tests cover the read because "their columns do carry a collation (C on
//     this chain)". They do not: `grep -ri collate migrations/postgres/`
//     returns NOTHING, and Postgres reports collation_name as EMPTY for a
//     column with no explicit COLLATE (measured: `s text COLLATE "C"` -> `C`,
//     `s text` -> `''`). So the chain's columns report the empty string, and
//     the pre-existing tests exercise the EMPTY PATH ONLY -- on PostgreSQL, a
//     snapshot that read the wrong collation *consistently* would have passed
//     every test in this package. The case below is what pins the non-empty
//     path.
//
// Both were found by cleat-review on PR #3121, which also wrote the case.

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/migration"
)

// PostgreSQL, and this is the case that matters most of the three: the chain
// sets no collation at all, so without it the non-empty path has no control on
// this dialect and a consistently-wrong read would be invisible.
func TestAColumnCollationChangeIsReportedLivePostgres(t *testing.T) {
	a := scratchPostgresDB(t)
	b := scratchPostgresDB(t)
	ctx := context.Background()

	if _, err := a.ExecContext(ctx, `CREATE TABLE t (s text COLLATE "C" NOT NULL)`); err != nil {
		t.Fatalf("build A: %v", err)
	}
	if _, err := b.ExecContext(ctx, `CREATE TABLE t (s text NOT NULL)`); err != nil {
		t.Fatalf("build B: %v", err)
	}

	// The difference is established from the catalogue directly, so the
	// snapshots below are not both the evidence and the subject.
	const q = `SELECT COALESCE(collation_name, '') FROM information_schema.columns
	           WHERE table_name = 't' AND column_name = 's'`
	var ca, cb string
	if err := a.QueryRowContext(ctx, q).Scan(&ca); err != nil {
		t.Fatalf("read A's collation: %v", err)
	}
	if err := b.QueryRowContext(ctx, q).Scan(&cb); err != nil {
		t.Fatalf("read B's collation: %v", err)
	}
	if ca == cb {
		t.Fatalf("both databases report %q, so a zero diff would prove nothing", ca)
	}
	t.Logf("established independently: A=%q B=%q", ca, cb)

	catA, err := Snapshot(ctx, a, migration.DialectPostgres)
	if err != nil {
		t.Fatalf("snapshot A: %v", err)
	}
	catB, err := Snapshot(ctx, b, migration.DialectPostgres)
	if err != nil {
		t.Fatalf("snapshot B: %v", err)
	}
	d := Diff(catA, catB)
	if len(d) == 0 {
		t.Fatalf("Diff reported ZERO differences over a pair that demonstrably differs in " +
			"column collation.\n\nThe column is selected but does not reach the rendered " +
			"line, so the comparator is still blind on this dialect.")
	}
	joined := strings.Join(d, "\n")
	if !strings.Contains(joined, "collation=C") {
		t.Fatalf("Diff reported differences but none names the collation:\n%s", joined)
	}
	t.Logf("Diff reported %d line(s), including %q", len(d), "collation=C")
}

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
