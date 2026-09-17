package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// A workflow definition name is a caller-supplied string compared with `=`, and
// `=` on a text column is COLLATION-dependent. No migration in this repo names a
// collation, so every such column inherits the server default -- a property of
// whichever image the operator runs, not of anything cleat declares:
//
//	git ls-files 'migrations/*' | xargs grep -lniE 'COLLATE |CHARACTER SET'   # nothing
//
// The defaults do not agree, and the majority is not the one you would guess.
// Measured 2026-09-16 against the shipped migrations on all three tier-1
// dialects, on the images CI pins (.github/workflows/*.yml):
//
//	                     collation of workflow_defs.name    'placeorder' = 'PlaceOrder'
//	PostgreSQL 16        en_US.utf8, deterministic `=`      false
//	MySQL 8.4.11         utf8mb4_0900_ai_ci                 TRUE
//	SQL Server 2022      SQL_Latin1_General_CP1_CI_AS       TRUE
//
// So `WHERE name = ?` is an exact lookup on ONE dialect of three, and registering
// both `PlaceOrder` and `placeorder` succeeds on PostgreSQL while both other
// dialects reject the second as a duplicate primary key (MySQL 1062, SQL Server
// 2627). Same input, same tier-1 matrix, three-way divergence in whether a
// deployment is even possible.
//
// THIS TEST FIXES NOTHING AND IS NOT A REGRESSION TEST. Choosing a collation is a
// decision with a migration behind it -- IMPROVEMENT-PLAN.md 3.316 holds the
// options and the argument -- and it is deliberately not taken here. What this
// pins is the CURRENT behaviour, per dialect, with a reason recorded for each, so
// that the divergence is executable rather than latent: today nothing in the tree
// compares two strings differing only in case, which is why this has stayed
// invisible. No test anywhere reads a server or column collation:
//
//	git ls-files '*_test.go' | xargs grep -lniE 'collation_name|@@collation|datcollate'
//
// If a future migration pins a collation, this test goes red and names the
// dialect that moved. That is the intended outcome, not a failure of this test.
//
// WHY cleat#936's REMEDY DOES NOT TRANSFER. #936 was the same mechanism reaching
// `parent_close_policy`, and it was closed by validating at the write boundary
// (engine/parent_close_policy.go) rather than by a COLLATE clause -- correctly,
// because that column holds one of THREE known literals, so rejecting anything
// else makes the collation question unreachable. A workflow name is an open set.
// There is no enum to validate against and `[a-zA-Z0-9._-]+` admits both
// spellings, so the write-boundary move is structurally unavailable here.
//
// That issue's own sweep concluded "the class has exactly one member", and this
// is the correction: its population was columns compared against string
// LITERALS, which is blind by construction to a column compared against a BOUND
// PARAMETER, and to a column whose exposure is a UNIQUE INDEX rather than a
// predicate. Derived from the database rather than by reading DDL, 39 unique-index
// columns carry a case-insensitive collation on MySQL and exactly two are immune
// -- `concurrency_keys.key_hash` and `idempotency_keys.key_hash`, both VARBINARY,
// because a binary type has no collation. Re-derive:
//
//	SELECT s.table_name, s.column_name, IFNULL(c.collation_name,'(binary - immune)')
//	FROM information_schema.statistics s JOIN information_schema.columns c
//	  ON c.table_schema=s.table_schema AND c.table_name=s.table_name
//	     AND c.column_name=s.column_name
//	WHERE s.table_schema=DATABASE() AND s.non_unique=0
//	  AND c.data_type IN ('varchar','char','text','varbinary','binary');
//
// Note that hashing is what makes those two immune, and that the project has two
// idempotency mechanisms with OPPOSITE exposure: `idempotency_keys.key_hash` is
// hashed and safe, while `workflow_schedules.idempotency_key` is a VARCHAR in a
// unique index and folds case on two dialects of three.
//
// Not every column here is a defect. `tenant_domains.hostname` and
// `tenant_egress_allow.host` hold hostnames, and DNS is case-insensitive, so
// folding is arguably CORRECT for those two -- which is the reason 3.316 calls
// for a decision per column class rather than one blanket COLLATE.

// caseFoldingDialect records, per dialect, what `=` does to two spellings of one
// name -- and WHY, because a bare true/false invites the next reader to "fix" the
// odd one out without knowing which way the majority runs.
type caseFoldingDialect struct {
	name    string
	dialect testutil.Dialect
	setup   func(*testing.T, *sql.DB)

	// foldsCase is whether `WHERE name = 'placeorder'` finds a row stored as
	// 'PlaceOrder'.
	foldsCase bool
	why       string
}

func caseFoldingDialects() []caseFoldingDialect {
	return []caseFoldingDialect{
		{
			name:    "postgres",
			dialect: testutil.DialectPostgres,
			setup: func(t *testing.T, db *sql.DB) {
				testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
			},
			foldsCase: false,
			why: "PostgreSQL compares text with a DETERMINISTIC collation, so `=` is " +
				"byte equality whatever lc_collate says. This is the only one of the " +
				"three that is exact, and it is exact robustly rather than by " +
				"configuration -- it would take a non-deterministic ICU collation, " +
				"which no migration creates, to change it.",
		},
		{
			name:      "mysql",
			dialect:   testutil.DialectMySQL,
			setup:     testutil.SetupMySQLFullSchema,
			foldsCase: true,
			why: "MySQL 8's server default is utf8mb4_0900_ai_ci -- accent-insensitive " +
				"and CASE-insensitive. Nothing opted into it; no migration names a " +
				"collation, so every VARCHAR inherits it. This is the collation " +
				"cleat#936 measured on parent_close_policy.",
		},
		{
			name:      "mssql",
			dialect:   testutil.DialectMSSQL,
			setup:     testutil.SetupMSSQLFullSchema,
			foldsCase: true,
			why: "SQL Server's container default is SQL_Latin1_General_CP1_CI_AS -- the " +
				"CI is case-insensitive. cleat#936 EXPECTED this and said so, " +
				"explicitly flagging it as unmeasured; IMPROVEMENT-PLAN 3.316 then " +
				"asserted the opposite, that SQL Server is case-sensitive. Measured " +
				"2026-09-16: #936's expectation was right and 3.316 was wrong, which " +
				"moves SQL Server from the minority column to the majority one.",
		},
	}
}

// TestAMiscasedNameMatchesOnTwoOfThreeDialects measures what `WHERE name = ?`
// does to a mis-cased workflow definition name, on a real database built from the
// shipped migrations.
//
// THE TWO PRECONDITIONS ARE THE POINT OF THIS TEST, not ceremony around it. The
// measurement is "a lookup returned no row", and there are at least three
// uninteresting ways to get that answer:
//
//   - the row was never inserted;
//   - the row is INVISIBLE to this connection, which on SQL Server is the
//     ordinary case -- workflow_defs carries a tenant FILTER predicate, so a
//     connection with no SESSION_CONTEXT sees nothing and the count is 0 with no
//     error at all;
//   - the predicate matches nothing for a reason unrelated to collation.
//
// Every one of those reads as "case-sensitive". Taking this measurement by hand
// on 2026-09-16 that is exactly what happened: the SQL Server column came back
// 0/0, which looks like a clean case-sensitive result and was in fact a row this
// connection could not see -- while the mis-cased INSERT was simultaneously
// rejected with `Violation of PRIMARY KEY ... duplicate key value is
// (00000000-..., placeorder, 1)`, proving the server HAD folded the case. The
// exact-match precondition is the only thing that separated those, so it is a
// Fatal with the word UNMEASURED in it rather than an assertion.
func TestAMiscasedNameMatchesOnTwoOfThreeDialects(t *testing.T) {
	const tenant = "00000000-0000-0000-0000-000000000000"

	for _, d := range caseFoldingDialects() {
		t.Run(d.name, func(t *testing.T) {
			db := testutil.TestDB(t, d.dialect)
			defer db.Close()
			d.setup(t, db)
			admin := testutil.AdminDB(t, db, d.dialect)

			// Mixed case in the stored spelling, and the probe asks for the
			// all-lower one. Both are legal workflow names under the
			// [a-zA-Z0-9._-]+ rule that readServiceName enforces, which is
			// the reason validation cannot close this.
			const stored = "CaseFoldProbe.PlaceOrder"
			miscased := strings.ToLower(stored)
			if miscased == stored {
				t.Fatalf("the probe name %q has no case to fold, so this test "+
					"cannot measure anything", stored)
			}

			ins := map[testutil.Dialect]string{
				testutil.DialectPostgres: `INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, tenant_id) VALUES ($1, 1, $2, 1, 0, $3)`,
				testutil.DialectMySQL:    `INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, tenant_id) VALUES (?, 1, ?, 1, 0, ?)`,
				testutil.DialectMSSQL:    `INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, tenant_id) VALUES (@p1, 1, @p2, 1, 0, @p3)`,
			}[d.dialect]
			countQ := map[testutil.Dialect]string{
				testutil.DialectPostgres: `SELECT count(*) FROM workflow_defs WHERE tenant_id = $1 AND name = $2`,
				testutil.DialectMySQL:    `SELECT count(*) FROM workflow_defs WHERE tenant_id = ? AND name = ?`,
				testutil.DialectMSSQL:    `SELECT count(*) FROM workflow_defs WHERE tenant_id = @p1 AND name = @p2`,
			}[d.dialect]
			del := map[testutil.Dialect]string{
				testutil.DialectPostgres: `DELETE FROM workflow_defs WHERE tenant_id = $1 AND name = $2`,
				testutil.DialectMySQL:    `DELETE FROM workflow_defs WHERE tenant_id = ? AND name = ?`,
				testutil.DialectMSSQL:    `DELETE FROM workflow_defs WHERE tenant_id = @p1 AND name = @p2`,
			}[d.dialect]

			// Both spellings, because on PostgreSQL they are two rows and on
			// the other two the second insert never lands -- so a cleanup
			// keyed on one spelling leaves the other behind, and on MySQL and
			// SQL Server it would not even be the spelling that exists.
			cleanup := func() {
				for _, n := range []string{stored, miscased} {
					_, _ = admin.Exec(del, tenant, n)
				}
			}
			cleanup()
			t.Cleanup(cleanup)

			wasm := []byte{0x00, 0x61, 0x73, 0x6d}
			if _, err := admin.Exec(ins, stored, wasm, tenant); err != nil {
				t.Fatalf("seeding workflow_defs(%s) on %s: %v", stored, d.dialect, err)
			}

			count := func(name string) int {
				t.Helper()
				var n int
				if err := admin.QueryRow(countQ, tenant, name).Scan(&n); err != nil {
					t.Fatalf("counting %q on %s: %v", name, d.dialect, err)
				}
				return n
			}

			// PRECONDITION 1 -- the row is there AND this connection can see
			// it. Without this the whole table below can be zeros.
			if n := count(stored); n != 1 {
				t.Fatalf("UNMEASURED on %s: the row just inserted as %q counts %d, not 1.\n"+
					"Nothing below is a statement about collation -- a lookup that cannot "+
					"find the row it just wrote reports 'case-sensitive' for every dialect.\n"+
					"On SQL Server the likely cause is the tenant FILTER predicate on "+
					"workflow_defs: a connection with no SESSION_CONTEXT sees no rows and "+
					"gets no error (cleat#982). Check how this pool sets the tenant, not "+
					"the collation.", d.dialect, stored, n)
			}

			// PRECONDITION 2 -- a name that was never stored counts 0. This is
			// the negative control: it proves the predicate can DISCRIMINATE,
			// so that a 1 below means the server matched rather than that the
			// predicate matches everything.
			const absent = "CaseFoldProbe.NeverInserted"
			if n := count(absent); n != 0 {
				t.Fatalf("UNMEASURED on %s: %q was never inserted and counts %d, not 0, "+
					"so this predicate does not discriminate and a match below would "+
					"prove nothing.", d.dialect, absent, n)
			}

			// THE MEASUREMENT.
			got := count(miscased) > 0
			if got != d.foldsCase {
				verb := map[bool]string{true: "MATCHED", false: "did not match"}
				t.Errorf("on %s, %q %s a row stored as %q; want it to %s.\n\n"+
					"  recorded reason for this dialect: %s\n\n"+
					"If the backend or the image changed, this test is now the stale "+
					"thing and IMPROVEMENT-PLAN.md 3.316 must move with it -- the point "+
					"of the section is WHICH dialects fold, and a dialect changing "+
					"column changes the decision it is waiting on.",
					d.dialect, miscased, verb[got], stored, verb[d.foldsCase], d.why)
			}

			// THE SECOND HALF, and it is the one an operator meets first: can
			// both spellings be registered at all? On PostgreSQL yes, two
			// definitions; on the other two the primary key folds them
			// together and the second is refused. A deployment that works on
			// one dialect is impossible on the others.
			_, err := admin.Exec(ins, miscased, wasm, tenant)
			rejected := err != nil
			if rejected != d.foldsCase {
				t.Errorf("on %s, registering both %q and %q: rejected=%v, want rejected=%v.\n"+
					"  A folding collation makes the two spellings one primary key "+
					"(tenant_id, name, version); a deterministic one makes them two rows.\n"+
					"  error: %v", d.dialect, stored, miscased, rejected, d.foldsCase, err)
			}
			// A rejection must be the DUPLICATE, not some unrelated failure --
			// a NOT NULL violation or a dead connection would also be non-nil
			// and would satisfy the check above for the wrong reason.
			if rejected && d.foldsCase {
				msg := strings.ToLower(err.Error())
				if !strings.Contains(msg, "duplicate") && !strings.Contains(msg, "primary key") {
					t.Errorf("on %s the second spelling was refused, but not as a "+
						"duplicate key -- so this is not evidence that the collation "+
						"folded the two names together: %v", d.dialect, err)
				}
			}
		})
	}
}
