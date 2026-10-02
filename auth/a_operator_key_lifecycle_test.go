package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
)

// These two tests are the only place the operator statements are EXECUTED
// rather than asserted about. Everything else in this package checks the shape
// of the SQL -- which placeholder, which schema, which table -- and shape
// assertions cannot catch a column that does not exist, a projection the driver
// will not scan, or an INSERT whose column list and argument list disagree.
// Those fail at run time, on the dialect the author is not running, which is
// the failure mode createAPIKeyStmt's own comment describes for the tenant
// keys. So they run here, the same way a_key_expiry_test.go runs the tenant
// ones, on every dialect the Multi-DB CI job has a DSN for.
//
// They go through the STORE rather than seeding rows with raw SQL, which is the
// difference from the tenant tests: an operator key has no writer outside the
// store (cleatctl operator-key create calls it), so exercising the public
// methods covers both the INSERT and the read-back in one round trip.

// operatorHash is the hash ResolveOperatorFromAPIKey looks a key up by.
func operatorHash(raw string) []byte {
	h := sha256.Sum256([]byte(raw))
	return h[:]
}

func operatorStoreForTest(t *testing.T, dialect string) *OperatorStore {
	t.Helper()
	db := testutil.TestDB(t, testutil.Dialect(dialect))
	store, err := NewOperatorStoreForDialect(db, dialect)
	if err != nil {
		t.Fatalf("NewOperatorStoreForDialect(%q): %v", dialect, err)
	}
	return store
}

func TestAnOperatorKeyRoundTripsOnEveryDialect(t *testing.T) {
	for _, dialect := range []string{DialectPostgres, DialectMySQL, DialectMSSQL} {
		t.Run(dialect, func(t *testing.T) {
			store := operatorStoreForTest(t, dialect)
			ctx := context.Background()

			// GenerateOperatorKey() rather than a fixed literal: TestDB runs
			// against a shared, persistent database, so a fixed key would have a
			// fixed hash and a previous run's row could shadow this one -- the
			// same reasoning a_key_expiry_test.go records for GenerateAPIKey.
			raw := GenerateOperatorKey()
			const description = "round trip"
			op, err := store.CreateOperatorKey(ctx, description, raw, nil)
			if err != nil {
				t.Fatalf("CreateOperatorKey(%s): %v", dialect, err)
			}

			// key_id must be a real UUID on every dialect, and this is the
			// assertion the `key_id::text` / `CONVERT(NVARCHAR(36), key_id)`
			// projections exist for: SQL Server hands UNIQUEIDENTIFIER back in a
			// byte order the drivers do not scan directly, so a bare projection
			// would not produce a parseable value here.
			if _, err := uuid.Parse(op.KeyID); err != nil {
				t.Errorf("CreateOperatorKey returned key_id %q, which is not a UUID: %v", op.KeyID, err)
			}

			got, err := store.ResolveOperatorFromAPIKey(ctx, operatorHash(raw))
			if err != nil {
				t.Fatalf("ResolveOperatorFromAPIKey after CreateOperatorKey: %v", err)
			}
			if got.KeyID != op.KeyID {
				t.Errorf("the resolved key id is %q, want the created %q -- the INSERT wrote one row and the lookup found another",
					got.KeyID, op.KeyID)
			}
			if got.Description != description {
				t.Errorf("the resolved description is %q, want %q", got.Description, description)
			}

			// A credential that was never stored must not resolve. Without
			// this, the two assertions above would pass against a lookup that
			// matched anything at all.
			if _, err := store.ResolveOperatorFromAPIKey(ctx, operatorHash(GenerateOperatorKey())); err == nil {
				t.Error("a key that was never stored resolved")
			}

			if err := store.RevokeOperatorKey(ctx, op.KeyID); err != nil {
				t.Fatalf("RevokeOperatorKey: %v", err)
			}
			if _, err := store.ResolveOperatorFromAPIKey(ctx, operatorHash(raw)); err == nil {
				t.Error("a revoked operator key still resolved, so the lookup is not filtering on disabled_at")
			}
			// The second revoke is the idempotence claim, and it is worth
			// asserting rather than assuming: `AND disabled_at IS NULL` is what
			// makes a re-run safe, and without it the statement would move the
			// timestamp and rewrite when the key died.
			if err := store.RevokeOperatorKey(ctx, op.KeyID); !errors.Is(err, ErrOperatorKeyNotFound) {
				t.Errorf("a second revoke returned %v, want ErrOperatorKeyNotFound", err)
			}

			keys, err := store.ListOperatorKeys(ctx)
			if err != nil {
				t.Fatalf("ListOperatorKeys: %v", err)
			}
			var found *Operator
			for i := range keys {
				if keys[i].KeyID == op.KeyID {
					found = &keys[i]
					break
				}
			}
			if found == nil {
				t.Fatalf("the key this test just created is absent from ListOperatorKeys. A revoked key disappearing from the list is exactly what that method's comment says it does not do -- and it is the shape that makes a revoked credential indistinguishable from one that never existed.")
			}
			if !found.Disabled {
				t.Error("ListOperatorKeys reported the revoked key as live, so its disabled_at projection or its state mapping is wrong")
			}
		})
	}
}

func TestAnExpiredOperatorKeyCannotAuthenticate(t *testing.T) {
	for _, dialect := range []string{DialectPostgres, DialectMySQL, DialectMSSQL} {
		t.Run(dialect, func(t *testing.T) {
			store := operatorStoreForTest(t, dialect)
			ctx := context.Background()

			// Three keys, and the live two are why this test cannot pass by
			// refusing everything the way the round-trip test's "never stored"
			// case could.
			past := time.Now().Add(-time.Hour)
			expiredRaw := GenerateOperatorKey()
			if _, err := store.CreateOperatorKey(ctx, "expired", expiredRaw, &past); err != nil {
				t.Fatalf("CreateOperatorKey(expired): %v", err)
			}
			if _, err := store.ResolveOperatorFromAPIKey(ctx, operatorHash(expiredRaw)); err == nil {
				t.Error("an expired operator key resolved, so the lookup is not filtering on expires_at")
			}

			future := time.Now().Add(time.Hour)
			liveRaw := GenerateOperatorKey()
			if _, err := store.CreateOperatorKey(ctx, "live", liveRaw, &future); err != nil {
				t.Fatalf("CreateOperatorKey(live): %v", err)
			}
			if _, err := store.ResolveOperatorFromAPIKey(ctx, operatorHash(liveRaw)); err != nil {
				t.Errorf("a live, unexpired operator key did not resolve: %v", err)
			}

			// NULL expires_at means "no expiry" and not "unknown": it is what
			// every key created before the column existed holds, and any
			// permanent one since. The lookup's `expires_at IS NULL OR ...` is
			// the half that keeps those authenticating.
			neverRaw := GenerateOperatorKey()
			if _, err := store.CreateOperatorKey(ctx, "never expires", neverRaw, nil); err != nil {
				t.Fatalf("CreateOperatorKey(no expiry): %v", err)
			}
			if _, err := store.ResolveOperatorFromAPIKey(ctx, operatorHash(neverRaw)); err != nil {
				t.Errorf("an operator key with no expiry did not resolve: %v", err)
			}
		})
	}
}
