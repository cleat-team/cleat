package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// A cache of WASM bytes has to be validated against something that changes
// when the bytes do (cleat#3516).
//
// WHY LENGTH IS NOT THAT. Worker.loadWASM validated its in-memory entry with
// GetWASMLength alone, so a replacement of the same size was served stale, and
// it returned a disk-cache entry without asking the store anything, so a
// replacement of any size was. cleat#3497 made a deliberate replacement of a
// version's bytes a supported act (`--force`), and on a worker that had loaded
// that version it did not take effect until the process restarted.
//
// WHAT THIS IS. The database computes a SHA-256 of the stored bytes and returns
// the 64 hex characters. The bytes can be ~19MB for a Python component, so they
// are not transferred to be compared, which is the same reason the replacement
// rule in workflow_def_replacement.go compares in the database.
//
// IT IS AN OPTIONAL INTERFACE, not a method on WorkflowStore. A new method there
// would break every implementation and every test double; a caller that finds
// the store does not implement this falls back to loading the bytes and hashing
// them, which is correct and costs a transfer.
//
// HASHING COSTS A READ OF THE WHOLE ROW, unlike GetWASMLength, which Postgres
// answers from the TOAST header. So callers should not ask on every task; see
// Worker.loadWASM, which asks on an interval and before it trusts the disk
// cache.

// WASMDigester is implemented by the stores that can hash a stored definition
// in the database.
type WASMDigester interface {
	// GetWASMDigest returns the lowercase hex SHA-256 of the stored WASM binary.
	// It is the same value as WASMDigest of the bytes LoadWASM returns.
	GetWASMDigest(ctx context.Context, defName string, defVersion int) (string, error)
}

// WASMDigest is the digest GetWASMDigest reports, computed locally.
func WASMDigest(wasmBytes []byte) string { return wasmCacheKey(wasmBytes) }

// GetWASMDigest returns the SHA-256 of the stored WASM binary, computed by
// PostgreSQL.
func (s *PostgresStore) GetWASMDigest(ctx context.Context, defName string, defVersion int) (string, error) {
	// An RLS transaction, for the reason GetWASMLength gives: on a bare
	// connection the tenant is unset and the policy cannot be evaluated.
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return "", fmt.Errorf("get wasm digest: begin: %w", err)
	}
	defer tx.Rollback()

	var digest sql.NullString
	var library sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT encode(sha256(wasm_bytes), 'hex'), library FROM workflow_defs WHERE name = $1 AND version = $2`,
		defName, defVersion).Scan(&digest, &library); err != nil {
		return "", err
	}
	if library.Valid {
		// A library member's digest is a stored column, checked against its bytes
		// when it was published, so asking for it hashes nothing. sha256 of the
		// definition's own NULL bytes, above, is NULL and unused.
		var memberDigest string
		if err := tx.QueryRowContext(ctx, `
			SELECT wasm_sha256 FROM library.members WHERE library = $1 AND name = $2 AND version = $3
		`, library.String, defName, defVersion).Scan(&memberDigest); err != nil {
			return "", libraryMemberReadError("get wasm digest", library.String, defName, defVersion, err)
		}
		return memberDigest, tx.Commit()
	}
	return digest.String, tx.Commit()
}

// GetWASMDigest returns the SHA-256 of the stored WASM binary, computed by
// MySQL.
func (s *MySQLStore) GetWASMDigest(ctx context.Context, defName string, defVersion int) (string, error) {
	var digest string
	// Scoped by tenant for the reason GetWASMLength gives: MySQL has no
	// row-level security, so this predicate is the whole of the isolation.
	err := s.db.QueryRowContext(ctx,
		`SELECT SHA2(wasm_bytes, 256) FROM workflow_defs WHERE name = ? AND version = ? AND tenant_id = ?`,
		defName, defVersion, s.tenantID).Scan(&digest)
	return digest, err
}

// GetWASMDigest returns the SHA-256 of the stored WASM binary, computed by SQL
// Server. HASHBYTES takes more than 8000 bytes only from SQL Server 2016.
func (s *MSSQLStore) GetWASMDigest(ctx context.Context, defName string, defVersion int) (string, error) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return "", fmt.Errorf("get wasm digest: begin: %w", err)
	}
	defer tx.Rollback()

	var digest sql.NullString
	var library sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT LOWER(CONVERT(varchar(64), HASHBYTES('SHA2_256', wasm_bytes), 2)), library FROM workflow_defs WHERE name = @p1 AND version = @p2 AND tenant_id = @p3`,
		defName, defVersion, s.tenantID).Scan(&digest, &library)
	if err != nil {
		return "", err
	}
	if library.Valid {
		// Stored at publish and checked against the bytes there: nothing is hashed.
		var memberDigest string
		if err := tx.QueryRowContext(ctx,
			`SELECT wasm_sha256 FROM library.members WHERE library = @p1 AND name = @p2 AND version = @p3`,
			library.String, defName, defVersion).Scan(&memberDigest); err != nil {
			return "", libraryMemberReadError("get wasm digest", library.String, defName, defVersion, err)
		}
		return memberDigest, nil
	}
	return digest.String, nil
}

// GetWASMDigest asks each shard in turn, like GetWASMLength.
func (s *ShardedStore) GetWASMDigest(ctx context.Context, defName string, defVersion int) (string, error) {
	lastErr := errors.New("no shard can hash a stored WASM binary")
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	for _, shard := range shards {
		d, ok := shard.Store.(WASMDigester)
		if !ok {
			continue
		}
		digest, err := d.GetWASMDigest(ctx, defName, defVersion)
		if err == nil {
			return digest, nil
		}
		lastErr = err
	}
	return "", lastErr
}
