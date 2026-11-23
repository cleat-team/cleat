package engine

import "context"

// A cancel or a terminate is addressed to a WORKFLOW, and a workflow that continues as new is the same workflow to the one who cancels it (cleat#4340, the second half of
// what #4379 did for signals). The store acts on the run that is live, once, whatever continue-as-new did:
//
//   - a pre-emptive cancel or a terminate addressed to a run that has continued settles the END of its chain;
//   - a cooperative cancel addressed to one sets its flag on the END of its chain, and a flag already set on a run when it continues is CARRIED to the successor in the
//     continue's own transaction, because a successor created un-cancelled is exactly the cancel that was lost;
//   - the verb and a continue cannot miss each other, because they are ordered by ONE row, the old run's lease: the verb locks it before it decides (lockChainEnd), and the
//     continue's completion update on the same row waits for it. A cancel that wins settles the old run and BUMPS ITS GENERATION, so the continue's completion update, which is
//     fenced on that generation, finds no row and creates no successor; a continue that wins leaves a `done` run with a successor, which the verb walks to and settles.
//
// A chain whose end is already settled answers as it does today: ErrAdminStateConflict, naming the end.

// SettleReceipt says which run a cancel or a terminate acted on. A caller that does not care keeps using CancelWorkflow, TerminateWorkflow and RequestCancellation, which
// discard it.
type SettleReceipt struct {
	// SettledRun is the run the verb acted on: the addressed run, or the end of its chain when it had continued.
	SettledRun string
	// Forwarded is true when SettledRun is not the run the verb was addressed to.
	Forwarded bool
}

func settleReceiptFor(addressed, acted string) SettleReceipt {
	if acted == "" { // the verb failed before it resolved the chain
		acted = addressed
	}
	return SettleReceipt{SettledRun: acted, Forwarded: acted != addressed}
}

// SettleReceiptStore is implemented by a store that can say which run a cancel or a terminate acted on. It is optional, like SignalReceiptStore, so the verbs' own signatures
// (and every implementer of them) stay as they were.
type SettleReceiptStore interface {
	CancelWorkflowReceipt(ctx context.Context, workflowID, reason string) (SettleReceipt, error)
	TerminateWorkflowReceipt(ctx context.Context, workflowID, reason string) (SettleReceipt, error)
	RequestCancellationReceipt(ctx context.Context, workflowID, reason string) (SettleReceipt, error)
}

// settledRefusal is the error for a verb that found the END of the addressed run's chain already settled. For the addressed run itself the text is what it always was; for a run
// that continued it names the end, so the caller is not told "already settled" about a run that went on and is not the one that refused.
func settledRefusal(addressed, end, status, finalStatus string) error {
	if addressed == end {
		return adminErrorf(ErrAdminStateConflict,
			"workflow %s: already settled (status=%s); refusing to write %s over it",
			addressed, status, finalStatus)
	}
	return adminErrorf(ErrAdminStateConflict,
		"workflow %s: continued as new, and its current run %s is already settled (status=%s); refusing to write %s over it",
		addressed, end, status, finalStatus)
}
