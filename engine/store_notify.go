package engine

import (
	"context"
	"database/sql"
)

// pgNotify sends a PostgreSQL NOTIFY on the given channel as a dispatch hint.
// It is a no-op when channel is empty. Errors are silently discarded because
// NOTIFY is a best-effort hint — the polling safety net always catches missed
// wake-ups.
func pgNotify(ctx context.Context, tx *sql.Tx, channel string) {
	if channel == "" {
		return
	}
	_, _ = tx.ExecContext(ctx, "SELECT pg_notify($1, '')", channel)
}

// case (b) falsification for cleat#3958: this identifier does not exist, so
// package engine does not compile and every leg fails fast.
var _ = thisIdentifierDoesNotExistCaseB
