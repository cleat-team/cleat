package engine

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// What a workflow result is worth after it has been through the result column,
// per dialect. cleat#1022.
//
// THIS TEST PINS A DEFECT ON PURPOSE, AND IT IS NOT AN ENDORSEMENT. On MySQL a
// large integer or a high-precision decimal comes back with a DIFFERENT VALUE
// from the one the workflow returned. Nothing errors, nothing logs, the result
// is still valid JSON, still an object, still the right shape, and still
// plausible. The API returns what the column holds, so a caller receives the
// degraded value.
//
// The resolution -- document the limit, detect and log, or store as text -- is
// a product decision and this test does not make it. What it does is make the
// boundary CHECKABLE, because until now it was measured twice and pinned by
// nothing: both probes were throwaways that never entered the tree. A driver
// change, a column-type change, or a partial fix could move either cliff and
// no test would notice.
//
// If you are here because this test went red after a deliberate fix: good, that
// is the design. Update the table, do not delete the test.
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

	// preservedBy lists the dialects that store the literal unchanged. A
	// dialect absent from the set degrades it.
	cases := []struct {
		name        string
		payload     string
		preservedBy []string
	}{
		// Control. If this degrades anywhere, the harness is broken rather
		// than the dialect, and every row below is meaningless.
		{"small int", `42`, []string{"postgres", "mysql", "mssql"}},

		{"2^63-1 (int64 max)", `9223372036854775807`, []string{"postgres", "mysql", "mssql"}},
		{"2^63 (past int64)", `9223372036854775808`, []string{"postgres", "mysql", "mssql"}},

		// The positive cliff, one unit apart.
		{"2^64-1 (uint64 max)", `18446744073709551615`, []string{"postgres", "mysql", "mssql"}},
		{"2^64 (past uint64)", `18446744073709551616`, []string{"postgres", "mssql"}},

		// The negative cliff, one unit apart. Asymmetric with the positive one.
		{"-2^63 (int64 min)", `-9223372036854775808`, []string{"postgres", "mysql", "mssql"}},
		{"-2^63-1 (past int64 min)", `-9223372036854775809`, []string{"postgres", "mssql"}},

		{"1.23e29 (the original report)", `123456789012345678901234567890`, []string{"postgres", "mssql"}},

		// Decimals. Neither earlier measurement covered these.
		{"17 significant digits", `0.12345678901234567`, []string{"postgres", "mssql"}},
		{"23 significant digits", `0.12345678901234567890123`, []string{"postgres", "mssql"}},
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
				// sides into the same degraded value and the test passes.
				got := strings.Contains(stored, tc.payload)
				want := false
				for _, d := range tc.preservedBy {
					if d == backend.Name() {
						want = true
					}
				}

				switch {
				case want && !got:
					t.Errorf("%s: %s no longer preserves this literal.\n  sent:   %s\n  stored: %s\n\n"+
						"A dialect that used to hold this value exactly has stopped. If a column "+
						"type or driver changed, that is a regression in what a workflow result is "+
						"worth on this backend.", tc.name, backend.Name(), sent, stored)
				case !want && got:
					t.Errorf("%s: %s now preserves this literal, and cleat#1022 says it does not.\n"+
						"  sent:   %s\n  stored: %s\n\n"+
						"This is a FIX, not a failure -- but the boundary this test pins has moved, "+
						"so update the preservedBy set and say what changed. Do not delete the case: "+
						"it is the only thing that would catch the degradation coming back.",
						tc.name, backend.Name(), sent, stored)
				}
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
	var stored string

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
		if err := conn.QueryRowContext(ctx,
			`SELECT result FROM workflow_instances WHERE id = @p1`, wfID).Scan(&stored); err != nil {
			t.Fatalf("mssql read result column: %v", err)
		}
		return stored
	}

	q := "SELECT CAST(result AS CHAR) FROM workflow_instances WHERE id = ?"
	if dialect == "postgres" {
		q = "SELECT result::text FROM workflow_instances WHERE id = $1"
	}
	if err := db.QueryRowContext(ctx, q, wfID).Scan(&stored); err != nil {
		t.Fatalf("%s read result column: %v", dialect, err)
	}
	return stored
}
