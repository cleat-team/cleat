package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// cleat#4500, cleat-review2's GAP on the first version: when the hashed shard said not-found and the LOOK for the run on another shard failed, onHolder returned the original
// ErrWorkflowNotFound. A client treats that as final, so a verb to an existing successor on a briefly unreachable shard was lost as "no such run". A shard that could not answer has not
// said it has no such run; the error must be returned, carry that shard's failure, and not be ErrWorkflowNotFound. (The mock shards are not SignalIdempotencyStore, so DeliverSignalIdempotent
// is covered by the same path in the real-shard rows and not here.)
func TestAShardThatCannotBeReadIsNotReportedAsARunThatDoesNotExist(t *testing.T) {
	ctx := context.Background()
	const id = "successor-4500"
	verbs := map[string]func(ss *ShardedStore) error{
		"DeliverSignal":           func(ss *ShardedStore) error { return ss.DeliverSignal(ctx, id, "S", `"x"`) },
		"DeliverSignalReceipt":    func(ss *ShardedStore) error { _, err := ss.DeliverSignalReceipt(ctx, id, "S", `"x"`, ""); return err },
		"TerminateWorkflow":       func(ss *ShardedStore) error { return ss.TerminateWorkflow(ctx, id, "r") },
		"TerminateWorkflowRcpt":   func(ss *ShardedStore) error { _, err := ss.TerminateWorkflowReceipt(ctx, id, "r"); return err },
		"CancelWorkflow":          func(ss *ShardedStore) error { return ss.CancelWorkflow(ctx, id, "r") },
		"CancelWorkflowReceipt":   func(ss *ShardedStore) error { _, err := ss.CancelWorkflowReceipt(ctx, id, "r"); return err },
		"RequestCancellation":     func(ss *ShardedStore) error { return ss.RequestCancellation(ctx, id, "r") },
		"RequestCancellationRcpt": func(ss *ShardedStore) error { _, err := ss.RequestCancellationReceipt(ctx, id, "r"); return err },
	}
	for name, do := range verbs {
		name, do := name, do
		t.Run(name, func(t *testing.T) {
			ss, mocks := makeShardedStore(t, 2)
			hashed := ss.getShard(id).Config.Name
			for _, m := range mocks {
				if m.name == hashed {
					m.err = ErrWorkflowNotFound // the hashed shard has no such run: the verb says so, and a look for it finds no row
					m.getWorkflowByIDFn = func(context.Context, string) (*WorkflowInstance, error) { return nil, nil }
				} else {
					m.getWorkflowByIDFn = func(context.Context, string) (*WorkflowInstance, error) {
						return nil, errors.New("connection refused")
					}
				}
			}
			err := do(ss)
			if err == nil || errors.Is(err, ErrWorkflowNotFound) {
				t.Fatalf("with the other shard unreadable the verb returned %v, want an error that is not ErrWorkflowNotFound", err)
			}
			if !strings.Contains(err.Error(), "connection refused") {
				t.Errorf("the error %q does not carry the other shard's failure", err)
			}
		})
	}
}
