package engine

// cleat#1123, and the reason it is a separate file from its sibling.
//
// TestGetWorkflowByIDReturnsEveryFieldTheRowCanHold was written for #1105 to
// "guard the class" after four fields were found missing from one read path in
// a single day. It guards exactly one member of that class: GetWorkflowByID.
// The LIST path was never in its denominator, and #1123 is what that costs --
// `GET /api/workflows` omits started_at and completed_at while
// `GET /api/workflows/{id}` returns both, on the same run and the same binary.
//
// That is the shape CLAUDE.md keeps naming: a check consistent with itself and
// silent about what it is not looking at. The repair is not a more careful
// version of the existing guard -- no amount of care inside it reaches a
// function it does not call -- it is a second member.
//
// # This test does not decide #1123
//
// Whether the list SHOULD carry every field is a product question: a list
// endpoint has a real argument for a narrow row, and `result` on a thousand
// runs is a different proposition from `result` on one. So the omissions are
// recorded as exemptions WITH their reason rather than asserted away, and the
// guard covers everything else.
//
// If #1123 decides a field belongs in the list, delete its entry below and this
// test enforces it. That is the whole intended maintenance path.

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestListWorkflowsReturnsEveryFieldTheRowCanHold(t *testing.T) {
	// Two kinds of entry, and the distinction is the point.
	//
	// NOT A COLUMN: a fact about the schema, identical to the sibling guard's
	// exemption and checkable against migrations/postgres/001_schema.sql.
	//
	// NOT SELECTED BY THE LIST: a fact about this read path, open at
	// cleat#1123. Each is a field GetWorkflowByID returns and the list does
	// not, so it is a deliberate narrowing or an oversight, and nobody has
	// said which.
	exempt := map[string]string{
		"min_version": "not a column of workflow_instances -- it belongs to " +
			"workflow_defs and describes the DEFINITION's compatibility floor",

		// `error` is deliberately NOT exempt, and this comment is here because
		// it was, briefly. I assumed the list dropped it, reasoning from the
		// column name (`error_msg`) not matching the json tag (`error`). The
		// list scans error_msg into a local and assigns it, so the field is
		// carried -- proven by removing the exemption and watching the test
		// stay green on all three dialects while tenant_id, removed in the
		// same run, went red.
		//
		// An exemption is never checked. Exempting a field that works removes
		// it from the guard's denominator, which is the exact defect this file
		// exists to answer, committed inside the answer.
		"result": "not selected by the list path. Defensible: a result can be " +
			"large and a list returns many rows, so carrying it multiplies the " +
			"payload by the page size. cleat#1123",
		"tenant_id": "not selected by the list path. Every row a caller can see " +
			"is already scoped to its tenant, so the field is constant across " +
			"the page. cleat#1123",
		"completed_at": "not selected by the list path -- THE FINDING in " +
			"cleat#1123. GetWorkflowByID returns it. No size argument applies " +
			"to a timestamp, so this one reads as an oversight rather than a " +
			"narrowing, and it is the field an operator sorts a run list by",
		"started_at": "not selected by the list path -- cleat#1123, added by " +
			"#1094 and reaching only the single GET. Same reasoning as " +
			"completed_at",
		"parent_workflow_id": "not selected by the list path. It was missing " +
			"from GetWorkflowByID too until #1103, so the list is one fix " +
			"behind rather than deliberately narrow. cleat#1123",
		"pending_terminal_status": "not selected by the list path. Internal " +
			"bookkeeping for the finalize handshake rather than a fact a " +
			"caller asks about",
		"continued_from": "not selected by the list path. cleat#1123; #826 is " +
			"the finding that a continue-as-new chain is unfollowable, so this " +
			"one has a live argument for being carried",
		"reclaim_count": "not selected by the list path, though it is present " +
			"in the JSON as a zero because it is a plain int rather than a " +
			"pointer -- absent and zero look identical here, which is its own " +
			"small hazard. cleat#1123",
	}

	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			id, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{"in":1}`), "everyfield-list", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			// The same hostile fixture the sibling uses: every column non-zero,
			// so a zero in the result means the read lost it rather than the
			// row never having had it.
			makeEveryColumnNonZero(t, store, id)

			rows, err := store.ListWorkflows(ctx, WorkflowFilter{Limit: 100})
			if err != nil {
				t.Fatalf("ListWorkflows: %v", err)
			}
			var got *WorkflowInstance
			for i := range rows {
				if rows[i].ID == id {
					got = &rows[i]
					break
				}
			}
			if got == nil {
				t.Fatalf("ListWorkflows returned %d rows and none was the run "+
					"under test (%s); the filter or the tenant scope is wrong, "+
					"and every assertion below would be vacuous", len(rows), id)
			}

			v := reflect.ValueOf(*got)
			ty := v.Type()
			checked := 0
			for i := 0; i < ty.NumField(); i++ {
				name := strings.Split(ty.Field(i).Tag.Get("json"), ",")[0]
				if name == "" || name == "-" {
					continue
				}
				if why, ok := exempt[name]; ok {
					t.Logf("exempt: %s -- %s", name, why)
					continue
				}
				checked++
				if v.Field(i).IsZero() {
					t.Errorf("ListWorkflows left %s at its zero value.\n\n"+
						"Every column of this row was set non-zero before the "+
						"fetch, so a zero here means the list path does not "+
						"carry the field. GetWorkflowByID and ListWorkflows "+
						"read the same table through different SELECTs "+
						"(workflowInstanceColumns), and they have already "+
						"drifted once -- cleat#1123.\n\nIf the field genuinely "+
						"should not appear in a list, add it to `exempt` WITH "+
						"THE REASON.", name)
				}
			}

			// A guard that checks nothing passes. This one exempts more than
			// its sibling does, which makes the floor matter more rather than
			// less: if the exemption list ever swallows the whole struct, the
			// test would still be green and would be asserting nothing.
			if checked < 8 {
				t.Fatalf("only %d fields were checked against ListWorkflows; "+
					"the exemption list has grown to cover the struct, or the "+
					"reflection stopped seeing tags. Either way this guard has "+
					"stopped having an opinion.", checked)
			}
		})
	}
}
