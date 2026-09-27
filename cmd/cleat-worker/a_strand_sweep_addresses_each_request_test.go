package main

import (
	"context"
	"sort"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// Two stranded requests that SHARE a name must each be completed and each have
// its own promise rejected. cleat#1416.
//
// This state could not exist before. workflow_update_requests was
// PRIMARY KEY (workflow_id, update_name), so a workflow held at most one
// request per name and "complete the update called bump" addressed exactly one
// row. With the name reusable it addresses a SET, and failStrandedUpdates is
// the worst place for that to go unnoticed: it is the sweep whose entire job is
// to make sure nobody is left holding a promise nothing settles.
//
// Passing upd.UpdateName here would close every row sharing the name on the
// first iteration -- the store's predicate carries `status = 'pending'`, so the
// second iteration then matches nothing -- while rejecting only the first
// promise. The second caller's promise is never settled and never will be:
// this sweep looks for rows that are still `pending`, and that row now says
// `completed`. Measured on PostgreSQL against the migration with the old
// predicate in place, one UPDATE keyed on the name reported `UPDATE 2`.
//
// The guard is on the IDENTITIES, not on the call count. Counting completions
// would pass a version that called CompleteUpdateRequest twice with the same
// argument, which is precisely the bug.
func TestAStrandSweepAddressesEachRequestSeparately(t *testing.T) {
	ms := &mockStore{}
	ms.getPendingUpdateRequestsFn = func(ctx context.Context, workflowID string) ([]engine.UpdateRequestInfo, error) {
		return []engine.UpdateRequestInfo{
			{WorkflowID: workflowID, RequestID: "ureq-a", UpdateName: "bump", PromiseID: "prom-a"},
			{WorkflowID: workflowID, RequestID: "ureq-b", UpdateName: "bump", PromiseID: "prom-b"},
		}, nil
	}
	var completed, rejected []string
	ms.completeUpdateRequestFn = func(ctx context.Context, workflowID, requestID, result, errMsg string) error {
		completed = append(completed, requestID)
		return nil
	}
	ms.rejectPromiseFn = func(ctx context.Context, promiseID, errMsg string) error {
		rejected = append(rejected, promiseID)
		return nil
	}

	w := newTestWorker(ms)
	w.failStrandedUpdates(&engine.WorkflowInstance{ID: "wf-1"}, "done")

	sort.Strings(completed)
	sort.Strings(rejected)

	if len(completed) != 2 || completed[0] != "ureq-a" || completed[1] != "ureq-b" {
		t.Errorf("completed %v, want [ureq-a ureq-b].\n\n"+
			"Each request must be addressed by its own identity. Two calls carrying "+
			"\"bump\" would close both rows on the first and match nothing on the second, "+
			"and the second caller's promise would then be unreachable to this sweep "+
			"forever -- it looks for `pending`, and that row would say `completed`.",
			completed)
	}
	if len(rejected) != 2 || rejected[0] != "prom-a" || rejected[1] != "prom-b" {
		t.Errorf("rejected %v, want [prom-a prom-b] -- a caller was left holding a promise "+
			"nothing settles, which is the failure this sweep exists to prevent", rejected)
	}
}
