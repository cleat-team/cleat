package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// maxContinueAsNewChainWalk bounds GetTerminalRun.
//
// A cycle is impossible by construction -- continued_from is written once, at
// INSERT, to the id of a row that already exists, and every continuation gets a
// fresh id -- so this cannot fire on data this engine wrote. It exists because
// "cannot happen" and "will not hang the API server" are different claims, and
// only the second one is worth having in a request path. A corrupted or
// hand-edited row is the case it covers.
//
// The number is deliberately far above any real chain: a workflow continuing
// once a second for a day is 86,400 runs.
const maxContinueAsNewChainWalk = 1_000_000

// ErrContinueAsNewChainTooLong is returned when a chain walk exceeds
// maxContinueAsNewChainWalk hops, which means continued_from does not describe
// a finite chain.
var ErrContinueAsNewChainTooLong = errors.New("continue-as-new chain did not terminate")

// walkToTerminalRun follows a ContinueAsNew chain forward from id and returns
// the last run in it.
//
// The walk lives here, once, rather than three times. Only successorOf differs
// per dialect, and it is one indexed lookup -- which is the whole reason this
// file exists: engine/store_lifecycle.go, engine/mysql_lifecycle.go and
// engine/mssql_lifecycle.go already carry three separately hand-written INSERTs
// for the same column (cleat#826), and that is enough duplication for one
// feature. Everything that could differ between dialects here -- the bound, the
// cycle report, what "not found" means -- is decided in one place.
//
// successorOf reports the id of the run that continued FROM the given id, or
// "" when there is none, which is the terminal case. get reads the final row.
func walkToTerminalRun(
	ctx context.Context,
	id string,
	successorOf func(context.Context, string) (string, error),
	get func(context.Context, string) (*WorkflowInstance, error),
) (*WorkflowInstance, error) {
	// Read the head first, so an id naming nothing returns nil, nil rather
	// than walking from an id that does not exist and reporting the same
	// answer a real terminal run would.
	head, err := get(ctx, id)
	if err != nil {
		return nil, err
	}
	if head == nil {
		return nil, nil
	}

	cur := id
	for hops := 0; ; hops++ {
		if hops >= maxContinueAsNewChainWalk {
			return nil, fmt.Errorf("%w: gave up at %s after %d hops",
				ErrContinueAsNewChainTooLong, cur, hops)
		}
		next, err := successorOf(ctx, cur)
		if err != nil {
			return nil, err
		}
		if next == "" {
			break
		}
		cur = next
	}

	if cur == id {
		return head, nil
	}
	return get(ctx, cur)
}

// successorScan turns the one-row-or-none result of a successor lookup into
// the ("" means terminal) convention walkToTerminalRun expects.
func successorScan(row *sql.Row) (string, error) {
	var next string
	switch err := row.Scan(&next); {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("continue-as-new successor lookup: %w", err)
	}
	return next, nil
}

// GetTerminalRun follows a ContinueAsNew chain forward from id. See
// WorkflowStore.
func (s *PostgresStore) GetTerminalRun(ctx context.Context, id string) (*WorkflowInstance, error) {
	return walkToTerminalRun(ctx, id, func(ctx context.Context, cur string) (string, error) {
		return successorScan(s.db.QueryRowContext(ctx,
			`SELECT id FROM workflow_instances WHERE continued_from = $1`, cur))
	}, s.GetWorkflowByID)
}

// GetTerminalRun follows a ContinueAsNew chain forward from id. See
// WorkflowStore.
func (s *MySQLStore) GetTerminalRun(ctx context.Context, id string) (*WorkflowInstance, error) {
	return walkToTerminalRun(ctx, id, func(ctx context.Context, cur string) (string, error) {
		return successorScan(s.db.QueryRowContext(ctx,
			`SELECT id FROM workflow_instances WHERE continued_from = ? AND tenant_id = ?`,
			cur, s.tenantID))
	}, s.GetWorkflowByID)
}

// GetTerminalRun follows a ContinueAsNew chain forward from id. See
// WorkflowStore.
func (s *MSSQLStore) GetTerminalRun(ctx context.Context, id string) (*WorkflowInstance, error) {
	return walkToTerminalRun(ctx, id, func(ctx context.Context, cur string) (string, error) {
		return successorScan(s.db.QueryRowContext(ctx,
			`SELECT id FROM workflow_instances WHERE continued_from = @p1 AND tenant_id = @p2`,
			cur, s.tenantID))
	}, s.GetWorkflowByID)
}
