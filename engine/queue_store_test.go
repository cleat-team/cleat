package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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

			if err := store.CreateQueue(ctx, tenantID, nameA, 3, nil, nil); err != nil {
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

			if err := store.CreateQueue(ctx, tenantID, nameB, 1, nil, nil); err != nil {
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

			if err := store.CreateQueue(ctx, tenantID, name, 3, nil, nil); err != nil {
				t.Fatalf("first CreateQueue: %v", err)
			}
			err := store.CreateQueue(ctx, tenantID, name, 5, nil, nil)
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

			if err := store.CreateQueue(ctx, tenantID, name, 3, nil, nil); err != nil {
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

func TestAQueueRateLimitRoundTripsThroughItsStore(t *testing.T) {
	for _, dialect := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMySQL, testutil.DialectMSSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			db := testutil.TestDB(t, dialect)
			testutil.SetupFullSchema(t, db, dialect)
			ctx := context.Background()

			tenantID := createQueueTestTenant(t, ctx, db, dialect, "qs-ratelimit")
			store := NewQueueStore(db, string(dialect))
			name := queueTestName("rate-payments")

			limit, period := 5, 60
			if err := store.CreateQueue(ctx, tenantID, name, 3, &limit, &period); err != nil {
				t.Fatalf("CreateQueue with a rate limit: %v", err)
			}

			got, err := store.GetQueue(ctx, tenantID, name)
			if err != nil {
				t.Fatalf("GetQueue: %v", err)
			}
			if got.RateLimit == nil || *got.RateLimit != limit {
				t.Errorf("RateLimit = %v, want %d", got.RateLimit, limit)
			}
			if got.RatePeriodSeconds == nil || *got.RatePeriodSeconds != period {
				t.Errorf("RatePeriodSeconds = %v, want %d", got.RatePeriodSeconds, period)
			}

			list, err := store.ListQueues(ctx, tenantID)
			if err != nil {
				t.Fatalf("ListQueues: %v", err)
			}
			var found bool
			for _, q := range list {
				if q.Name != name {
					continue
				}
				found = true
				if q.RateLimit == nil || *q.RateLimit != limit || q.RatePeriodSeconds == nil || *q.RatePeriodSeconds != period {
					t.Errorf("ListQueues rate limit for %s = (%v, %v), want (%d, %d)",
						name, q.RateLimit, q.RatePeriodSeconds, limit, period)
				}
			}
			if !found {
				t.Fatalf("ListQueues did not return %s", name)
			}
		})
	}
}

func TestAQueueWithNoRateLimitIsUnlimited(t *testing.T) {
	for _, dialect := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMySQL, testutil.DialectMSSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			db := testutil.TestDB(t, dialect)
			testutil.SetupFullSchema(t, db, dialect)
			ctx := context.Background()

			tenantID := createQueueTestTenant(t, ctx, db, dialect, "qs-norate")
			store := NewQueueStore(db, string(dialect))
			name := queueTestName("no-rate-payments")

			if err := store.CreateQueue(ctx, tenantID, name, 3, nil, nil); err != nil {
				t.Fatalf("CreateQueue: %v", err)
			}
			got, err := store.GetQueue(ctx, tenantID, name)
			if err != nil {
				t.Fatalf("GetQueue: %v", err)
			}
			if got.RateLimit != nil || got.RatePeriodSeconds != nil {
				t.Errorf("a queue created with no rate limit has RateLimit=%v RatePeriodSeconds=%v, want nil, nil",
					got.RateLimit, got.RatePeriodSeconds)
			}
		})
	}
}

// TestCreateQueueRefusesAnUnpairedRateLimit is the falsifiable half of
// validateRateLimitPair: giving one of rate_limit/rate_period_seconds
// without the other must be refused before any statement runs, not left to
// ck_queues_rate_limit_paired to catch as an opaque constraint violation.
//
// Asserts the ERROR TEXT, not just that an error occurred. A first version of
// this test checked only err != nil and stayed green with validateRateLimitPair's
// body replaced by `if false`, because ck_queues_rate_limit_paired still
// refused the write one layer down -- any error satisfies "got nil error,
// want a validation error". The whole point of validating in Go before the
// statement runs is a message that names which flag is missing (see
// CreateQueue's own doc comment); a test that cannot tell that message apart
// from the constraint's opaque one is not testing the thing it is named for.
func TestCreateQueueRefusesAnUnpairedRateLimit(t *testing.T) {
	const wantSubstr = "must both be set, or neither"
	for _, dialect := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMySQL, testutil.DialectMSSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			db := testutil.TestDB(t, dialect)
			testutil.SetupFullSchema(t, db, dialect)
			ctx := context.Background()

			tenantID := createQueueTestTenant(t, ctx, db, dialect, "qs-unpaired")
			store := NewQueueStore(db, string(dialect))

			limit := 5
			err := store.CreateQueue(ctx, tenantID, queueTestName("unpaired-a"), 1, &limit, nil)
			if err == nil || !strings.Contains(err.Error(), wantSubstr) {
				t.Errorf("CreateQueue with rate_limit set and rate_period_seconds nil: got %v, want an error containing %q", err, wantSubstr)
			}

			period := 60
			err = store.CreateQueue(ctx, tenantID, queueTestName("unpaired-b"), 1, nil, &period)
			if err == nil || !strings.Contains(err.Error(), wantSubstr) {
				t.Errorf("CreateQueue with rate_period_seconds set and rate_limit nil: got %v, want an error containing %q", err, wantSubstr)
			}
		})
	}
}

func TestSetQueueRateLimitSetsAndClears(t *testing.T) {
	for _, dialect := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMySQL, testutil.DialectMSSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			db := testutil.TestDB(t, dialect)
			testutil.SetupFullSchema(t, db, dialect)
			ctx := context.Background()

			tenantID := createQueueTestTenant(t, ctx, db, dialect, "qs-setratelimit")
			store := NewQueueStore(db, string(dialect))
			name := queueTestName("set-rate-payments")

			// Created with no rate limit, then set, then cleared -- the full
			// round trip an operator's `queue create` then `queue update`
			// then `queue update --clear-rate-limit` goes through.
			if err := store.CreateQueue(ctx, tenantID, name, 2, nil, nil); err != nil {
				t.Fatalf("CreateQueue: %v", err)
			}

			limit, period := 10, 30
			if err := store.SetQueueRateLimit(ctx, tenantID, name, &limit, &period); err != nil {
				t.Fatalf("SetQueueRateLimit: %v", err)
			}
			got, err := store.GetQueue(ctx, tenantID, name)
			if err != nil {
				t.Fatalf("GetQueue after set: %v", err)
			}
			if got.RateLimit == nil || *got.RateLimit != limit || got.RatePeriodSeconds == nil || *got.RatePeriodSeconds != period {
				t.Fatalf("after SetQueueRateLimit(%d, %d): got (%v, %v)", limit, period, got.RateLimit, got.RatePeriodSeconds)
			}
			// The concurrency limit must be untouched by a rate-limit-only
			// update -- SetQueueRateLimit has no business changing it.
			if got.ConcurrencyLimit != 2 {
				t.Errorf("ConcurrencyLimit changed by SetQueueRateLimit: got %d, want 2", got.ConcurrencyLimit)
			}

			if err := store.SetQueueRateLimit(ctx, tenantID, name, nil, nil); err != nil {
				t.Fatalf("SetQueueRateLimit (clear): %v", err)
			}
			got2, err := store.GetQueue(ctx, tenantID, name)
			if err != nil {
				t.Fatalf("GetQueue after clear: %v", err)
			}
			if got2.RateLimit != nil || got2.RatePeriodSeconds != nil {
				t.Errorf("after clearing: RateLimit=%v RatePeriodSeconds=%v, want nil, nil", got2.RateLimit, got2.RatePeriodSeconds)
			}
		})
	}
}

func TestSetQueueRateLimitOnAnUnregisteredQueueIsNotFound(t *testing.T) {
	for _, dialect := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMySQL, testutil.DialectMSSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			db := testutil.TestDB(t, dialect)
			testutil.SetupFullSchema(t, db, dialect)
			ctx := context.Background()

			tenantID := createQueueTestTenant(t, ctx, db, dialect, "qs-setratelimit-missing")
			store := NewQueueStore(db, string(dialect))

			limit, period := 5, 60
			err := store.SetQueueRateLimit(ctx, tenantID, queueTestName("never-registered"), &limit, &period)
			if !errors.Is(err, ErrQueueNotFound) {
				t.Fatalf("SetQueueRateLimit for an unregistered name: got %v, want ErrQueueNotFound", err)
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
