package engine

import (
	"context"
	"fmt"
	"time"
)

// PayloadEncryptionState is cleat#2324's fix for a keyless worker silently
// treating sealed event history as plaintext.
//
// engine/encryption.go documents, deliberately, that a sealed column carries
// no envelope or version prefix -- see PayloadEncryption's doc comment,
// "The transition needs no envelope", for why one would be strictly worse.
// So a worker with no key ring configured has no way to recognise ciphertext
// by INSPECTING a row. This interface answers a different question instead,
// one that does not require looking at the data at all: has ANY worker on
// this database ever had a key ring configured. A keyless worker checks this
// once at startup and refuses to proceed if the answer is yes, rather than
// reading sealed columns as if they were plain data.
//
// Implemented only by *PostgresStore and *ShardedStore (which delegates to
// its PostgreSQL shards): --encrypt-sensitive-payloads already refuses every
// driver but postgres (cmd/cleat-worker/main.go), and cleat's sharding is
// postgres-only end to end -- every shard opens with
// sql.Open("postgres", ...), see the shard-building loop in
// cmd/cleat-worker/main.go. *MySQLStore and *MSSQLStore can never hold a
// sealed row and do not implement this.
type PayloadEncryptionState interface {
	// MarkPayloadEncryptionEnabled records that this database has a key ring
	// configured. Idempotent: call it once per worker startup when
	// --encrypt-sensitive-payloads is set, BEFORE the first write rather
	// than on first successful seal -- see the migration's own doc comment
	// for why a security check should err toward refusing a deploy that
	// turns out to be safe, rather than missing the window before the
	// first write.
	MarkPayloadEncryptionEnabled(ctx context.Context) error

	// PayloadEncryptionEverEnabled reports whether any worker has ever
	// marked this database via MarkPayloadEncryptionEnabled.
	PayloadEncryptionEverEnabled(ctx context.Context) (bool, error)
}

// MarkPayloadEncryptionEnabled implements PayloadEncryptionState.
func (s *PostgresStore) MarkPayloadEncryptionEnabled(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO payload_encryption_ever_enabled (singleton, enabled_at) VALUES (true, $1)
		 ON CONFLICT (singleton) DO NOTHING`,
		time.Now())
	if err != nil {
		return fmt.Errorf("mark payload encryption enabled: %w", err)
	}
	return nil
}

// PayloadEncryptionEverEnabled implements PayloadEncryptionState.
func (s *PostgresStore) PayloadEncryptionEverEnabled(ctx context.Context) (bool, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM payload_encryption_ever_enabled)`,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("check payload encryption ever enabled: %w", err)
	}
	return exists, nil
}

var _ PayloadEncryptionState = (*PostgresStore)(nil)
