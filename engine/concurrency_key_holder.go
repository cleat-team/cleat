package engine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ConcurrencyKeyHolder is who holds a key and until when.
//
// Zero value means nobody holds it. Callers should test Held rather than
// comparing WorkflowID to "", so that a future holder with an empty id -- which
// nothing writes today -- cannot read as "free".
type ConcurrencyKeyHolder struct {
	WorkflowID string
	ExpiresAt  time.Time
	Held       bool
}

// GetConcurrencyKeyHolder answers "what is holding this key, and for how long".
//
// cleat#1172: a start refused for a concurrency-key conflict answers
//
//	409 {"error":"workflow already running with key K"}
//
// which names K -- the value the CALLER supplied -- and nothing else. The two
// questions an operator has at that moment are what holds it and for how long,
// and neither was answerable: not from the refusal, not by filtering the
// listing, and not by paging.
//
// # The issue's "cheap half" was not quite as cheap as it read
//
// It says the engine "already knows the holder -- it refuses because it found
// one", and that the handler "has the winning runID in hand at that moment".
// It does not. AcquireConcurrencyKey returns `(acquired bool, err error)`; on
// PostgreSQL the INSERT is `ON CONFLICT DO NOTHING RETURNING workflow_id`, so a
// conflict returns NO rows and the holder is never read. The runID the handler
// holds is the LOSER's -- StartNewRun created it a few lines earlier. So the
// holder has to be looked up, which is what this does.
//
// # Why this is not on the ConcurrencyKeyStore interface
//
// Fourteen test doubles implement that interface. Adding a method to it would
// break every one of them for a diagnostic they have no opinion about, and a
// change whose diff is mostly unrelated doubles is a change nobody reads. The
// HTTP layer asks for this through an optional interface assertion instead --
// the same shape CountRunnableWorkflows is reached by.
//
// # Dialects do not agree on where the hash is made
//
// PostgreSQL hashes in SQL (`digest($1, 'sha256')`, matching
// AcquireConcurrencyKey beside it); MySQL and SQL Server hash in Go. Each
// implementation below follows its own store's existing convention rather than
// a shared one, because the stored bytes were written that way -- migration
// 058's comment records what happens when a SQL-side hash meets a Go-side one
// on SQL Server.
func (s *PostgresStore) GetConcurrencyKeyHolder(ctx context.Context, key string) (ConcurrencyKeyHolder, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return ConcurrencyKeyHolder{}, fmt.Errorf("concurrency key holder: begin: %w", err)
	}
	defer tx.Rollback()

	var h ConcurrencyKeyHolder
	// expires_at > now() rather than a bare lookup: an expired row is not a
	// holder, and AcquireConcurrencyKey deletes such rows before inserting. A
	// refusal can only have come from a live one.
	err = tx.QueryRowContext(ctx, `
		SELECT workflow_id, expires_at FROM concurrency_keys
		WHERE key_hash = digest($1, 'sha256') AND tenant_id = $2 AND expires_at > now()
	`, key, s.tenantID).Scan(&h.WorkflowID, &h.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ConcurrencyKeyHolder{}, tx.Commit()
	}
	if err != nil {
		return ConcurrencyKeyHolder{}, fmt.Errorf("concurrency key holder: %w", err)
	}
	h.Held = true
	return h, tx.Commit()
}

// GetConcurrencyKeyHolder answers "what is holding this key, and for how long".
// See the PostgreSQL implementation for why this is not on the interface.
func (s *MySQLStore) GetConcurrencyKeyHolder(ctx context.Context, key string) (ConcurrencyKeyHolder, error) {
	hash := sha256.Sum256([]byte(key))
	var h ConcurrencyKeyHolder
	err := s.db.QueryRowContext(ctx, `
		SELECT workflow_id, expires_at FROM concurrency_keys
		WHERE key_hash = ? AND tenant_id = ? AND expires_at > NOW(6)
	`, hash[:], s.tenantID).Scan(&h.WorkflowID, &h.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ConcurrencyKeyHolder{}, nil
	}
	if err != nil {
		return ConcurrencyKeyHolder{}, fmt.Errorf("concurrency key holder: %w", err)
	}
	h.Held = true
	return h, nil
}

// GetConcurrencyKeyHolder answers "what is holding this key, and for how long".
// See the PostgreSQL implementation for why this is not on the interface.
func (s *MSSQLStore) GetConcurrencyKeyHolder(ctx context.Context, key string) (ConcurrencyKeyHolder, error) {
	keyHash := sha256.Sum256([]byte(key))
	var h ConcurrencyKeyHolder
	err := s.db.QueryRowContext(ctx, `
		SELECT workflow_id, expires_at FROM concurrency_keys
		WHERE key_hash = @p1 AND tenant_id = @p2 AND expires_at > SYSUTCDATETIME()
	`, keyHash[:], s.tenantID).Scan(&h.WorkflowID, &h.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ConcurrencyKeyHolder{}, nil
	}
	if err != nil {
		return ConcurrencyKeyHolder{}, fmt.Errorf("concurrency key holder: %w", err)
	}
	h.Held = true
	return h, nil
}

// claimedKeyTTL is a long safety bound on a concurrency key or queue holder,
// not what decides whether it is still held (cleat#1965).
//
// A slot or key is held for as long as its run is non-terminal --
// acquireCandidateConcurrencyKey's held-count queries and the candidate
// predicate (store_lifecycle.go and its MySQL/SQL Server twins) all join to
// workflow_instances and test its status, not this value. A run that parks
// (sleeps, or waits on a signal with no deadline) sends no heartbeat and has
// no wake time to renew from, so tying validity to elapsed time at all makes
// every long-lived run eventually stop counting against its own limit --
// that was this issue. See the design on cleat#1965 for why renewal was
// rejected too.
//
// What this value still bounds: ReapExpiredConcurrencyKeys deletes a row
// once its run goes terminal (or the row is orphaned -- its run's own
// workflow_instances row is gone, e.g. to retention), with no wait. This is
// the backstop for whatever that join misses -- a bug in the join, a bulk
// admin write that bypasses ReleaseWorkflowConcurrencyKeys, anything not yet
// found. Seven days costs nothing to carry and is long enough that hitting
// it at all is itself a signal something upstream is wrong.
const claimedKeyTTL = 7 * 24 * time.Hour
