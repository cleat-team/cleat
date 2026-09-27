package engine

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// orphanLogs captures the engine's structured output so the test can assert on
// what was REPORTED rather than on the function returning.
type orphanLogs struct{ buf bytes.Buffer }

func (o *orphanLogs) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&o.buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// sawOrphanReport keys on the orphan_child_run_ids ATTRIBUTE rather than on the
// message text. The message is prose and will be reworded; the attribute is the
// thing a log search would key on, so asserting it is asserting the contract.
func (o *orphanLogs) sawOrphanReport() bool {
	return strings.Contains(o.buf.String(), "orphan_child_run_ids")
}

// orphanLister is a childWfStore that answers OriginalChildRunIDs from a fixed
// list, so the detector's decision can be driven without a database.
type orphanLister struct {
	mockChildWorkflowStore
	started []string
	err     error
	asked   int
}

func (o *orphanLister) OriginalChildRunIDs(context.Context, string) ([]string, error) {
	o.asked++
	return o.started, o.err
}

// A child this parent started, absent from the history being replayed, names
// itself. cleat#1661.
//
// WHY THIS IS ANOMALOUS AND NOT A RACE. The child row and the parent's
// child_workflow event are written by ONE transaction -- StartChildWorkflowAtomic
// opens a tx, INSERTs both, commits, on all three dialects. So "the child exists
// but the event does not" is not a window crash timing can open, and the parent
// starting a second child is the consequence rather than the cause.
//
// WHAT THIS DOES NOT COVER, said plainly: it does not fix the defect and cannot.
// Nothing here stops the duplicate child; it makes the next occurrence arrive as
// a named condition with the run IDs attached, instead of as "the child ran 2
// times" in a ports test several layers away.
func TestAnOrphanChildNamesItself(t *testing.T) {
	ctx := context.Background()

	newSession := func(store ChildWorkflowStore, history []EventRecord) *execSession {
		rec := &orphanLogs{}
		return &execSession{
			workflowID: "parent-1",
			engine:     &Engine{childWfStore: store, logger: rec.logger()},
			history:    history,
		}
	}

	childEvent := func(runID string) EventRecord {
		return EventRecord{EventType: EventTypeChildWorkflow, RunID: runID}
	}

	t.Run("a started child missing from history is reported", func(t *testing.T) {
		store := &orphanLister{started: []string{"child-a"}}
		rec := &orphanLogs{}
		s := newSession(store, nil)
		s.engine.logger = rec.logger()

		s.reportOrphanChildren(ctx, "parent-1")

		if store.asked != 1 {
			t.Fatalf("the store was asked %d times, want 1", store.asked)
		}
		if !rec.sawOrphanReport() {
			t.Errorf("a child was started and appears in no child_workflow event, and nothing " +
				"was reported.\n\nThat is cleat#1661: the parent goes on to start a duplicate " +
				"and the only symptom is an assertion about child executions in a ports test.")
		}
	})

	// THE CONTROL, and it is the one that matters. Without it, "an orphan is
	// reported" is equally satisfied by a detector that reports on every child
	// start -- which would fire on every healthy fan-out in the fleet and be
	// switched off within a day.
	t.Run("a started child present in history is silent", func(t *testing.T) {
		store := &orphanLister{started: []string{"child-a"}}
		rec := &orphanLogs{}
		s := newSession(store, []EventRecord{childEvent("child-a")})
		s.engine.logger = rec.logger()

		s.reportOrphanChildren(ctx, "parent-1")

		if rec.sawOrphanReport() {
			t.Errorf("a child recorded in history was reported as an orphan; this detector would " +
				"fire on every healthy parent that starts a child")
		}
	})

	t.Run("a parent with no children is silent and cheap", func(t *testing.T) {
		store := &orphanLister{started: nil}
		rec := &orphanLogs{}
		s := newSession(store, nil)
		s.engine.logger = rec.logger()

		s.reportOrphanChildren(ctx, "parent-1")

		if rec.sawOrphanReport() {
			t.Errorf("a parent with no children reported an orphan")
		}
	})

	// A failed lookup must not speak. "No children found" and "I could not ask"
	// are the same empty slice, and a check that cannot tell them apart would
	// report an orphan every time the database hiccuped -- the same rule
	// reportShortReplayHistory follows for an unread event count.
	t.Run("a failed lookup says nothing", func(t *testing.T) {
		store := &orphanLister{started: []string{"child-a"}, err: errors.New("db down")}
		rec := &orphanLogs{}
		s := newSession(store, nil)
		s.engine.logger = rec.logger()

		s.reportOrphanChildren(ctx, "parent-1")

		if rec.sawOrphanReport() {
			t.Errorf("the lookup failed and an orphan was reported anyway")
		}
	})

	// Continued runs are excluded by the QUERY, not here, and this pins the
	// reason so nobody "simplifies" the WHERE clause away: continue-as-new
	// inherits parent_workflow_id and records no event, so a parent whose one
	// child continued three times would otherwise show three orphans.
	t.Run("the store, not this function, excludes continued runs", func(t *testing.T) {
		store := &orphanLister{started: []string{"child-a"}}
		rec := &orphanLogs{}
		s := newSession(store, []EventRecord{childEvent("child-a")})
		s.engine.logger = rec.logger()
		s.reportOrphanChildren(ctx, "parent-1")
		if rec.sawOrphanReport() {
			t.Errorf("unexpected report: OriginalChildRunIDs is contracted to return only " +
				"runs the parent STARTED, so a continued run must never reach this function")
		}
	})
}
