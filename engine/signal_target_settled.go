package engine

import (
	"context"
	"errors"
	"fmt"
)

// ErrWorkflowNotOpen is what delivering a signal to a run that has closed FOR GOOD fails with (cleat#4340).
//
// Until #4340 a signal sent to a run that was done, cancelled or had continued as new was accepted and stored under that run's own id, and read by no one: those runs can never be reopened, and a
// continuation is a different run with a different id. The sender was told "delivered". That is a signal lost silently, so it is refused instead, in the same statement that would have stored it, so
// nothing is written.
//
// WHICH STATUSES, AND WHY NOT ALL FIVE. A failed, terminated or dead-lettered run can be reopened under the SAME id by an operator (retry, re-replay: reReplayableStatuses), and the signals already
// stored for it stay in place, so "signal a dead-lettered run, then retry it" works today and the signal is read after the retry. That was found in review, after this error was first written to refuse
// all five, and the coordinator ruled that removing it is the owner's decision (cleat#4345), not this change's. So those three keep accepting and storing, and the HTTP reply says what happened to the
// signal instead of claiming delivery. The refused set is therefore exactly "settled and not redrivable": done and cancelled, plus a done run that continued as new (which is just done with a successor).
//
// It is distinct from ErrWorkflowNotFound on purpose and in only one direction. NotFound is identical for a run that does not exist and one under another tenant (cleat#2218, cleat#2227), so
// that nothing about another tenant's runs can be learned from it. NotOpen is returned only for a run the CALLER'S OWN tenant can see, so naming its state and its successor reveals nothing the
// caller could not already read with GetWorkflow. The delivery statement is tenant-scoped, so a foreign run never reaches this error.
var ErrWorkflowNotOpen = errors.New("workflow is no longer open")

// WorkflowNotOpenError carries what the caller needs to act: which run, what state, and where the work went.
type WorkflowNotOpenError struct {
	// WorkflowID is the run the signal was addressed to.
	WorkflowID string
	// Status is the closed-for-good status the run is in: done or cancelled.
	Status string
	// ContinuedTo is, when the run settled by CONTINUING AS NEW, the last run in its chain as of the moment of the refusal, which is the run a caller that wants to signal "the current one" should
	// use. It is empty for a run that really finished, and also when the chain could not be read, because it is advice and the refusal does not depend on it. It is the chain's END and not the
	// immediate successor, because a caller holding the first of a long chain would otherwise be refused once per link.
	ContinuedTo string
}

func (e *WorkflowNotOpenError) Error() string {
	if e.ContinuedTo != "" {
		return fmt.Sprintf("workflow %s is no longer open (status %s): it continued as new, the current run is %s", e.WorkflowID, e.Status, e.ContinuedTo)
	}
	return fmt.Sprintf("workflow %s is no longer open (status %s)", e.WorkflowID, e.Status)
}

// IsRedrivableStatus says whether an operator can reopen a run in this status under the same id, so a signal stored for it is read after the redrive. It reads the engine's own list,
// reReplayableStatuses (dead_lettered is also what retry accepts), so it cannot drift from what re-replay accepts. Exported because the HTTP reply qualifies "stored" for exactly these runs.
func IsRedrivableStatus(status string) bool {
	for _, s := range reReplayableStatuses {
		if s == status {
			return true
		}
	}
	return false
}

// closedForGood is "settled and not redrivable": the statuses whose stored signals no run will ever read.
func closedForGood(status string) bool { return isSettledStatus(status) && !IsRedrivableStatus(status) }

// Is makes errors.Is(err, ErrWorkflowNotOpen) true for every WorkflowNotOpenError.
func (e *WorkflowNotOpenError) Is(target error) bool { return target == ErrWorkflowNotOpen }

// signalRefusal says WHY a delivery statement wrote no row, given the status of the target as the caller's tenant sees it ("" and found=false when there is no such row). A run that has closed for good
// is NotOpen; anything else (no row, or a row that is not closed by the time it is read, which a revive can cause) is NotFound, as it was before.
func signalRefusal(workflowID, status string, found bool) error {
	if found && closedForGood(status) {
		return &WorkflowNotOpenError{WorkflowID: workflowID, Status: status}
	}
	return ErrWorkflowNotFound
}

// withContinuedTo adds the chain's current end to a NotOpen error for a run that is `done`, which is how a continuation leaves its predecessor. successorOf is the store's own lookup, or the
// cross-shard one on a ShardedStore. Any other error, and a refusal whose chain cannot be read, passes through unchanged: the end of the chain is advice, never a condition.
func withContinuedTo(ctx context.Context, err error, successorOf func(context.Context, string) (string, error)) error {
	var ne *WorkflowNotOpenError
	if !errors.As(err, &ne) || ne.Status != statusDone {
		return err
	}
	end, serr := latestRunInChain(ctx, ne.WorkflowID, successorOf)
	if serr != nil {
		return err
	}
	cp := *ne
	cp.ContinuedTo = end
	return &cp
}
