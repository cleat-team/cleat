package engine

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/google/uuid"
)

// TestEngineStoresRejectAnExpiredAPIKey is cleat#2370.
//
// cleat#2352 (#2360) made auth.TenantStore.ResolveTenantFromAPIKey reject an
// expired key on all three dialects, and auth/a_key_expiry_test.go proves it.
// It never touched PostgresStore, MySQLStore or MSSQLStore here in engine --
// three SEPARATE implementations of the same method, queried by
// resolveAPIKeyStmt's near-twin in each of store_deployment.go, mysql_store.go
// and mssql_deployment.go. ShardedStore delegates to whichever of these a
// shard wraps and carries no SQL of its own, so fixing these three is also
// fixing it; there is no fourth statement to test.
//
// Before this fix, all three accepted a key with `expires_at` in the past: the
// WHERE clause checked only `disabled_at IS NULL`, exactly the auth.TenantStore
// bug #2360 fixed, reintroduced at three other call sites that share the same
// table and the same column but not the same statement. A host that wires one
// of these stores directly as its auth.TenantResolver -- rather than the
// enforcing auth.TenantStore -- got a non-enforcing resolver with no error, no
// log line and no test failure anywhere in the tree.
//
// Falsified by hand while writing this: reverting the three WHERE clauses back
// to `disabled_at IS NULL` alone turns the expired-key assertion red on all
// three dialects (the key resolves and no error comes back), and the
// live-key assertion stays green -- confirming the test is checking the
// clause this fix added, not something else nearby.
//
// Real databases only, for the same reason auth/a_key_expiry_test.go gives:
// a fake driver that dispatches on a query-string substring cannot tell
// "expires_at > now()" apart from any other WHERE clause it happens to also
// match, so it would pass whether the clause were correct, absent or
// inverted.
func TestEngineStoresRejectAnExpiredAPIKey(t *testing.T) {
	type resolver interface {
		ResolveTenantFromAPIKey(ctx context.Context, keyHash []byte) (uuid.UUID, error)
	}

	cases := []struct {
		dialect  testutil.Dialect
		newStore func(db *sql.DB) resolver
	}{
		{testutil.DialectPostgres, func(db *sql.DB) resolver { return NewPostgresStore(db) }},
		{testutil.DialectMySQL, func(db *sql.DB) resolver { return NewMySQLStore(db) }},
		{testutil.DialectMSSQL, func(db *sql.DB) resolver { return NewMSSQLStore(db) }},
	}

	for _, tc := range cases {
		t.Run(string(tc.dialect), func(t *testing.T) {
			db := testutil.TestDB(t, tc.dialect)
			ctx := context.Background()
			store := tc.newStore(db)

			tenantID := createQueueTestTenant(t, ctx, db, tc.dialect, "expiry-"+string(tc.dialect))

			// Key material is per-run, same reason as
			// mssql_store_integration_test.go's ResolveTenantFromAPIKey test and
			// auth/a_key_expiry_test.go: tenant_api_keys is not covered by any
			// dialect's CleanupTestData, key_hash is not unique, and a fixed hash
			// would resolve whichever earlier run's row got there first.
			suffix := fmt.Sprintf("%s-%d", tc.dialect, time.Now().UnixNano())
			expiredHash := sha256Of("expiry-test-expired-" + suffix)
			liveHash := sha256Of("expiry-test-live-" + suffix)

			now := time.Now()
			seedExpiryTestAPIKey(t, ctx, db, tc.dialect, tenantID, expiredHash,
				sql.NullTime{Time: now.Add(-time.Hour), Valid: true})
			seedExpiryTestAPIKey(t, ctx, db, tc.dialect, tenantID, liveHash,
				sql.NullTime{Time: now.Add(time.Hour), Valid: true})

			if _, err := store.ResolveTenantFromAPIKey(ctx, expiredHash); err == nil {
				t.Error("expired key: ResolveTenantFromAPIKey returned no error, want one -- " +
					"cleat#2370: this store's WHERE clause does not check expires_at")
			}
			got, err := store.ResolveTenantFromAPIKey(ctx, liveHash)
			if err != nil {
				t.Errorf("future-expiry key: ResolveTenantFromAPIKey: %v, want success", err)
			} else if got.String() != tenantID {
				t.Errorf("future-expiry key resolved tenant %s, want %s", got, tenantID)
			}
		})
	}

	// ShardedStore has no SQL of its own (ResolveTenantFromAPIKey just polls
	// each shard's own store), so the fix above already covers it -- this
	// subtest pins that reasoning rather than re-deriving it, the same way
	// sharded_overclaim_db_test.go pins a fan-out property against real
	// shards rather than mocks. Two shards on the SAME database, the pattern
	// sharded_overclaim_db_test.go uses: what is exercised is the delegation,
	// not cross-database routing, and ShardedStore is Postgres-only in
	// production (every shard opened via PostgresStoreFactory).
	t.Run("sharded", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectPostgres)
		ctx := context.Background()
		tenantID := createQueueTestTenant(t, ctx, db, testutil.DialectPostgres, "expiry-sharded")

		suffix := fmt.Sprintf("sharded-%d", time.Now().UnixNano())
		expiredHash := sha256Of("expiry-test-expired-" + suffix)
		liveHash := sha256Of("expiry-test-live-" + suffix)
		now := time.Now()
		seedExpiryTestAPIKey(t, ctx, db, testutil.DialectPostgres, tenantID, expiredHash,
			sql.NullTime{Time: now.Add(-time.Hour), Valid: true})
		seedExpiryTestAPIKey(t, ctx, db, testutil.DialectPostgres, tenantID, liveHash,
			sql.NullTime{Time: now.Add(time.Hour), Valid: true})

		const shardCount = 2
		dsn := testutil.PostgresTestDSN()
		stores := make([]WorkflowStore, shardCount)
		configs := make([]ShardConfig, shardCount)
		closers := make([]func() error, shardCount)
		for i := 0; i < shardCount; i++ {
			shardDB, err := sql.Open("postgres", dsn)
			if err != nil {
				t.Fatalf("open shard %d: %v", i, err)
			}
			if err := shardDB.PingContext(ctx); err != nil {
				shardDB.Close()
				t.Fatalf("ping shard %d: %v", i, err)
			}
			t.Cleanup(func() { shardDB.Close() })
			stores[i] = NewPostgresStore(shardDB)
			configs[i] = ShardConfig{Name: fmt.Sprintf("shard%d", i)}
			closers[i] = func() error { return nil }
		}
		sharded, err := NewShardedStore(configs, stores, closers)
		if err != nil {
			t.Fatalf("NewShardedStore: %v", err)
		}

		if _, err := sharded.ResolveTenantFromAPIKey(ctx, expiredHash); err == nil {
			t.Error("expired key: ShardedStore.ResolveTenantFromAPIKey returned no error, want one")
		}
		got, err := sharded.ResolveTenantFromAPIKey(ctx, liveHash)
		if err != nil {
			t.Errorf("future-expiry key: ShardedStore.ResolveTenantFromAPIKey: %v, want success", err)
		} else if got.String() != tenantID {
			t.Errorf("future-expiry key resolved tenant %s, want %s", got, tenantID)
		}
	})
}

// seedExpiryTestAPIKey inserts an API key directly against the dialect's own
// table name and column list -- tenant_api_keys has no exported Go-level
// constructor, same as auth/a_key_expiry_test.go's seedAPIKey, which this
// mirrors. MySQL alone needs a key_id (mysql_store.go's createAPIKeyStmt
// equivalent in auth/tenant_store.go); Postgres and SQL Server generate one.
func seedExpiryTestAPIKey(t *testing.T, ctx context.Context, db *sql.DB, dialect testutil.Dialect, tenantID string, keyHash []byte, expiresAt sql.NullTime) {
	t.Helper()

	var stmt string
	var args []any
	switch dialect {
	case testutil.DialectMySQL:
		stmt = `INSERT INTO tenant_api_keys (key_id, tenant_id, key_hash, description, expires_at) VALUES (?, ?, ?, ?, ?)`
		args = []any{uuid.New().String(), tenantID, keyHash, "cleat#2370 test key", expiresAt}
	case testutil.DialectMSSQL:
		stmt = `INSERT INTO admin.tenant_api_keys (tenant_id, key_hash, description, expires_at) VALUES (@p1, @p2, @p3, @p4)`
		args = []any{tenantID, keyHash, "cleat#2370 test key", expiresAt}
	default:
		stmt = `INSERT INTO admin.tenant_api_keys (tenant_id, key_hash, description, expires_at) VALUES ($1, $2, $3, $4)`
		args = []any{tenantID, keyHash, "cleat#2370 test key", expiresAt}
	}
	if _, err := db.ExecContext(ctx, stmt, args...); err != nil {
		t.Fatalf("seed api key (%s): %v", dialect, err)
	}

	cleanupTable := "tenant_api_keys"
	if dialect != testutil.DialectMySQL {
		cleanupTable = "admin.tenant_api_keys"
	}
	cleanupPlaceholder := "?"
	if dialect == testutil.DialectMSSQL {
		cleanupPlaceholder = "@p1"
	} else if dialect == testutil.DialectPostgres {
		cleanupPlaceholder = "$1"
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(),
			fmt.Sprintf("DELETE FROM %s WHERE key_hash = %s", cleanupTable, cleanupPlaceholder),
			keyHash); err != nil {
			t.Errorf("clean up api key (%s): %v", dialect, err)
		}
	})
}
