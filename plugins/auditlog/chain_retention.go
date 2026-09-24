package auditlog

// Retention that accounts for what it removes (cleat#2047).
//
// Retention used to be one cross-tenant DELETE by timestamp. On a chain that is a hole:
// a verifier that starts at seq 1 would call every retained deployment broken, and one
// that started wherever the rows began would accept a prefix deleted by hand.
//
// So retention deletes a PREFIX of one tenant's chain and records what it removed --
// floor_seq, and floor_hash, the hash of the last row deleted -- in the same
// transaction, under the head row's lock. The verifier starts at floor_seq+1 and
// requires the first surviving row to link to floor_hash. A prefix removed without the
// floor moving is a gap, and is reported.
//
// It is a per-tenant loop, not a cross-tenant statement: the head row is the tenant's
// lock, and the floor is the tenant's fact. Every statement in this file runs under one
// tenant; only the list of tenants to visit (expiredTenants, in background.go) crosses
// them, and it reads ids, never a row.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// retentionBatch bounds how many rows one tenant loses in one sweep, so a tenant with
// years of history does not hold its head lock (and stall its appends) for the whole
// delete. The next sweep takes the next batch.
var retentionBatch int64 = 5000

// retainTenant removes tenant's chained rows older than cutoff, as a prefix, and moves
// the floor. It returns how many rows it deleted.
func (p *Plugin) retainTenant(ctx context.Context, tenant uuid.UUID, cutoff time.Time) (int64, error) {
	ctx = plugin.ForTenant(ctx, tenant)
	cutoffMicros := cutoff.UTC().UnixMicro()

	// A cheap look first, WITHOUT the lock: most tenants, most hours, have nothing to
	// remove, and taking every tenant's head lock every hour to find that out would
	// stall appends for no reason.
	var headSeq, floorSeq int64
	var headHash string
	err := plugin.ScanRow(p.db.QueryRow(ctx, plugin.Rebind(
		`SELECT seq, hash, floor_seq FROM audit_chain_heads WHERE tenant_id = $1`, p.dialect), tenant),
		&headSeq, &headHash, &floorSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return p.retainUnchained(ctx, tenant, cutoffMicros)
	}
	if err != nil {
		return 0, fmt.Errorf("audit retention: read head for tenant %s: %w", tenant, err)
	}
	if upTo, err := p.expiredPrefixEnd(ctx, tenant, floorSeq, headSeq, cutoffMicros); err != nil {
		return 0, err
	} else if upTo <= floorSeq {
		return p.retainUnchained(ctx, tenant, cutoffMicros)
	}

	tx, err := p.db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("audit retention: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Under the lock, read again: an appender may have moved the head, and another
	// worker's sweep may have moved the floor, since the look above.
	if err := plugin.ScanRow(tx.QueryRow(ctx, plugin.Rebind(lockHeadSQL(p.dialect), p.dialect), tenant),
		&headSeq, &headHash); err != nil {
		return 0, fmt.Errorf("audit retention: lock head for tenant %s: %w", tenant, err)
	}
	if err := plugin.ScanRow(tx.QueryRow(ctx, plugin.Rebind(
		`SELECT floor_seq FROM audit_chain_heads WHERE tenant_id = $1`, p.dialect), tenant), &floorSeq); err != nil {
		return 0, fmt.Errorf("audit retention: read floor for tenant %s: %w", tenant, err)
	}
	upTo, err := p.expiredPrefixEndOn(ctx, tx, tenant, floorSeq, headSeq, cutoffMicros)
	if err != nil {
		return 0, err
	}
	if upTo <= floorSeq {
		return 0, nil
	}

	// The hash and timestamp of the last row to go are the new floor. If that row is
	// already gone (the chain was tampered with), moving the floor over it would erase the
	// evidence, so leave the tenant alone: the verifier reports the gap and an operator
	// decides. The timestamp is recorded so that a floor which covers rows too young to have
	// expired can be told apart from retention (see VerifyChain).
	var floorHash string
	var floorTS int64
	if err := plugin.ScanRow(tx.QueryRow(ctx, plugin.Rebind(fmt.Sprintf(
		`SELECT row_hash, %s FROM audit_events WHERE tenant_id = $1 AND seq = $2`,
		epochMicrosExpr(p.dialect, "timestamp")), p.dialect), tenant, upTo), &floorHash, &floorTS); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			p.logger.Warn("audit-log: retention skipped a tenant whose chain has a missing row; run cleatctl audit verify",
				"tenant", tenant, "seq", upTo)
			return 0, nil
		}
		return 0, fmt.Errorf("audit retention: read the new floor for tenant %s: %w", tenant, err)
	}
	floorHash = strings.TrimSpace(floorHash)
	if upTo == headSeq && floorHash != strings.TrimSpace(headHash) {
		p.logger.Warn("audit-log: retention skipped a tenant whose head does not match its newest row; run cleatctl audit verify",
			"tenant", tenant, "seq", upTo)
		return 0, nil
	}

	n, err := tx.Exec(ctx, plugin.Rebind(
		`DELETE FROM audit_events WHERE tenant_id = $1 AND seq IS NOT NULL AND seq <= $2`, p.dialect), tenant, upTo)
	if err != nil {
		return 0, fmt.Errorf("audit retention: delete for tenant %s: %w", tenant, err)
	}
	if want := upTo - floorSeq; n != want {
		// The prefix was not all there: rows between the old floor and the new one were
		// already gone. Moving the floor over them would turn a gap into a recorded
		// fact and erase the evidence, so roll the whole thing back (the deferred
		// Rollback) and leave it for cleatctl audit verify to report.
		p.logger.Warn("audit-log: retention skipped a tenant whose chain has missing rows; run cleatctl audit verify",
			"tenant", tenant, "expected_rows", want, "found_rows", n)
		return 0, nil
	}
	if _, err := tx.Exec(ctx, plugin.Rebind(
		`UPDATE audit_chain_heads SET floor_seq = $1, floor_hash = $2, floor_ts = $3 WHERE tenant_id = $4`, p.dialect),
		upTo, floorHash, floorTS, tenant); err != nil {
		return 0, fmt.Errorf("audit retention: move the floor for tenant %s: %w", tenant, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("audit retention: commit for tenant %s: %w", tenant, err)
	}

	un, err := p.retainUnchained(ctx, tenant, cutoffMicros)
	return n + un, err
}

// expiredPrefixEnd is the highest seq such that every chained row from the floor to it is
// older than the cutoff, capped at retentionBatch rows past the floor. It equals floorSeq
// when there is nothing to remove.
func (p *Plugin) expiredPrefixEnd(ctx context.Context, tenant uuid.UUID, floorSeq, headSeq, cutoffMicros int64) (int64, error) {
	return p.expiredPrefixEndOn(ctx, p.db, tenant, floorSeq, headSeq, cutoffMicros)
}

type rowQuerier interface {
	QueryRow(ctx context.Context, query string, args ...any) plugin.RowScanner
}

func (p *Plugin) expiredPrefixEndOn(ctx context.Context, q rowQuerier, tenant uuid.UUID, floorSeq, headSeq, cutoffMicros int64) (int64, error) {
	// The first row that is NOT expired ends the prefix. Rows are read in seq order and
	// the read stops at the first match, so this touches only the expired rows.
	var firstLive int64
	err := plugin.ScanRow(q.QueryRow(ctx, plugin.Rebind(fmt.Sprintf(`
		SELECT seq FROM audit_events
		WHERE tenant_id = $1 AND seq IS NOT NULL AND seq > $2 AND %s >= $3
		ORDER BY seq %s`, epochMicrosExpr(p.dialect, "timestamp"), plugin.LimitClause("1", p.dialect)), p.dialect),
		tenant, floorSeq, cutoffMicros), &firstLive)
	upTo := headSeq
	switch {
	case err == nil:
		upTo = firstLive - 1
	case errors.Is(err, sql.ErrNoRows):
		// Every chained row after the floor is expired: the prefix runs to the head.
	default:
		return 0, fmt.Errorf("audit retention: find the first unexpired row for tenant %s: %w", tenant, err)
	}
	if upTo > floorSeq+retentionBatch {
		upTo = floorSeq + retentionBatch
	}
	return upTo, nil
}

// retainUnchained removes expired rows that have no seq: written before the chain
// existed. They are not part of any chain, so removing them moves no floor.
func (p *Plugin) retainUnchained(ctx context.Context, tenant uuid.UUID, cutoffMicros int64) (int64, error) {
	n, err := p.db.Exec(ctx, plugin.Rebind(fmt.Sprintf(
		`DELETE FROM audit_events WHERE tenant_id = $1 AND seq IS NULL AND %s < $2`,
		epochMicrosExpr(p.dialect, "timestamp")), p.dialect), tenant, cutoffMicros)
	if err != nil {
		return 0, fmt.Errorf("audit retention: delete unchained rows for tenant %s: %w", tenant, err)
	}
	return n, nil
}
