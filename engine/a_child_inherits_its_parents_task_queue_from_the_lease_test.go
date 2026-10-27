package engine

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3245 B4a: a child started without its own task queue runs on its
// parent's, and both child-start paths read the parent's queue from its lease
// row. B5 drops task_queue from workflow_instances.
//
// The parent is seeded with testutil.SeedRun, whose instance row is narrow, so
// its workflow_instances.task_queue sits at the column default ('default')
// while its lease row carries the real queue. A read of the instance row
// therefore gives the child the wrong queue.
func TestAChildInheritsItsParentsTaskQueueFromTheLeaseRow(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()
	startPayloadReaderRun(t, store, "b4a-child-queue") // deploys the def

	const parentQueue = "b4a-parent-queue"
	parentID := "b4a-child-queue-parent"
	testutil.SeedRun(t, db, testutil.RunSeed{
		ID: parentID, TenantID: DefaultTenantUUID, DefName: "b4a-child-queue", TaskQueue: parentQueue,
	})

	queueOf := func(id string) string {
		t.Helper()
		var q string
		if err := db.QueryRow(`SELECT task_queue FROM workflow_leases WHERE id = $1`, id).Scan(&q); err != nil {
			t.Fatalf("read %s's task_queue: %v", id, err)
		}
		return q
	}

	child, err := store.StartChildWorkflow(ctx, parentID, "b4a-child-queue", `{}`, 1, "ABANDON", 0)
	if err != nil {
		t.Fatalf("StartChildWorkflow: %v", err)
	}
	if got := queueOf(child); got != parentQueue {
		t.Errorf("StartChildWorkflow: child task_queue = %q, want the parent's %q", got, parentQueue)
	}

	event := EventRecord{Step: 0, EventType: "child_workflow", ChildName: "b4a-child-queue", ChildInput: `{}`}
	atomic, err := store.StartChildWorkflowAtomic(ctx, uuid.New().String(), parentID, "b4a-child-queue", `{}`, 1, "ABANDON", event, 0)
	if err != nil {
		t.Fatalf("StartChildWorkflowAtomic: %v", err)
	}
	if got := queueOf(atomic); got != parentQueue {
		t.Errorf("StartChildWorkflowAtomic: child task_queue = %q, want the parent's %q", got, parentQueue)
	}
}
