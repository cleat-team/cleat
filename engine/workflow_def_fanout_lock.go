package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/lib/pq"
)

// One deploy of a (name, version) at a time across a sharded store (cleat#3547).
//
// WHY. ShardedStore.DeployWorkflowDef writes the definition to every shard in turn. Each shard's own
// write takes a row lock, which orders two deploys of one version WITHIN that shard and nothing across
// shards. Two deploys A and B of different bytes can interleave as A on the first shard, B on the first
// shard, B on the second, A on the second: the first shard ends holding B and the second holds A. Both
// deploys return nil and nothing detects it afterwards; a run replays its history under whichever
// shard's copy it is read from. On ONE store the same race is harmless, because replacement is
// specified behaviour (workflow_def_replacement.go) and the later deploy simply wins everywhere. The
// defect is only that "later" was decided per shard.
//
// THE FIX is to make it decided once. Every deployer of one (tenant, name, version) takes a lock on the
// designated shard (the one getShard(name) picks, which is also the first shard written) before the
// read-only pre-pass and holds it until the last shard is written. The fan-outs then queue and each
// completes before the next starts, so the shards agree. It does not forbid replacement, which a single
// store allows and its tests pin.
//
// THE LOCK IS TRANSACTION-SCOPED (pg_advisory_xact_lock), on purpose. It lives exactly as long as the
// transaction holding it, so it cannot outlive its holder: ending the transaction, a cancelled context
// (database/sql rolls a transaction back when the context it began with is cancelled) and a dead
// connection all release it. A session-scoped lock survives COMMIT and stays on a pooled connection
// that the next caller is handed, which is the failure cleat#3647 and cleat#3677 met. See also
// secret_key_gate.go, which has to use a session lock on MySQL and says what that costs.
//
// THE WAIT IS BOUNDED. A deployer queues behind the one in front of it for at most the wait below, and
// then fails with WorkflowDefDeployInProgressError, which says what holds it and that nothing was
// written. The bound is a measured fan-out with margin, not a guess: 19 MB of WASM (the size of a Python
// component, the largest the replacement rule's own comment cites) written to 3 PostgreSQL shards took
// 0.77-0.87 s on one machine over loopback, about 0.27 s a shard, five runs on 2026-11-08. 60 s is
// roughly seventy times that, or ten shards at ten times the per-shard cost.
//
// WHAT IT COSTS. Two connections on the designated shard's pool per deploy in flight on that
// name: one holds the lock, one does the write. A deployer waiting for the lock holds its connection
// while it waits, so a pool of N connections stalls if N deploys of one name are queued at once; the
// bound turns that into the error above rather than a hang. The worker's shard pools are 15.
//
// WHAT IT DOES NOT COVER, stated so nobody reads it as more than it is:
//   - A writer that does not go through ShardedStore. `cleat deploy --db` is one: it writes by raw SQL
//     to the single database it is given and, against a sharded deployment, reaches that shard only
//     (docs/sharding.md), so it neither takes this lock nor writes the others.
//   - Two processes configured with DIFFERENT shard lists. getShard hashes over the list, so they
//     would designate different shards and take different locks. The list is read once at startup from
//     --shards-file and ShardedStore never changes it (the only assignments are in NewShardedStore and
//     the per-tenant copy at sharded_store.go:237), so one process cannot move the lock's home between
//     two of its own deploys. A rolling restart onto a different list is a resharding, which routing
//     does not support either: runs live on the shard their id hashes to under the OLD list.
//   - A fan-out that fails part way still leaves the shards different until it is run again. That is
//     unchanged, and the error says so.

// defaultDefFanOutLockWait is how long a deploy queues for the lock; see above for where 60 s comes from.
const defaultDefFanOutLockWait = 60 * time.Second

// defFanOutLockNamespace is the first key of the two-key advisory lock (the bytes of "cldf"), so no
// other advisory lock in this schema can collide with a deploy's. The second key is a hash of
// (tenant, name, version); a hash collision between two names only makes them queue behind each other.
const defFanOutLockNamespace int32 = 0x636c6466

// ErrWorkflowDefDeployInProgress is matched by errors.Is when a deploy gave up waiting for the lock.
var ErrWorkflowDefDeployInProgress = errors.New("another deploy of this workflow version is in progress")

// WorkflowDefDeployInProgressError names what could not be taken and for how long it was waited for.
type WorkflowDefDeployInProgressError struct {
	Name    string
	Version int
	Waited  time.Duration
}

func (e *WorkflowDefDeployInProgressError) Error() string {
	return fmt.Sprintf("another deploy of workflow %q version %d holds the cross-shard deploy lock; waited %s for it. "+
		"Nothing was written. Run the deploy again once the other one has finished", e.Name, e.Version, e.Waited)
}

// Is reports a match against ErrWorkflowDefDeployInProgress.
func (e *WorkflowDefDeployInProgressError) Is(target error) bool {
	return target == ErrWorkflowDefDeployInProgress
}

// defFanOutLocker is implemented by a store that can hold the cross-shard deploy lock. PostgresStore
// does, and sharding is PostgreSQL-only in the worker (--shards-file opens "postgres" connections and
// PostgresStoreFactories). A multi-shard ShardedStore whose designated shard does not implement it
// refuses a deploy rather than running unordered, because an unprotected store would read as protected.
type defFanOutLocker interface {
	// lockWorkflowDefFanOut returns once it holds the lock, or fails with a
	// *WorkflowDefDeployInProgressError after waiting. The returned function releases it, is safe to
	// call more than once, and never blocks.
	lockWorkflowDefFanOut(ctx context.Context, name string, version int, wait time.Duration) (release func(), err error)
}

func (s *PostgresStore) lockWorkflowDefFanOut(ctx context.Context, name string, version int, wait time.Duration) (func(), error) {
	// Not beginTxWithRLS: the lock is on a number derived from the tenant, name and version, and reads
	// no tenant-scoped row, so there is no tenant context to set.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("cross-shard deploy lock: begin: %w", err)
	}
	ms := wait.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	// lock_timeout applies to an advisory lock wait. is_local (the third argument) is SET LOCAL: it ends
	// with the transaction, so nothing is left on the connection. A constant statement with the value
	// as a parameter, and not "SET LOCAL ... = " + a formatted number, because the tree-scanning guard
	// TestNoPostgresStatementReachesAnRLSTableWithoutTheTenantSet cannot read a statement built by
	// concatenation and fails on one it cannot resolve.
	if _, err := tx.ExecContext(ctx, `SELECT set_config('lock_timeout', $1, true)`, strconv.FormatInt(ms, 10)); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("cross-shard deploy lock: set lock_timeout: %w", err)
	}
	// Length-prefixed, not delimited: a name may contain any character a delimiter could be, and a
	// PostgreSQL text parameter may not contain NUL at all (22021).
	key := fmt.Sprintf("%d:%s|%d:%s|%d", len(s.tenantID), s.tenantID, len(name), name, version)
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1::int, hashtext($2::text))`, defFanOutLockNamespace, key); err != nil {
		_ = tx.Rollback()
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "55P03" { // lock_not_available: lock_timeout expired
			return nil, &WorkflowDefDeployInProgressError{Name: name, Version: version, Waited: wait}
		}
		return nil, fmt.Errorf("cross-shard deploy lock: %w", err)
	}
	return func() {
		// ErrTxDone is the normal answer when the context was cancelled first and database/sql has
		// already rolled the transaction back, which also released the lock.
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			// The transaction could not be ended, so the connection is in an unknown state and
			// database/sql will not pool it after a failed rollback. Nothing more to do here.
			return
		}
	}, nil
}
