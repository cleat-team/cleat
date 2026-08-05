package engine

// IMPROVEMENT-PLAN 3.19: CreateUpdateRequest wrote a payload the columns reject.
//
// §2.60c found that `workflow_signals.payload` must hold valid JSON on all three
// dialects -- PostgreSQL `JSONB`, MySQL `JSON`, SQL Server `NVARCHAR(MAX)` with
// a CHECK -- and that only PostgresStore knew. It extracted encodeSignalPayload
// (now encodeJSONPayload) and applied it to the signal path on all three.
//
// It did not reach workflow_update_requests.payload, the sibling column with the
// same requirement, where each store was wrong in its own way:
//
//	PostgresStore  wrapped with `"` + payload + `"`, the concatenation §2.60c
//	               identified as producing invalid JSON the moment the payload
//	               contains a quote or a backslash
//	MySQLStore     did not wrap at all -- Error 3140 on a non-JSON payload
//	MSSQLStore     did not wrap at all -- CHECK constraint violation
//
// So `CreateUpdateRequest(ctx, wf, "name", "payload-1", ...)` succeeded on
// PostgreSQL and failed on the other two, and a payload containing a quote
// failed on all three. Found by pointing engine/testutil's MSSQL schema at the
// shipped migration (§2.71), which is the only place the constraint exists.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestCreateUpdateRequestAcceptsAnyPayload(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			const defName = "update-payload-def"
			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}
			runID, _, err := store.StartNewRun(ctx, "", defName, 1, json.RawMessage(`{}`),
				fmt.Sprintf("upd-payload-%d", time.Now().UnixNano()), DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			for i, tc := range []struct {
				name    string
				payload string
			}{
				{"an object", `{"data":"payload"}`},
				// The plain-string case: valid input, and not JSON.
				{"not JSON", "payload-1"},
				// §2.60c's second defect, in the copy it did not reach: the
				// quote-concatenation produces invalid JSON for both of these.
				{"a quote", `he said "hello"`},
				{"a backslash", `C:\path\to\file`},
			} {
				tc := tc
				t.Run(tc.name, func(t *testing.T) {
					updateName := fmt.Sprintf("upd-%d", i)
					if err := store.CreateUpdateRequest(ctx, runID, updateName, tc.payload,
						fmt.Sprintf("promise-%d", i)); err != nil {
						t.Fatalf("CreateUpdateRequest(%q): %v", tc.payload, err)
					}

					// And it has to come back as what went in. Storing it in a
					// form the column accepts but the reader cannot decode
					// would satisfy the constraint and lose the payload.
					reqs, err := store.GetPendingUpdateRequests(ctx, runID)
					if err != nil {
						t.Fatalf("GetPendingUpdateRequests: %v", err)
					}
					var found bool
					for _, r := range reqs {
						if r.UpdateName != updateName {
							continue
						}
						found = true
						// Every dialect returns what the caller passed in. It
						// did not before: PostgresStore unwrapped with
						// `payload #>> '{}'` while MySQLStore and MSSQLStore
						// returned the quoted form, so the same call answered
						// differently per backend (IMPROVEMENT-PLAN 3.19).
						if r.Payload != tc.payload {
							t.Errorf("payload came back as %q, want %q", r.Payload, tc.payload)
						}
					}
					if !found {
						t.Errorf("update request %q is not among the pending ones", updateName)
					}
				})
			}
		})
	}
}
