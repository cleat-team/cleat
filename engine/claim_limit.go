package engine

import (
	"context"
	"log/slog"
)

// enforceClaimLimit checks the one invariant every claim path must hold: a
// claim for n never returns more than n workflows.
//
// Why this exists. IMPROVEMENT-PLAN.md 2.11 records a claim for 3 that
// returned 10, with all ten rows left `running`, against a plain
// PostgresStore. The suspected mechanism (an EvalPlanQual recheck
// re-executing the candidate sublink) was investigated and **ruled out** --
// 24,000 claims under concurrent disruption never exceeded the limit -- so
// the cause is still unknown. The plan's instruction for that state is
// explicit: "If it recurs, capture the statement and its plan rather than
// reasoning from the row count."
//
// Nothing captured anything. An over-claim was indistinguishable from a
// normal claim, and the caller simply received more workflows than it asked
// for. That is also the 2.17 shape: rows updated to `running` with
// `assigned_to` set, beyond what the caller will execute, stranded until
// their lease expires.
//
// So on violation this does two things. It logs at ERROR with everything
// needed to identify the claim, and it hands the excess back to the caller to
// release rather than silently truncating -- truncation is what made 2.17 a
// bug rather than a nuisance.
//
// This is a backstop for a defect believed fixed, not a fix. If it ever
// fires, the log line is the evidence 2.11 asked for and could not get.
func enforceClaimLimit(ctx context.Context, log *slog.Logger, dialect, workerID string, limit int, claimed []*WorkflowInstance) (keep, excess []*WorkflowInstance) {
	if limit <= 0 || len(claimed) <= limit {
		return claimed, nil
	}

	keep, excess = claimed[:limit], claimed[limit:]

	excessIDs := make([]string, 0, len(excess))
	for _, wf := range excess {
		excessIDs = append(excessIDs, wf.ID)
	}
	if log != nil {
		log.ErrorContext(ctx, "claim returned more workflows than its limit -- see IMPROVEMENT-PLAN.md 2.11",
			"dialect", dialect,
			"worker_id", workerID,
			"limit", limit,
			"returned", len(claimed),
			"excess_workflow_ids", excessIDs,
		)
	}
	return keep, excess
}
