package eventtriggers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/plugin"
)

// cleat#2227. Before it, plugin.Environment.SignalWorkflow returned nil for
// EVERY case where the target workflow was not visible under the caller's
// own tenant -- foreign, nonexistent, and purged were indistinguishable from
// a genuine delivery (cleat#2218's fix for DeliverSignal's existence
// oracle). signalAwaiters only unregistered an awaiter on that nil, so an
// awaiter whose workflow had been purged looked exactly like one that had
// just been served, and stayed registered forever -- cleat#2213's leak.
//
// cleat#2227 makes SignalWorkflow return the typed plugin.ErrWorkflowNotFound
// for that case instead of nil, and this test proves signalAwaiters reacts
// to it specifically: unregistering on ErrWorkflowNotFound, same as it
// already did on success, and NOT on an ordinary delivery failure -- the
// control below, without which "unregister on any error" would pass this
// test just as well and reintroduce a different bug (an awaiter dropped
// after a transient failure, never getting the delivery it registered for).
func TestANotFoundAwaiterUnregistersInsteadOfLeaking(t *testing.T) {
	t.Run("ErrWorkflowNotFound unregisters the awaiter", func(t *testing.T) {
		db := newRecordingDB(t)
		db.awaiters = []string{"wf-purged"}

		env := &plugin.Environment{
			SignalWorkflow: func(_ context.Context, workflowID, _, _ string) error {
				if workflowID != "wf-purged" {
					t.Fatalf("SignalWorkflow called with workflowID %q, want wf-purged", workflowID)
				}
				return plugin.ErrWorkflowNotFound
			},
		}

		signalAwaiters(context.Background(), db, quietLogger(), env,
			uuid.New(), "order.created", `{}`)

		if !execedDelete(db, "wf-purged", "order.created") {
			t.Errorf("no DELETE FROM event_awaiters for wf-purged was issued; the awaiter leaks "+
				"forever now that its workflow is confirmed gone (cleat#2213 reopened).\nexecs: %v",
				db.execQueries)
		}
	})

	// THE CONTROL. Without it, "unregister on any non-nil error" would pass
	// the arm above too, and would drop an awaiter after a merely transient
	// SignalWorkflow failure -- one that should be retried on the NEXT
	// publish of this event type, not discarded.
	t.Run("an ordinary delivery failure does not unregister the awaiter", func(t *testing.T) {
		db := newRecordingDB(t)
		db.awaiters = []string{"wf-flaky"}
		boom := errors.New("connection reset")

		env := &plugin.Environment{
			SignalWorkflow: func(context.Context, string, string, string) error {
				return boom
			},
		}

		signalAwaiters(context.Background(), db, quietLogger(), env,
			uuid.New(), "order.created", `{}`)

		if execedDelete(db, "wf-flaky", "order.created") {
			t.Errorf("a DELETE FROM event_awaiters was issued for wf-flaky after an ordinary "+
				"error (%v), not ErrWorkflowNotFound -- a transient failure now silently drops "+
				"the awaiter instead of leaving it to retry on the next publish.\nexecs: %v",
				boom, db.execQueries)
		}
	})
}

// execedDelete reports whether db recorded a DELETE FROM event_awaiters
// naming workflowID and eventType, among its positional args -- checked on
// the args rather than the query text, since the query is the same literal
// for every call and only the bound values distinguish them.
func execedDelete(db *recordingDB, workflowID, eventType string) bool {
	for i, q := range db.execQueries {
		if !strings.Contains(q, "DELETE FROM event_awaiters") {
			continue
		}
		args := db.execs[i]
		if len(args) < 2 {
			continue
		}
		if args[0] == workflowID && args[1] == eventType {
			return true
		}
	}
	return false
}
