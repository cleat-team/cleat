package engine

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// What a workflow result is worth after it has been through the result column,
// per dialect. cleat#1022.
//
// THIS TEST PINNED A DEFECT UNTIL THE MIGRATION IN THIS SAME PR. On MySQL a
// large integer or a high-precision decimal came back with a DIFFERENT VALUE
// from the one the workflow returned -- no error, no log, still valid JSON,
// still an object, still the right shape, still plausible. The API returns what
// the column holds, so a caller received the degraded value.
//
// The resolution this test deliberately did not make has now been made: STORE
// AS TEXT, which is what SQL Server has always done and why SQL Server never
// had the defect. migrations/mysql/070 makes these columns LONGTEXT with a
// JSON_VALID check; migrations/mysql/071 stops finalize_workflow_status casting
// the result to JSON, which re-degraded the value BEFORE it reached the column
// and would have left this test green over a still-broken path.
//
// THIS TEST IS THE EVIDENCE THE FIX WORKED, which is why it was written first
// and in its own PR: it went red in the "now preserves ... this is a FIX"
// direction on every previously-degraded case, on a database with the migration
// applied and on one without it. The per-dialect expectation is gone because it
// existed only to encode a divergence that no longer exists.
//
// WHY THE CASES ARE IN PAIRS ONE UNIT APART. cleat#1022's own text says the
// limit is BIGINT. It is not: MySQL's JSON keeps an integer as INT64 *or*
// UINT64 and only falls back to DOUBLE when it fits neither, so the real range
// is asymmetric -- [-2^63, 2^64-1] -- and the positive end is twice as far out
// as the issue claims. A test written against BIGINT would sit nowhere near the
// edge and would pass while the boundary moved underneath it. Each pair here
// straddles a cliff by one, so either side moving is a failure.
//
// THE DECIMAL CASES ARE NEW. Both previous measurements covered integers only
// and said so. Measured here: MySQL degrades a decimal needing more precision
// than a float64 holds -- 0.12345678901234567 comes back as
// 0.12345678901234568 -- so integers are not a special case, they are the half
// somebody happened to look at.
func TestAWorkflowResultSurvivesTheColumnOrDoesNot(t *testing.T) {
	const tenant = "c1022000-1022-4022-8022-c10220001022"

	cases := []struct {
		name    string
		payload string
	}{
		// Control. If this degrades anywhere, the harness is broken rather
		// than the dialect, and every row below is meaningless.
		{"small int", `42`},

		{"2^63-1 (int64 max)", `9223372036854775807`},
		{"2^63 (past int64)", `9223372036854775808`},

		// The positive cliff, one unit apart.
		{"2^64-1 (uint64 max)", `18446744073709551615`},
		{"2^64 (past uint64)", `18446744073709551616`},

		// The negative cliff, one unit apart. Asymmetric with the positive one.
		{"-2^63 (int64 min)", `-9223372036854775808`},
		{"-2^63-1 (past int64 min)", `-9223372036854775809`},

		{"1.23e29 (the original report)", `123456789012345678901234567890`},

		// Decimals. Neither earlier measurement covered these.
		{"17 significant digits", `0.12345678901234567`},
		{"23 significant digits", `0.12345678901234567890123`},
	}

	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			base, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			s := storeForTenant(t, base, tenant)

			// rawDBOf (flush_fence_test.go) reaches the store's own *sql.DB. The
			// whole question is what the COLUMN holds, and anything that goes back
			// through the read path can repair or re-degrade the value on the way out.
			db := rawDBOf(t, base)

			for _, tc := range cases {
				wfID := seedRunForTenant(t, s, tenant, "prec-"+strings.ReplaceAll(tc.name, " ", "-"))

				// CompleteWorkflow is fenced on (assigned_to, generation), so
				// an unclaimed run fails with "fence lost" -- which reads as a
				// dialect problem and is a fixture problem. One run is
				// outstanding at a time, so a single claim cannot consume a
				// sibling (cleat#1115).
				claimed, err := s.ClaimWorkflow(ctx, "w1")
				if err != nil || claimed == nil {
					t.Fatalf("%s: ClaimWorkflow: %v (nil=%v)", tc.name, err, claimed == nil)
				}
				if claimed.ID != wfID {
					t.Fatalf("%s: claimed %s but seeded %s -- another run was outstanding, so "+
						"this case is measuring the wrong row", tc.name, claimed.ID, wfID)
				}

				sent := fmt.Sprintf(`{"x":%s}`, tc.payload)
				if err := s.CompleteWorkflow(ctx, wfID, "w1", claimed.Generation, sent, nil); err != nil {
					t.Fatalf("%s: CompleteWorkflow: %v", tc.name, err)
				}

				stored := readResultColumn(t, ctx, db, backend.Name(), tenant, wfID)

				// Compare BYTES, never decoded numbers. Decoding is exactly
				// what hides this: json.Unmarshal into a float64 turns both
				// sides into the same value at the cliff edge and the test
				// passes.
				checkPreserved(t, backend.Name(), "workflow_instances.result",
					tc.name, tc.payload, sent, stored)
			}
		})
	}
}

// readResultColumn returns the result column as the database stores it.
//
// THE SQL SERVER BRANCH IS NOT BOILERPLATE. Its tenant isolation is a
// SECURITY POLICY with a FILTER PREDICATE, and a filter predicate exempts
// NOBODY -- not sysadmin, not db_owner (cleat#1491). So a raw pool read with no
// SESSION_CONTEXT returns zero rows, which surfaces as "sql: no rows in result
// set" and reads exactly like the row never having been written. PostgreSQL
// does not show this because the engine test role is a superuser and
// PostgreSQL exempts superusers from RLS unconditionally.
//
// Session context is per CONNECTION, so the set and the select have to share
// one -- a pool read would set it on one connection and select on another.
func readResultColumn(t *testing.T, ctx context.Context, db *sql.DB, dialect, tenant, wfID string) string {
	t.Helper()
	return readJSONColumn(t, ctx, db, dialect, tenant, "workflow_instances", "result", "id", wfID, "")
}

// readJSONColumn is readResultColumn generalised to any JSON-typed column, for
// cleat#1022's second half: `result` is not the boundary, it is the column
// somebody happened to measure. Same per-dialect rules apply unchanged, which is
// why this is one function and not a second copy -- the SQL Server branch below
// is subtle enough that a copy would drift.
//
// `keyCol`/`keyVal` select the row; `extraCol`/`extraVal` add a second
// predicate when the primary key needs two columns (workflow_signals is keyed
// on (workflow_id, signal_name)). Empty extraCol means one predicate.
func readJSONColumn(t *testing.T, ctx context.Context, db *sql.DB, dialect, tenant, table, col, keyCol, keyVal, extraVal string) string {
	t.Helper()
	var stored string
	extraCol := ""
	if extraVal != "" {
		extraCol = "signal_name"
	}

	if dialect == "mssql" {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("mssql conn: %v", err)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(ctx,
			`EXEC sp_set_session_context @key = N'tenant_id', @value = @p1`, tenant); err != nil {
			t.Fatalf("mssql set session context: %v", err)
		}
		q := `SELECT ` + col + ` FROM ` + table + ` WHERE ` + keyCol + ` = @p1`
		args := []any{keyVal}
		if extraCol != "" {
			q += ` AND ` + extraCol + ` = @p2`
			args = append(args, extraVal)
		}
		if err := conn.QueryRowContext(ctx, q, args...).Scan(&stored); err != nil {
			t.Fatalf("mssql read %s.%s: %v", table, col, err)
		}
		return stored
	}

	// CAST(... AS CHAR) on MySQL and ::text on PostgreSQL both ask the server
	// for the column's own stored text. Neither repairs a narrowed number --
	// by the time either runs, MySQL has already parsed the literal into a
	// DOUBLE and the digits are gone.
	q := "SELECT CAST(" + col + " AS CHAR) FROM " + table + " WHERE " + keyCol + " = ?"
	if dialect == "postgres" {
		q = "SELECT " + col + "::text FROM " + table + " WHERE " + keyCol + " = $1"
	}
	args := []any{keyVal}
	if extraCol != "" {
		if dialect == "postgres" {
			q += " AND " + extraCol + " = $2"
		} else {
			q += " AND " + extraCol + " = ?"
		}
		args = append(args, extraVal)
	}
	if err := db.QueryRowContext(ctx, q, args...).Scan(&stored); err != nil {
		t.Fatalf("%s read %s.%s: %v", dialect, table, col, err)
	}
	return stored
}

// The FINALIZE path preserves a result too, and it is a SECOND writer that the
// test above cannot see. cleat#1022.
//
// WHY THIS EXISTS AS A SEPARATE TEST RATHER THAN MORE ROWS ABOVE. Two different
// writers reach workflow_instances.result. CompleteWorkflow issues a plain
// UPDATE from Go. The FINALIZE half of the two-phase terminal transition goes
// through the finalize_workflow_status PROCEDURE -- engine/mysql_lifecycle.go's
// `CALL finalize_workflow_status(...)` on this dialect -- and that procedure
// wrote `result = CAST(p_result AS JSON)`.
//
// A CAST re-degrades the value BEFORE it reaches the column, so it is not fixed
// by a column type. Measured on a LONGTEXT column, which is what makes the
// point -- the column is not what is doing it:
//
//	SET r = CAST('{"x":123456789012345678901234567890}' AS JSON)
//	  -> {"x": 1.2345678901234566e29}
//	SET r =      '{"x":123456789012345678901234567890}'
//	  -> {"x":123456789012345678901234567890}
//
// So migrations/mysql/070 alone would have left the test above GREEN over a
// still-degrading path, which is the exact shape CLAUDE.md warns about: a check
// that reads cleanest where it measured least. migrations/mysql/071 removes the
// cast and this test is what holds it removed.
//
// p_query_state keeps its cast deliberately -- query_state stays a JSON column
// because it is the one column MySQL genuinely queries as JSON
// (engine/mysql_store.go:434) -- so it is still narrowed and is not asserted
// here.
func TestAFinalizedResultSurvivesTheColumn(t *testing.T) {
	const tenant = "c1022ccc-1022-4022-8022-c10220001022"

	// The two cliff-adjacent values plus a control. The full table lives on the
	// test above; what is being checked here is the WRITE PATH, not the
	// boundary, so repeating every row would be noise.
	cases := []struct{ name, payload string }{
		{"small int", `42`},
		{"2^64 (past uint64)", `18446744073709551616`},
		{"1.23e29 (cleat#1022's value)", `123456789012345678901234567890`},
		{"17 significant digits", `0.12345678901234567`},
	}

	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			base, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			s := storeForTenant(t, base, tenant)
			db := rawDBOf(t, base)

			for _, tc := range cases {
				wfID := seedRunForTenant(t, s, tenant,
					"fin-"+strings.ReplaceAll(tc.name, " ", "-"))

				// One run outstanding at a time, so a single claim cannot
				// consume a sibling (cleat#1115). Assert the claim is the run
				// we seeded rather than assuming it: claiming the wrong row
				// measures the wrong row and still passes.
				claimed, err := s.ClaimWorkflow(ctx, "w1")
				if err != nil || claimed == nil {
					t.Fatalf("%s: ClaimWorkflow: %v (nil=%v)", tc.name, err, claimed == nil)
				}
				if claimed.ID != wfID {
					t.Fatalf("%s: claimed %s but seeded %s -- another run was outstanding, "+
						"so this case is measuring the wrong row", tc.name, claimed.ID, wfID)
				}

				sent := fmt.Sprintf(`{"x":%s}`, tc.payload)
				if err := s.FinalizeWorkflowSegment(ctx, wfID, "w1", claimed.Generation,
					nil, "done", sent, "", "", nil, time.Time{}); err != nil {
					t.Fatalf("%s: FinalizeWorkflowSegment: %v", tc.name, err)
				}

				stored := readResultColumn(t, ctx, db, backend.Name(), tenant, wfID)
				checkPreserved(t, backend.Name(), "workflow_instances.result (finalize path)",
					tc.name, tc.payload, sent, stored)
			}
		})
	}
}
