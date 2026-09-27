package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A workflow INPUT and a SIGNAL PAYLOAD survive their columns byte for byte, on
// every dialect. cleat#1022, second half.
//
// THIS TEST PINNED A DEFECT UNTIL THE MIGRATION IN THIS SAME PR, and the
// history is kept because the cases only make sense with it. MySQL's JSON type
// keeps an integer as INT64 or UINT64 and falls back to DOUBLE when it fits
// neither, so every value below outside [-2^63, 2^64-1] -- and any decimal
// needing more precision than a float64 holds -- USED TO BE REWRITTEN on the
// way in, on this dialect alone and with nothing to say so.
//
// migrations/mysql/070 stores these columns as LONGTEXT with a JSON_VALID
// check, which is what SQL Server has always done, so all three dialects now
// preserve. THE PER-DIALECT EXPECTATION IS GONE ON PURPOSE: it existed only to
// encode a divergence, and encoding "every dialect, every case" as three
// identical lists would be dead weight that hides the next real divergence.
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
// IF THIS GOES RED, A DIALECT HAS STARTED REWRITING A CALLER'S VALUE AGAIN.
// That is a regression and not a tidiness problem: the value reaches the
// workflow body, so every branch and every arithmetic result downstream is
// computed from it. Do not delete the case and do not relax it to a decoded
// comparison -- see checkPreserved for why decoding is blind exactly at the
// boundary.
func TestAnInputSurvivesTheColumnOrDoesNot(t *testing.T) {
	const tenant = "c1022bbb-1022-4022-8022-c10220001022"

	cases := []struct {
		name    string
		payload string
	}{
		// Control. If this degrades anywhere the harness is broken rather than
		// the dialect, and every row below is meaningless.
		{"small int", `42`},

		// Second control: the SAME DIGITS as a JSON string. It never depended
		// on the number path at all, so when these cases DID fail it separated
		// "the column narrows a NUMBER" from "the value is truncated
		// somewhere". Kept for the same reason after the fix.
		{"same digits as a string", `"123456789012345678901234567890"`},

		// The cliff MySQL's JSON type used to have, straddled one unit apart so
		// that either side moving is a failure. cleat#1022's own text put this
		// limit at BIGINT; it was not -- MySQL's JSON kept an integer as INT64
		// *or* UINT64, so the positive edge was 2^64-1, twice as far out. A
		// test written to the signed bound would have sat nowhere near the real
		// edge and passed while the boundary moved underneath it.
		{"2^64-1 (uint64 max)", `18446744073709551615`},
		{"2^64 (past uint64)", `18446744073709551616`},

		// The negative edge, which was asymmetric with the positive one.
		{"-2^63 (int64 min)", `-9223372036854775808`},
		{"-2^63-1 (past int64 min)", `-9223372036854775809`},

		{"1.23e29 (cleat#1022's value)", `123456789012345678901234567890`},

		// Decimals were not a special case of integers; they had the same edge
		// at float64's precision. Both earlier measurements of cleat#1022
		// covered integers only and said so.
		{"17 significant digits", `0.12345678901234567`},
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
					tc.payload, sent, storedIn)

				// --- workflow_signals.payload, via DeliverSignal -------------
				sigName := "sig-" + strings.ReplaceAll(tc.name, " ", "-")
				if err := s.DeliverSignal(ctx, runID, sigName, sent); err != nil {
					t.Fatalf("%s: DeliverSignal: %v", tc.name, err)
				}
				storedSig := readJSONColumn(t, ctx, db, backend.Name(), tenant,
					"workflow_signals", "payload", "workflow_id", runID, sigName)
				checkPreserved(t, backend.Name(), "workflow_signals.payload", tc.name,
					tc.payload, sent, storedSig)
			}
		})
	}
}

// checkPreserved compares BYTES, never decoded numbers.
//
// DECODING IS BLIND EXACTLY AT THE BOUNDARY, which is the case that matters
// most. A consumer decoding to a float64 recovers 2^64 exactly -- MySQL stored
// the text `1.8446744073709552e19`, and that text round-trips through a double
// back to 18446744073709551616 -- so a decoded comparison reports the one-unit-
// past-the-cliff case as equal while the stored document says something else.
// As exact decimals, every value MySQL used to rewrite denotes a different
// number from the one that was sent.
func checkPreserved(t *testing.T, dialect, column, caseName, payload, sent, stored string) {
	t.Helper()
	if strings.Contains(stored, payload) {
		return
	}
	t.Errorf("%s: %s did not preserve this literal in %s.\n  sent:   %s\n  stored: %s\n\n"+
		"A dialect has started rewriting a caller's value. Before cleat#1022 this was MySQL's "+
		"JSON column narrowing a number it could not hold, and migrations/mysql/070 fixed it by "+
		"storing these columns as LONGTEXT with a JSON_VALID check -- what SQL Server has always "+
		"done. If that migration was reverted, or a column was added back as JSON, or a write "+
		"path reintroduced CAST(... AS JSON) -- which re-degrades the value BEFORE it reaches "+
		"even a LONGTEXT column, and is why migrations/mysql/071 exists -- this is what it looks "+
		"like: no error, valid JSON, right shape, different value.",
		caseName, dialect, column, sent, stored)
}
