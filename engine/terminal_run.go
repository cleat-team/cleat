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

	// The walk and the read are separate statements, and a chain moves between
	// them. cleat#4061: the run the walk settled on can continue as new before
	// its row is read, and a run that continues is finalised as `done` with an
	// empty result -- so the reply was an INTERMEDIATE run, status done, result
	// {}, that a client polling for a terminal status takes for the answer. The
	// real end was one or two rows further on. TestTypeScriptContinueAsNewOnARealWorker
	// saw it on SQL Server and then on MySQL.
	//
	// So the successor is asked again AFTER the row is read, and the walk goes on
	// if one has appeared. What this returns is then a row that had no successor
	// at a moment later than the moment it was read, which is the strongest
	// "last run as of now" a client can be given without one transaction across
	// the whole walk. It costs one extra successor lookup per call (a fan-out
	// per shard on a ShardedStore).
	cur, row := id, head
	for hops := 0; ; hops++ {
		if hops >= maxContinueAsNewChainWalk {
			return nil, fmt.Errorf("%w: gave up at %s after %d re-reads",
				ErrContinueAsNewChainTooLong, cur, hops)
		}
		end, err := terminalRunID(ctx, cur, successorOf)
		if err != nil {
			return nil, err
		}
		if end != cur {
			cur, row = end, nil
		}
		if row == nil {
			row, err = get(ctx, cur)
			if err != nil {
				return nil, err
			}
			if row == nil {
				return nil, nil
			}
		}
		next, err := successorOf(ctx, cur)
		if err != nil {
			return nil, err
		}
		if next == "" {
			return row, nil
		}
		// cur continued while its row was being read: that row is the run the
		// chain LEFT. Keep walking from its successor.
		cur, row = next, nil
	}
}

// readChildOutcomeFollowingTheChain is walkToTerminalRun for GetChildResult (cleat#4069).
//
// GetChildResult resolves the chain to its last run (cleat#955) and then reads that run's row in a SEPARATE statement, and the chain moves between the two: the run the walk settled on can
// continue as new before its row is read. A continued run is finalised `done` with an EMPTY result ("done because it was superseded, not because it finished"), so what that read returned was a
// settled outcome carrying `{}` for a child that had not finished, and AwaitChild records the outcome durably in the PARENT's history. #4061 was the same shape for GetTerminalRun, which hands a
// polling client a row it can poll again; this hands a workflow a wrong answer it cannot take back. Reproduced on PostgreSQL, MySQL and SQL Server (a_child_result_never_settles_on_a_run_the_chain_has_left_test.go).
//
// So the successor is asked for again AFTER the row is read, and the walk goes on if one has appeared, exactly as walkToTerminalRun does. A run that continues is marked done and its successor
// inserted in ONE transaction, so a read that sees the superseded `done` row is a read taken at or after the commit that created the successor, and the lookup that follows it (a later
// snapshot) cannot miss it. What is returned is the outcome of a run that had no successor at a moment later than the moment it was read: the strongest "last run as of now" without one
// transaction across the whole walk. It costs one extra successor lookup per call (a fan-out per shard on a ShardedStore).
//
// read returns the zero ChildOutcome for a run it cannot find, as every GetChildResult does, and that is returned as it is.
func readChildOutcomeFollowingTheChain(
	ctx context.Context,
	id string,
	successorOf func(context.Context, string) (string, error),
	read func(context.Context, string) (ChildOutcome, error),
) (ChildOutcome, error) {
	cur, err := terminalRunID(ctx, id, successorOf)
	if err != nil {
		return ChildOutcome{}, err
	}
	for hops := 0; ; hops++ {
		if hops >= maxContinueAsNewChainWalk {
			return ChildOutcome{}, fmt.Errorf("%w: gave up at %s after %d re-reads",
				ErrContinueAsNewChainTooLong, cur, hops)
		}
		out, err := read(ctx, cur)
		if err != nil {
			return ChildOutcome{}, err
		}
		next, err := successorOf(ctx, cur)
		if err != nil {
			return ChildOutcome{}, err
		}
		if next == "" {
			return out, nil
		}
		// cur continued while its row was being read: that outcome is the run the chain LEFT. Keep going from its successor.
		if cur, err = terminalRunID(ctx, next, successorOf); err != nil {
			return ChildOutcome{}, err
		}
	}
}

// terminalRunID walks a ContinueAsNew chain forward and returns the id of its
// last run, without reading any row but the ones it hops through.
//
// Split out of walkToTerminalRun for GetChildResult, which needs the id rather
// than the instance: it has its own per-dialect SELECT with result compaction
// and status handling, and re-pointing that at the terminal id changes WHICH
// row it reads without changing anything about how it reads it.
func terminalRunID(
	ctx context.Context,
	id string,
	successorOf func(context.Context, string) (string, error),
) (string, error) {
	cur := id
	for hops := 0; ; hops++ {
		if hops >= maxContinueAsNewChainWalk {
			return "", fmt.Errorf("%w: gave up at %s after %d hops",
				ErrContinueAsNewChainTooLong, cur, hops)
		}
		next, err := successorOf(ctx, cur)
		if err != nil {
			return "", err
		}
		if next == "" {
			return cur, nil
		}
		cur = next
	}
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

// runSuccessorFinder answers "which run continued from this one" for the rows
// a single store holds, WITHOUT first requiring that it hold the given run.
//
// That distinction is the whole reason this interface exists. GetTerminalRun
// begins by reading its head, and returns nil when the id names nothing it can
// see -- correct for a single store, and wrong for a shard. A chain is not
// shard-local: every continuation gets a fresh id and getShard hashes the id,
// so run N and run N+1 routinely live on different shards. The shard holding
// the SUCCESSOR does not hold the predecessor, so asking it GetTerminalRun(cur)
// gets nil back before it ever looks at continued_from.
//
// ShardedStore therefore asks this instead, which is a bare indexed lookup and
// has no opinion about who owns cur. Unexported: it is an implementation
// detail between ShardedStore and the concrete stores, and putting it on
// WorkflowStore would break every implementation of that interface to serve
// one caller.
type runSuccessorFinder interface {
	successorOfRun(ctx context.Context, id string) (string, error)
}

// successorOfRun on PostgreSQL needs BOTH the transaction and the predicate,
// and it had neither. cleat#1177.
//
// `s.db` is a plain *sql.DB, so this statement ran outside any transaction --
// and workflow_instances has RLS ENABLED and FORCED with a fail-closed policy,
// `USING (tenant_id = cleat.assert_tenant_set())`, where assert_tenant_set
// RAISEs when the setting is missing. The tenant is established with
// set_config(..., true) -- is_local -- so it exists only inside
// beginTxWithRLS. Outside one, every candidate row raises:
//
//	ERROR: cleat.tenant_id is not set
//
// WHY THAT SAT HERE UNNOTICED. A policy's USING is evaluated PER ROW, so a run
// that never continued as new has no candidate row, the assert is never
// reached, and the statement returns ("", nil) -- which is the right answer for
// that run. It fails only when there IS a successor to find, which is the only
// case this function exists to serve, on the chains cleat#826 is about.
//
// The MySQL and MSSQL arms below are correct BECAUSE they carry
// `AND tenant_id = ?`; neither depends on RLS. This one was written to lean on
// RLS and then handed a statement that never opens the transaction RLS needs.
// Both are added here rather than one: the predicate alone would work, and
// would leave the only PostgreSQL read of this table that is not tenant-scoped
// by the database as well as by the query.
func (s *PostgresStore) successorOfRun(ctx context.Context, id string) (string, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return "", fmt.Errorf("continue-as-new successor lookup: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	return successorScan(tx.QueryRowContext(ctx,
		`SELECT id FROM workflow_leases WHERE continued_from = $1 AND tenant_id = $2`,
		id, s.tenantID))
}

func (s *MySQLStore) successorOfRun(ctx context.Context, id string) (string, error) {
	return successorScan(s.db.QueryRowContext(ctx,
		`SELECT id FROM workflow_instances WHERE continued_from = ? AND tenant_id = ?`,
		id, s.tenantID))
}

func (s *MSSQLStore) successorOfRun(ctx context.Context, id string) (string, error) {
	// beginTxWithContext, not bare s.db -- cleat#2210: a WithTenant copy must
	// run under its own tenant's SESSION_CONTEXT, not whatever the pool's
	// original connection happened to carry.
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return "", fmt.Errorf("continue-as-new successor lookup: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	return successorScan(tx.QueryRowContext(ctx,
		`SELECT id FROM workflow_instances WHERE continued_from = @p1 AND tenant_id = @p2`,
		id, s.tenantID))
}

// GetTerminalRun follows a ContinueAsNew chain forward from id. See
// WorkflowStore.
func (s *PostgresStore) GetTerminalRun(ctx context.Context, id string) (*WorkflowInstance, error) {
	return walkToTerminalRun(ctx, id, s.successorOfRun, s.GetWorkflowByID)
}

// GetTerminalRun follows a ContinueAsNew chain forward from id. See
// WorkflowStore.
func (s *MySQLStore) GetTerminalRun(ctx context.Context, id string) (*WorkflowInstance, error) {
	return walkToTerminalRun(ctx, id, s.successorOfRun, s.GetWorkflowByID)
}

// GetTerminalRun follows a ContinueAsNew chain forward from id. See
// WorkflowStore.
func (s *MSSQLStore) GetTerminalRun(ctx context.Context, id string) (*WorkflowInstance, error) {
	return walkToTerminalRun(ctx, id, s.successorOfRun, s.GetWorkflowByID)
}
