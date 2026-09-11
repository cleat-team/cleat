package main

// cleat#1261 and cleat#1256.
//
// idempotency_keys.expires_at is written on every insert from the store's
// configured TTL, is NOT NULL, and has its own index. Nothing read it. Cleanup
// deleted by `created_at < now() - 7 days` instead -- a Go constant duplicating
// the schema default -- so WithIdempotencyKeyTTL changed nothing observable: a
// one-hour TTL still honoured a key for a week.
//
// And the loop ran only under `if *driver == "postgres"`, so on MySQL and SQL
// Server nothing removed a key at all. That is not merely unbounded growth: a
// key that is never removed is honoured forever, so the same client code got a
// fresh run on one dialect and `already_started` from an arbitrarily old run on
// another, with nothing in the API to say which.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// TestExpiresAtGovernsCleanupNotCreatedAt is deliberately free of sleeps: the
// row is aged by setting expires_at, so the assertion does not depend on how
// much wall-clock passes between two statements. (cleat#1244 is the standing
// reminder of what that dependency costs.)
func TestExpiresAtGovernsCleanupNotCreatedAt(t *testing.T) {
	db := testutil.SuiteTestDB(t, "cleat_worker")
	store := engine.NewPostgresStore(db)
	ctx := context.Background()

	const def = "idempotency-expiry"
	if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: def, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	start := func(key string) string {
		t.Helper()
		id, _, err := store.StartNewRun(ctx, "", def, 1, json.RawMessage(`{}`), key,
			engine.DefaultTenantUUID, 0)
		if err != nil {
			t.Fatalf("start %q: %v", key, err)
		}
		return id
	}
	// Counted through workflow_id rather than key_hash: the hash is computed
	// inside the store and this test has no business reimplementing it.
	keyRows := func(runID string) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM idempotency_keys WHERE workflow_id = $1`, runID).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	unique := time.Now().Format("150405.000000000")
	expiredID := start("expiry-governs-expired-" + unique)
	liveID := start("expiry-governs-live-" + unique)

	if keyRows(expiredID) != 1 || keyRows(liveID) != 1 {
		t.Fatalf("precondition: expected a key row for each run, got %d and %d",
			keyRows(expiredID), keyRows(liveID))
	}

	// Age one row by its EXPIRY only. created_at stays a few milliseconds old,
	// so the rule this replaces (created_at < now() - 7 days) would match
	// neither row -- which is exactly what makes this test discriminate.
	if _, err := db.ExecContext(ctx,
		`UPDATE idempotency_keys SET expires_at = now() - INTERVAL '1 second' WHERE workflow_id = $1`,
		expiredID); err != nil {
		t.Fatalf("age the expired row: %v", err)
	}

	var oldRuleWouldMatch int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM idempotency_keys
		  WHERE workflow_id IN ($1, $2) AND created_at < now() - INTERVAL '7 days'`,
		expiredID, liveID).Scan(&oldRuleWouldMatch); err != nil {
		t.Fatalf("old-rule probe: %v", err)
	}
	if oldRuleWouldMatch != 0 {
		t.Fatalf("precondition: the created_at rule matched %d of these rows, so this "+
			"test cannot tell the two rules apart", oldRuleWouldMatch)
	}

	stmt, ok := expiredIdempotencyKeysSQL("postgres")
	if !ok {
		t.Fatal("no cleanup statement for postgres")
	}
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	if n := keyRows(expiredID); n != 0 {
		t.Errorf("a key past its expires_at survived cleanup (%d rows). The configured "+
			"TTL is written to expires_at and must be what removes it (cleat#1261)", n)
	}
	if n := keyRows(liveID); n != 1 {
		t.Errorf("a key inside its TTL was deleted (%d rows left, want 1); cleanup must "+
			"not reach a key that is still meant to be honoured", n)
	}
}

// TestEveryWorkerDriverHasACleanupStatement is the guard for cleat#1256: the
// loop used to start only for postgres, leaving two dialects with nothing that
// ever removed a key. A dialect that the worker accepts but cleanup does not
// know about must fail here rather than silently accumulate keys forever.
func TestEveryWorkerDriverHasACleanupStatement(t *testing.T) {
	for _, driver := range []string{"postgres", "mysql", "mssql", "sqlserver"} {
		stmt, ok := expiredIdempotencyKeysSQL(driver)
		if !ok {
			t.Errorf("driver %q has no idempotency cleanup statement, so its keys are "+
				"never removed and are honoured forever (cleat#1256)", driver)
			continue
		}
		if !strings.Contains(stmt, "expires_at") {
			t.Errorf("driver %q cleans up by something other than expires_at: %q", driver, stmt)
		}
		if strings.Contains(stmt, "created_at") {
			t.Errorf("driver %q still keys cleanup on created_at: %q", driver, stmt)
		}
	}
	if _, ok := expiredIdempotencyKeysSQL("oracle"); ok {
		t.Error("an unknown driver reported a cleanup statement; the loop would run it")
	}
}
