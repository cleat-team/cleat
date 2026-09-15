package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// What a workflow INPUT and a SIGNAL PAYLOAD are worth after they have been
// through their columns, per dialect. cleat#1022, second half.
//
// THIS TEST PINS A DEFECT ON PURPOSE AND IS NOT AN ENDORSEMENT, exactly as its
// sibling TestAWorkflowResultSurvivesTheColumnOrDoesNot does. Read that one
// first: it has the boundary, the cliff pairs, and why the cases come in twos.
//
// WHY A SECOND TEST RATHER THAN MORE ROWS IN THE FIRST. cleat#1022 is written
// about `result`, and both measurements on it covered `result` alone. That
// framing is wrong, and the correction is the reason this file exists:
//
//	MySQL, one INSERT, two columns of the same row --
//	  j JSON  ->  {"x": 1.2345678901234566e29}
//	  t TEXT  ->  {"x":123456789012345678901234567890}
//
// The narrowing belongs to the JSON *type*, not to the result column, so every
// MySQL JSON column holding caller-controlled data has it. `result` is simply
// the one somebody looked at.
//
// AND THE OTHER COLUMNS ARE WORSE THAN `result`, which is why this is not
// tidiness. A degraded result is a wrong value handed back to a caller who
// returned the right one and could in principle compare the two. A degraded
// INPUT is a wrong value handed to the workflow body: every branch and every
// arithmetic result downstream is computed from it, `StartNewRun` returned 200,
// and the true value exists nowhere in the system to compare against.
//
// It is at least deterministic. The degradation happens once, at write, so a
// replay reads the same degraded value the live run read. Silent corruption,
// not a determinism break -- worth stating because the determinism failure is
// the one you would go looking for and it is not there.
//
// WHAT IS NOT AFFECTED, measured rather than assumed: `event_history` stores
// request, response, signal_payload, child_input, new_input, plugin_input and
// plugin_output as LONGTEXT on MySQL, and all of them preserve. In particular a
// DURABLE CALL'S RECORDED RESULT IS SAFE. Within that one table cleat already
// stores caller-controlled JSON as text seven times over, so the JSON-typed
// columns are the outliers rather than the convention.
//
// `workflow_instances.query_state` is deliberately NOT covered here. It is the
// one column MySQL genuinely queries as JSON -- JSON_UNQUOTE(JSON_EXTRACT(...))
// at engine/mysql_store.go:434 -- so it cannot become text without rewriting
// that read, and it is therefore expected to stay degraded on MySQL after the
// fix the sibling columns get. Stated rather than left implied, so that nobody
// reads its absence as an oversight.
//
// If you are here because this test went red after a deliberate fix: good, that
// is the design. Update preservedBy, do not delete the case.
func TestAnInputSurvivesTheColumnOrDoesNot(t *testing.T) {
	const tenant = "c1022bbb-1022-4022-8022-c10220001022"

	cases := []struct {
		name        string
		payload     string
		preservedBy []string
	}{
		// Control. If this degrades anywhere the harness is broken rather than
		// the dialect, and every row below is meaningless.
		{"small int", `42`, []string{"postgres", "mysql", "mssql"}},

		// Second control, and it is the one that gives the negatives their
		// meaning: the SAME DIGITS as a JSON string. Text of this length
		// survives on every dialect, so a DEGRADED row below is the column
		// narrowing a NUMBER -- not the value being truncated somewhere.
		{"same digits as a string", `"123456789012345678901234567890"`,
			[]string{"postgres", "mysql", "mssql"}},

		// The positive cliff, one unit apart. MySQL's JSON keeps an integer as
		// INT64 *or* UINT64, so the positive limit is 2^64-1 and not BIGINT.
		{"2^64-1 (uint64 max)", `18446744073709551615`, []string{"postgres", "mysql", "mssql"}},
		{"2^64 (past uint64)", `18446744073709551616`, []string{"postgres", "mssql"}},

		// The negative cliff, one unit apart. Asymmetric with the positive one.
		{"-2^63 (int64 min)", `-9223372036854775808`, []string{"postgres", "mysql", "mssql"}},
		{"-2^63-1 (past int64 min)", `-9223372036854775809`, []string{"postgres", "mssql"}},

		{"1.23e29 (cleat#1022's value)", `123456789012345678901234567890`,
			[]string{"postgres", "mssql"}},

		// Decimals are not a special case of integers; they have the same cliff
		// at float64's precision.
		{"17 significant digits", `0.12345678901234567`, []string{"postgres", "mssql"}},
	}

	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			base, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			s := storeForTenant(t, base, tenant)

			// rawDBOf (flush_fence_test.go) reaches the store's own *sql.DB.
			// The whole question is what the COLUMN holds, and anything that
			// goes back through the read path can repair or re-degrade the
			// value on the way out.
			db := rawDBOf(t, base)

			if err := s.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: "in-1022", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}

			for _, tc := range cases {
				sent := fmt.Sprintf(`{"x":%s}`, tc.payload)

				want := false
				for _, d := range tc.preservedBy {
					if d == backend.Name() {
						want = true
					}
				}

				// --- workflow_instances.input, via StartNewRun ---------------
				//
				// The idempotency key must be unique per case: StartNewRun
				// returns the EXISTING run for a repeated key, so a shared key
				// would silently measure the first case eight times and report
				// seven passes it never made.
				runID, _, err := s.StartNewRun(ctx, "", "in-1022", 1, json.RawMessage(sent),
					fmt.Sprintf("in-1022-%s-%d", tc.name, time.Now().UnixNano()), tenant, 0)
				if err != nil {
					t.Fatalf("%s: StartNewRun: %v", tc.name, err)
				}
				storedIn := readJSONColumn(t, ctx, db, backend.Name(), tenant,
					"workflow_instances", "input", "id", runID, "")
				checkPreserved(t, backend.Name(), "workflow_instances.input", tc.name,
					tc.payload, sent, storedIn, want)

				// --- workflow_signals.payload, via DeliverSignal -------------
				sigName := "sig-" + strings.ReplaceAll(tc.name, " ", "-")
				if err := s.DeliverSignal(ctx, runID, sigName, sent); err != nil {
					t.Fatalf("%s: DeliverSignal: %v", tc.name, err)
				}
				storedSig := readJSONColumn(t, ctx, db, backend.Name(), tenant,
					"workflow_signals", "payload", "workflow_id", runID, sigName)
				checkPreserved(t, backend.Name(), "workflow_signals.payload", tc.name,
					tc.payload, sent, storedSig, want)
			}
		})
	}
}

// checkPreserved compares BYTES, never decoded numbers. Decoding is exactly what
// hides this: json.Unmarshal into a float64 turns both sides into the same
// degraded value and the assertion passes on every dialect.
//
// It reports the two directions differently on purpose. "Stopped preserving" is
// a regression; "started preserving" is the deliberate fix landing, and a test
// that says only "want X got Y" sends the reader of a GOOD change hunting for a
// breakage.
func checkPreserved(t *testing.T, dialect, column, caseName, payload, sent, stored string, want bool) {
	t.Helper()
	got := strings.Contains(stored, payload)
	switch {
	case want && !got:
		t.Errorf("%s: %s no longer preserves this literal in %s.\n  sent:   %s\n  stored: %s\n\n"+
			"A dialect that used to hold this value exactly has stopped. If a column type or "+
			"driver changed, that is a regression in what a caller's data is worth on this backend.",
			caseName, dialect, column, sent, stored)
	case !want && got:
		t.Errorf("%s: %s now preserves this literal in %s, and cleat#1022 says it does not.\n"+
			"  sent:   %s\n  stored: %s\n\n"+
			"This is a FIX, not a failure -- but the boundary this test pins has moved, so "+
			"update the preservedBy set and say what changed. Do not delete the case: it is the "+
			"only thing that would catch the degradation coming back.",
			caseName, dialect, column, sent, stored)
	}
}
