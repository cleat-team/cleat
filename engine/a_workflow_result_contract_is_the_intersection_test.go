package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// The contract for a workflow result is the INTERSECTION of what all three
// backends accept, and cleat does not normalise them to agree. cleat#1025.
//
// This test is the documentation's guard. docs/reference/database-backends.md
// section 7.4 states the limits; every row of both its tables is asserted here,
// so a backend changing behaviour turns that section RED rather than stale. A
// number in prose with no test under it is the failure mode CLAUDE.md records
// over and over.
//
// WHY THE LIMITS DIVERGE AT ALL: the column is a different type per backend.
//
//	PostgreSQL   JSONB                                    validates AND normalises
//	MySQL        LONGTEXT + CHECK (JSON_VALID(..))        validates, depth-limited, keeps the bytes
//	SQL Server   NVARCHAR(MAX) + CHECK (ISJSON(..)=1)     validates, keeps the bytes
//
// MySQL's row changed in cleat#1022 (migrations/mysql/070). It was `JSON`, which
// silently rewrote any integer outside [-2^63, 2^64-1] and any decimal past
// float64's precision. JSON_VALID still enforces the depth limit -- measured,
// with the constraint installed, invalid JSON refused and valid JSON accepted as
// controls -- so the only rows of section 7.4 that moved are key order,
// duplicate keys and integers beyond 2^64-1.
//
// Nothing upstream catches a violation: coerceResultJSON checks json.Valid and
// object shape and REPORTS rather than rejects, and every payload below is
// valid JSON and an object. The rejection lands in FinalizeWorkflowSegment,
// after the workflow body and its side effects have already run -- the work is
// done and the record says failed.
//
// NO BACKEND IS THE STRICT ONE, which is the whole reason the contract is an
// intersection rather than one backend's rules: a NUL escape passes on MySQL
// and SQL Server and fails on PostgreSQL; depth 120 passes on PostgreSQL and
// SQL Server and fails on MySQL.
func TestAWorkflowResultContractIsTheIntersection(t *testing.T) {
	nest := func(n int) string {
		return strings.Repeat(`{"a":`, n) + "1" + strings.Repeat("}", n)
	}

	// The payloads that must be written as ESCAPES rather than typed. Both are
	// four- and six-character sequences inside a JSON string, not the code
	// points themselves: a raw NUL byte cannot survive a shell, a Go source
	// file or this repository's own JSONB columns, and writing one by accident
	// is how the first attempt at this measurement was lost.
	nulEscape := `{"s":"a\u0000b"}`
	loneSurrogate := `{"s":"\ud800"}`

	// What each backend does with each payload. The intersection is DERIVED
	// from this table below rather than restated, so the two cannot drift.
	type expect struct{ pg, my, ms bool }
	cases := []struct {
		label   string
		payload string
		exp     expect
		why     string
	}{
		{"control", `{"ok":true}`, expect{true, true, true},
			"if this is refused anywhere, nothing else in this test means anything"},
		{"NUL escape", nulEscape, expect{false, true, true},
			"PostgreSQL cannot store a NUL in text; the other two keep the escape"},
		{"lone high surrogate", loneSurrogate, expect{false, false, true},
			"two of three reject an unpaired surrogate; SQL Server's ISJSON accepts it"},
		{"nesting 100", nest(100), expect{true, true, true},
			"MySQL's maximum depth, and the tightest of the three -- the contract's bound"},
		{"nesting 101", nest(101), expect{true, false, true},
			"one past MySQL's limit: this is the boundary the contract is set by"},
		{"nesting 128", nest(128), expect{true, false, true},
			"SQL Server's maximum; MySQL already refused 27 levels earlier"},
		{"nesting 129", nest(129), expect{true, false, false},
			"one past SQL Server's limit, so only PostgreSQL still accepts it"},
	}

	for _, d := range []struct {
		name    string
		dialect testutil.Dialect
		setup   func(*testing.T, *sql.DB)
		want    func(expect) bool
	}{
		{"postgres", testutil.DialectPostgres, func(t *testing.T, db *sql.DB) {
			testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
		}, func(e expect) bool { return e.pg }},
		{"mysql", testutil.DialectMySQL, testutil.SetupMySQLFullSchema,
			func(e expect) bool { return e.my }},
		{"mssql", testutil.DialectMSSQL, testutil.SetupMSSQLFullSchema,
			func(e expect) bool { return e.ms }},
	} {
		t.Run(d.name, func(t *testing.T) {
			db := testutil.TestDB(t, d.dialect)
			defer db.Close()
			d.setup(t, db)

			wfID := "result-contract-" + d.name
			seedWorkflowInstance(t, db, d.dialect, wfID)
			admin := testutil.AdminDB(t, db, d.dialect)

			set := map[testutil.Dialect]string{
				testutil.DialectPostgres: `UPDATE workflow_instances SET result = $1 WHERE id = $2`,
				testutil.DialectMySQL:    "UPDATE workflow_instances SET result = ? WHERE id = ?",
				testutil.DialectMSSQL:    `UPDATE workflow_instances SET result = @p1 WHERE id = @p2`,
			}[d.dialect]
			read := map[testutil.Dialect]string{
				testutil.DialectPostgres: `SELECT result::text FROM workflow_instances WHERE id = $1`,
				testutil.DialectMySQL:    "SELECT CAST(result AS CHAR) FROM workflow_instances WHERE id = ?",
				testutil.DialectMSSQL:    `SELECT CAST(result AS NVARCHAR(MAX)) FROM workflow_instances WHERE id = @p1`,
			}[d.dialect]
			countQ := map[testutil.Dialect]string{
				testutil.DialectPostgres: `SELECT count(*) FROM workflow_instances WHERE id = $1`,
				testutil.DialectMySQL:    "SELECT count(*) FROM workflow_instances WHERE id = ?",
				testutil.DialectMSSQL:    `SELECT count(*) FROM workflow_instances WHERE id = @p1`,
			}[d.dialect]

			// PRECONDITION, and it is not ceremony. On SQL Server the tenant
			// filter is a FILTER PREDICATE: a row this connection cannot see
			// makes the UPDATE match nothing and REPORT SUCCESS, so every case
			// below would read as "accepted" while writing nothing at all.
			// Taking this measurement by hand, that is exactly what happened --
			// a full three-dialect table where the SQL Server column was
			// entirely unmeasured and nothing errored.
			var visible int
			if err := admin.QueryRow(countQ, wfID).Scan(&visible); err != nil {
				t.Fatalf("counting the seeded run on %s: %v", d.dialect, err)
			}
			if visible != 1 {
				t.Fatalf("PRECONDITION FAILED on %s: the seeded run is not visible to this "+
					"connection (%d rows). An UPDATE matching zero rows returns no error, so "+
					"every case below would report success without writing anything.",
					d.dialect, visible)
			}

			verb := map[bool]string{true: "ACCEPTED", false: "REJECTED"}
			for _, c := range cases {
				_, err := admin.Exec(set, c.payload, wfID)
				got := err == nil
				if want := d.want(c.exp); got != want {
					t.Errorf("%s: %s on %s, want %s.\n  why this case exists: %s\n  error: %v\n\n"+
						"docs/reference/database-backends.md section 7.4 states this backend's "+
						"answer. If the backend has changed, that section is now wrong and must "+
						"change with this test: the contract is the INTERSECTION of the three "+
						"columns, so one of them moving can move the contract.",
						c.label, verb[got], d.dialect, verb[want], c.why, err)
				}
			}

			// ACCEPTANCE IS NOT THE WHOLE CONTRACT. Two of three normalise, so a
			// result accepted everywhere can still read back differently. The
			// documented rule -- never depend on key order, never send duplicate
			// keys -- is only true because this half is checked.
			normalises := map[testutil.Dialect]bool{
				testutil.DialectPostgres: true,
				// MySQL moved to false in cleat#1022. Its result column is no
				// longer a JSON column -- migrations/mysql/070 makes it
				// LONGTEXT with a JSON_VALID check, which is what SQL Server
				// has always had -- so it no longer reorders keys or collapses
				// duplicates. The CONTRACT is unchanged: PostgreSQL still
				// normalises, so callers still must not depend on either.
				// What changed is how many backends enforce that by accident.
				testutil.DialectMySQL: false,
				testutil.DialectMSSQL: false,
			}[d.dialect]

			for _, n := range []struct{ label, payload string }{
				{"key order", `{"b":1,"a":2}`},
				{"duplicate keys", `{"a":1,"a":2}`},
			} {
				if _, err := admin.Exec(set, n.payload, wfID); err != nil {
					t.Fatalf("%s on %s: %v", n.label, d.dialect, err)
				}
				var back string
				if err := admin.QueryRow(read, wfID).Scan(&back); err != nil {
					t.Fatalf("reading %s back on %s: %v", n.label, d.dialect, err)
				}
				identical := back == n.payload
				if normalises && identical {
					t.Errorf("%s: %s stored %q byte-identical, but this backend normalises.\n\n"+
						"If it has stopped, section 7.4's second table is wrong -- callers were "+
						"told not to depend on key order BECAUSE two of three reorder.",
						d.dialect, n.label, back)
				}
				if !normalises && !identical {
					t.Errorf("%s: %s stored %q, want the bytes as given (%q).\n\n"+
						"SQL Server is the one backend that preserves them, which is precisely "+
						"why the contract forbids depending on it.", d.dialect, n.label, back, n.payload)
				}
			}

			// Integers are exact on every backend, and 2^64 is kept here as a
			// REGRESSION ROW rather than a divergence.
			//
			// It used to degrade on MySQL, whose JSON column held an integer as
			// INT64 or UINT64 and fell back to DOUBLE beyond both (cleat#1022).
			// migrations/mysql/070 and 071 removed that -- the column is
			// LONGTEXT now and finalize_workflow_status no longer casts. The
			// bound was NOT signed BIGINT: 2^63 is past it and was always kept,
			// so a limit written to the signed bound would have been wrong by a
			// factor of two on the positive side and would have sat nowhere
			// near the real edge.
			for _, n := range []struct {
				label, payload, want string
				exact                bool
			}{
				{"2^63", `{"n":9223372036854775808}`, "9223372036854775808", true},
				{"2^64-1", `{"n":18446744073709551615}`, "18446744073709551615", true},
				{"2^64", `{"n":18446744073709551616}`, "18446744073709551616", true},
			} {
				if _, err := admin.Exec(set, n.payload, wfID); err != nil {
					t.Fatalf("integer %s on %s: %v", n.label, d.dialect, err)
				}
				var back string
				if err := admin.QueryRow(read, wfID).Scan(&back); err != nil {
					t.Fatalf("reading integer %s back on %s: %v", n.label, d.dialect, err)
				}
				kept := strings.Contains(back, n.want)
				if kept != n.exact {
					keptVerb := map[bool]string{true: "kept exactly", false: "degraded"}
					t.Errorf("integer %s on %s was %s (%q), want %s.\n\n"+
						"Section 7.4 says integers are exact on every backend since "+
						"cleat#1022. A MySQL failure here means migrations/mysql/070 or 071 "+
						"was reverted, or a write path reintroduced CAST(... AS JSON), which "+
						"re-degrades the value before it reaches even a LONGTEXT column. "+
						"If this changed deliberately, the doc changes too.",
						n.label, d.dialect, keptVerb[kept], back, keptVerb[n.exact])
				}
			}
		})
	}
}
