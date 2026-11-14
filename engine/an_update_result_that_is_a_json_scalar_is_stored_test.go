package engine

// cleat#3816. An update's result is JSON, and a scalar is JSON: `"ok"`, `5`, `true` and `null` have to be
// storable on every backend, as an object is. On SQL Server three CHECK constraints (ck_workflow_instances_result,
// ck_workflow_promises_result, ck_workflow_update_requests_result) used the one-argument isjson, which is 0 for
// a bare scalar, so the write failed with "conflicted with the CHECK constraint". The other two are pinned by
// TestAScalarResultReadsBackExactlyAsStored; this is the third.
//
// What is NOT accepted is pinned too, because the repair widened a predicate and a test that only checks the new
// values would pass against one that accepted anything: `{` is not JSON on any backend.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

func TestAnUpdateResultThatIsAJSONScalarIsStored(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			base, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, base)
			truncateAll(t, base)
			ctx := context.Background()

			wf, _, err := base.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "scalar-update", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			complete := func(result string) error {
				if err := base.CreateUpdateRequest(ctx, wf, "u", `{}`, fmt.Sprintf("p-%s", result)); err != nil {
					t.Fatalf("CreateUpdateRequest: %v", err)
				}
				pending, err := base.GetPendingUpdateRequests(ctx, wf)
				if err != nil || len(pending) == 0 {
					t.Fatalf("GetPendingUpdateRequests = %v, %v; want the request just created", pending, err)
				}
				return base.CompleteUpdateRequest(ctx, wf, pending[len(pending)-1].RequestID, result, "")
			}

			for _, result := range []string{`{"a":1}`, `[1,2]`, `"yes"`, `"a\"b"`, `5`, `1.5`, `true`, `false`, `null`} {
				if err := complete(result); err != nil {
					t.Errorf("CompleteUpdateRequest(%s) = %v; want it stored", result, err)
				}
			}
			if err := complete(`{`); err == nil {
				t.Errorf("CompleteUpdateRequest(`{`) stored text that is not JSON")
			}
		})
	}
}
