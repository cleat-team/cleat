package engine

import (
	"context"
	"errors"
	"fmt"
)

// A signal is addressed to a WORKFLOW, and a workflow that continues as new is the same workflow to its sender (cleat#4340). The store delivers it to the run that is live, once, whatever
// continue-as-new did:
//
//   - a signal addressed to a run that has continued is stored on the END of its chain;
//   - a signal already stored on a run when it continues is CARRIED to the successor, in the continue's own transaction;
//   - the two cannot miss each other, because a delivery and a continue are ordered by ONE row, the old run's lease: a delivery locks it before it decides, and the continue's completion UPDATE of the
//     same row waits for any delivery that holds it (see lockChainEnd and each store's ContinueAsNew). Removing the delivery's lock lets signals strand, which the race test shows. A lock taken by the
//     continue BEFORE its other writes was tried and removed because no test needed it. THAT DOES NOT SHOW A DEADLOCK IS IMPOSSIBLE, and an earlier version of this comment said it was: CI
//     chose one SQL Server delivery as a deadlock victim in 96 (cleat#4340), at the statement that locks the run, and the cause was never found (not reproduced in about 100 local runs, including
//     under CPU starvation). So the SQL Server delivery is retried as a whole when it is the victim (MSSQLStore.DeliverSignalReceipt); whether the continue's lock-first would have prevented that
//     deadlock is not known, and nothing here claims it.
//
// Until #4343 a signal to a continued run was accepted and read by no one; #4343 refused it and named the successor. This is the answer the original design gave.

// SignalReceipt says where a delivered signal went. A caller that does not care keeps using DeliverSignal and DeliverSignalIdempotent, which discard it.
type SignalReceipt struct {
	// DeliveredTo is the run that now holds the signal: the addressed run, or the end of its chain when it had continued. It is empty for a Replay, because a replayed key delivers nothing.
	DeliveredTo string
	// Forwarded is true when DeliveredTo is not the run the signal was addressed to.
	Forwarded bool
	// Replay is true when an idempotency key had already delivered a signal, so this call delivered nothing.
	Replay bool
}

// SignalReceiptStore is implemented by a store that can say where a signal went. idempotencyKey may be empty, which is an unkeyed delivery.
type SignalReceiptStore interface {
	DeliverSignalReceipt(ctx context.Context, workflowID, signalName, payload, idempotencyKey string) (SignalReceipt, error)
}

// receiptFor builds the receipt for a delivery that wrote a signal.
func receiptFor(addressed, deliveredTo string) SignalReceipt {
	return SignalReceipt{DeliveredTo: deliveredTo, Forwarded: deliveredTo != addressed}
}

// lockChainEnd resolves the run a signal addressed to a run must be stored on, and HOLDS THE LOCK that orders the delivery against that run's continue-as-new.
//
// hint is the end of the chain as an unlocked read found it ("" when the addressed run had no successor). It is only where the walk starts: a continue can commit between that read and the lock, so
// every run is checked again under its lock. lockStatus takes the run's lease row for update and reports its status (found=false when the caller's tenant has no such run); successorInTx reads, in
// the delivery's own transaction, the run that continued from a given one.
//
// A run that is `done` and has a successor is a continued run: the walk moves to the successor and locks it. A run that is `done` with none really finished, and is returned as the end, so the
// gated insert that follows refuses it. Any other status is an open or redrivable run, and is the end.
//
// It locks every `done` run on the way, which costs nothing (nothing writes a done run's lease row) and keeps the rule one line long. Walkers lock in chain order, so two of them
// cannot deadlock on each other.
func lockChainEnd(
	addressed, hint string,
	lockStatus func(run string) (status string, found bool, err error),
	successorInTx func(run string) (string, error),
) (string, error) {
	cur := addressed
	if hint != "" {
		cur = hint
	}
	for hops := 0; hops < maxContinueAsNewChainWalk; hops++ {
		status, found, err := lockStatus(cur)
		if err != nil {
			return "", err
		}
		if !found || status != statusDone {
			return cur, nil
		}
		next, err := successorInTx(cur)
		if err != nil {
			return "", err
		}
		if next == "" {
			return cur, nil
		}
		cur = next
	}
	return "", fmt.Errorf("%w: gave up at %s", ErrContinueAsNewChainTooLong, cur)
}

// refusalFor reshapes a refusal from the gated insert, which names the END of the chain, into the error the caller is owed: it names the run the caller ADDRESSED, the end's state, and where the
// work went. Any other error passes through.
func refusalFor(err error, addressed, end string) error {
	var ne *WorkflowNotOpenError
	if !errors.As(err, &ne) {
		return err
	}
	cp := *ne
	cp.WorkflowID = addressed
	if end != addressed {
		cp.ContinuedTo = end
	}
	return &cp
}
