package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/google/uuid"
)

func TestValidQueueName(t *testing.T) {
	longName := make([]byte, 129)
	for i := range longName {
		longName[i] = 'a'
	}
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"", false},
		{"payments", true},
		{"payments-queue.v2_3", true},
		{"has a space", false},
		{"has/a/slash", false},
		{"has'a'quote", false},
		{string(longName), false},
	} {
		if got := validQueueName(tc.name); got != tc.want {
			t.Errorf("validQueueName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// queueTestName returns a queue name unique to this test process and call,
// via a nanosecond timestamp -- the same uniqueness idiom
// engine/claim_limit_invariant_test.go already uses for workflow ids. Needed
// on every dialect, not only MySQL: these tests run against a shared,
// possibly-reused test database (testutil.TestDB), and a bare "payments"
// reused across test functions or across repeated local runs would collide
// on CreateQueue's own uniqueness check.
func queueTestName(label string) string {
	return fmt.Sprintf("%s-%d", label, time.Now().UnixNano())
}

func TestAQueueRoundTripsThroughItsStore(t *testing.T) {
	for _, dialect := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMySQL, testutil.DialectMSSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			db := testutil.TestDB(t, dialect)
			testutil.SetupFullSchema(t, db, dialect)
			ctx := context.Background()

			tenantID := createQueueTestTenant(t, ctx, db, dialect, "qs-roundtrip")
			store := NewQueueStore(db, string(dialect))

			nameA := queueTestName("aaa-payments")
			nameB := queueTestName("zzz-emails")

			if err := store.CreateQueue(ctx, tenantID, nameA, 3); err != nil {
				t.Fatalf("CreateQueue: %v", err)
			}

			got, err := store.GetQueue(ctx, tenantID, nameA)
			if err != nil {
				t.Fatalf("GetQueue: %v", err)
			}
			if got.Name != nameA || got.ConcurrencyLimit != 3 {
				t.Errorf("GetQueue = %+v, want name=%s limit=3", got, nameA)
			}
			if got.DisabledAt != nil {
				t.Errorf("a freshly created queue has DisabledAt = %v, want nil", got.DisabledAt)
			}
			if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
				t.Errorf("CreatedAt/UpdatedAt were not populated: %+v", got)
			}

			if err := store.CreateQueue(ctx, tenantID, nameB, 1); err != nil {
				t.Fatalf("CreateQueue (second queue): %v", err)
			}

			list, err := store.ListQueues(ctx, tenantID)
			if err != nil {
				t.Fatalf("ListQueues: %v", err)
			}
			var indexA, indexB = -1, -1
			for i, q := range list {
				if q.Name == nameA {
					indexA = i
				}
				if q.Name == nameB {
					indexB = i
				}
			}
			if indexA == -1 || indexB == -1 {
				t.Fatalf("ListQueues did not return both registered queues: indexA=%d indexB=%d, list=%+v", indexA, indexB, list)
			}
			// ORDER BY name: nameA ("aaa-...") sorts before nameB ("zzz-...").
			// A shared MySQL tenant can carry rows from other tests too, so
			// this checks the two positions relative to each other rather than
			// asserting they are 0 and 1.
			if indexA > indexB {
				t.Errorf("ListQueues order: %s (index %d) sorts after %s (index %d), want before", nameA, indexA, nameB, indexB)
			}
		})
	}
}

func TestARegisteredQueueCannotBeRegisteredTwice(t *testing.T) {
	for _, dialect := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMySQL, testutil.DialectMSSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			db := testutil.TestDB(t, dialect)
			testutil.SetupFullSchema(t, db, dialect)
			ctx := context.Background()

			tenantID := createQueueTestTenant(t, ctx, db, dialect, "qs-dup")
			store := NewQueueStore(db, string(dialect))
			name := queueTestName("payments")

			if err := store.CreateQueue(ctx, tenantID, name, 3); err != nil {
				t.Fatalf("first CreateQueue: %v", err)
			}
			err := store.CreateQueue(ctx, tenantID, name, 5)
			if !errors.Is(err, ErrQueueAlreadyExists) {
				t.Fatalf("second CreateQueue for the same name: got %v, want ErrQueueAlreadyExists", err)
			}

			// The first registration's limit must survive the refused second
			// one -- CreateQueue must not have partially applied anything.
			got, err := store.GetQueue(ctx, tenantID, name)
			if err != nil {
				t.Fatalf("GetQueue: %v", err)
			}
			if got.ConcurrencyLimit != 3 {
				t.Errorf("ConcurrencyLimit = %d after a refused re-registration, want 3 (the first value, unchanged)", got.ConcurrencyLimit)
			}
		})
	}
}

func TestAnUnregisteredQueueNameIsNotFound(t *testing.T) {
	for _, dialect := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMySQL, testutil.DialectMSSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			db := testutil.TestDB(t, dialect)
			testutil.SetupFullSchema(t, db, dialect)
			ctx := context.Background()

			tenantID := createQueueTestTenant(t, ctx, db, dialect, "qs-missing")
			store := NewQueueStore(db, string(dialect))

			_, err := store.GetQueue(ctx, tenantID, queueTestName("never-registered"))
			if !errors.Is(err, ErrQueueNotFound) {
				t.Fatalf("GetQueue for an unregistered name: got %v, want ErrQueueNotFound", err)
			}
		})
	}
}

func TestDisablingAQueueIsIdempotent(t *testing.T) {
	for _, dialect := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMySQL, testutil.DialectMSSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			db := testutil.TestDB(t, dialect)
			testutil.SetupFullSchema(t, db, dialect)
			ctx := context.Background()

			tenantID := createQueueTestTenant(t, ctx, db, dialect, "qs-disable")
			store := NewQueueStore(db, string(dialect))
			name := queueTestName("payments")

			if err := store.CreateQueue(ctx, tenantID, name, 3); err != nil {
				t.Fatalf("CreateQueue: %v", err)
			}

			if err := store.DisableQueue(ctx, tenantID, name); err != nil {
				t.Fatalf("first DisableQueue: %v", err)
			}
			got, err := store.GetQueue(ctx, tenantID, name)
			if err != nil {
				t.Fatalf("GetQueue after disable: %v", err)
			}
			if got.DisabledAt == nil {
				t.Fatalf("DisabledAt is nil after DisableQueue")
			}
			// The row must still exist -- the soft-flag pattern, not deletion.
			if got.ConcurrencyLimit != 3 {
				t.Errorf("ConcurrencyLimit changed by disabling: got %d, want 3", got.ConcurrencyLimit)
			}
			firstDisabledAt := *got.DisabledAt

			// Disabling twice must succeed, per DisableQueue's own doc comment,
			// and must not move DisabledAt the second time -- otherwise a
			// retried disable call would keep resetting the retirement clock.
			if err := store.DisableQueue(ctx, tenantID, name); err != nil {
				t.Fatalf("second DisableQueue (must be idempotent): %v", err)
			}
			got2, err := store.GetQueue(ctx, tenantID, name)
			if err != nil {
				t.Fatalf("GetQueue after second disable: %v", err)
			}
			if got2.DisabledAt == nil || !got2.DisabledAt.Equal(firstDisabledAt) {
				t.Errorf("DisabledAt moved on a repeat disable: first=%v second=%v", firstDisabledAt, got2.DisabledAt)
			}
		})
	}
}

func TestDisablingAnUnregisteredQueueIsNotFound(t *testing.T) {
	for _, dialect := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMySQL, testutil.DialectMSSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			db := testutil.TestDB(t, dialect)
			testutil.SetupFullSchema(t, db, dialect)
			ctx := context.Background()

			tenantID := createQueueTestTenant(t, ctx, db, dialect, "qs-disable-missing")
			store := NewQueueStore(db, string(dialect))

			err := store.DisableQueue(ctx, tenantID, queueTestName("never-registered"))
			if !errors.Is(err, ErrQueueNotFound) {
				t.Fatalf("DisableQueue for an unregistered name: got %v, want ErrQueueNotFound", err)
			}
		})
	}
}

// createQueueTestTenant returns a tenant id these tests can scope against --
// required on PostgreSQL and SQL Server, whose queues table carries a foreign
// key to admin.tenants.
//
// MySQL is the exception: tenants carries a singleton unique constraint
// (tiers.yaml's documented single-tenant decision for that dialect), so a
// second INSERT there fails. Every MySQL subtest above therefore shares the
// one row every dialect already has -- the default tenant, backfilled by
// cleat#1898's migration -- rather than trying to create its own. queueTestName
// is what makes that safe: every queue name these tests register is unique to
// its own call, so sharing a tenant id across subtests and across repeated
// local runs never collides on (tenant_id, name).
func createQueueTestTenant(t *testing.T, ctx context.Context, db *sql.DB, dialect testutil.Dialect, label string) string {
	t.Helper()
	if dialect == testutil.DialectMySQL {
		return DefaultTenantUUID
	}

	tenantID := uuid.New().String()
	var stmt string
	switch dialect {
	case testutil.DialectMSSQL:
		stmt = `INSERT INTO admin.tenants (tenant_id, name) VALUES (@p1, @p2)`
	default:
		stmt = `INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)`
	}
	if _, err := db.ExecContext(ctx, stmt, tenantID, queueTestName(label)); err != nil {
		t.Fatalf("create test tenant: %v", err)
	}
	return tenantID
}
