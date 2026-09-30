package engine

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"
	"unsafe"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/monitoring/prometheus"
)

// TestPostgresOpenIsolatedStoreMatchesOpenStore is cleat#2200 item 1's
// regression test.
//
// OpenIsolatedStore's own doc comment has called it "OpenStore's shape"
// since it was written, but until this fix it applied only tenantID,
// logger and syncCommitOff -- not encryption, idempotency TTL, the notify
// channel or metrics. Nothing calls it for a write today (HeartbeatBatchFenced
// writes no payloads and sends no NOTIFY), so nothing observed the gap. A
// later caller that reused this exported, general-purpose factory method for
// writes would have stored PLAINTEXT payloads on a deployment running with
// --encrypt-sensitive-payloads, silently -- there is no error path that a
// missing WithEncryption call takes.
//
// This configures every option OpenStore and OpenIsolatedStore both expose,
// opens a store through each, and asserts the two agree on EVERY field of
// *PostgresStore except db (compared by reflection, not name-by-name).
//
// NOT NAME-BY-NAME ON PURPOSE. The first version of this test checked seven
// named fields and missed the eighth (Metrics) -- cleat-review found that it
// compared the struct's dead, never-written lowercase `metrics` field
// instead of the exported `Metrics` field OpenStore and OpenIsolatedStore
// actually set, so the assertion was nil == nil on every run and could not
// fail no matter what OpenIsolatedStore did. A hand-picked field list has
// the same blind spot the very next field OpenStore grows would fall into;
// reflecting over the whole struct removes the list to maintain.
func TestPostgresOpenIsolatedStoreMatchesOpenStore(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping a real database test in short mode")
	}
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, db)
	defer func() {
		testutil.CleanupPostgresTestData(t, db)
		db.Close()
	}()

	metrics, err := prometheus.New(prometheus.Config{WorkerID: "test-worker"})
	if err != nil {
		t.Fatalf("prometheus.New: %v", err)
	}
	enc := &PayloadEncryption{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	const ttl = 3 * time.Hour
	const notifyChannel = "cleat_test_notify_channel"

	factory := NewPostgresStoreFactory(db, "public", ttl).
		WithDSN(testutil.PostgresTestDSN()).
		WithEncryption(enc, true).
		WithNotifyChannel(notifyChannel).
		WithMetrics(metrics).
		WithLogger(logger).
		WithSyncCommitOff(true)

	ctx := context.Background()

	openStore, openCloser, err := factory.OpenStore(ctx, DefaultTenantUUID, "default")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer openCloser.Close()

	isolatedStore, isolatedCloser, err := factory.OpenIsolatedStore(ctx, DefaultTenantUUID, 2, "default")
	if err != nil {
		t.Fatalf("OpenIsolatedStore: %v", err)
	}
	defer isolatedCloser.Close()

	open, ok := openStore.(*PostgresStore)
	if !ok {
		t.Fatalf("OpenStore returned %T, want *PostgresStore", openStore)
	}
	isolated, ok := isolatedStore.(*PostgresStore)
	if !ok {
		t.Fatalf("OpenIsolatedStore returned %T, want *PostgresStore", isolatedStore)
	}

	// The known-positive: prove this test can fail at all, by checking a
	// field that is SUPPOSED to differ between the two stores. If db were
	// ever equal, every comparison below would be checking two views of the
	// same pool rather than two independently-configured ones.
	if open.db == isolated.db {
		t.Fatal("OpenStore and OpenIsolatedStore share a *sql.DB -- OpenIsolatedStore is " +
			"supposed to open its own pool, and this test's other assertions are meaningless " +
			"if it did not")
	}

	assertAllFieldsMatchExceptDB(t, open, isolated)
}

// assertAllFieldsMatchExceptDB compares every field of *PostgresStore between
// open and isolated except db, failing on the first mismatch it finds per
// field. db is read through the unexported field directly by the caller (see
// the known-positive above) rather than here, since it is expected to
// DIFFER -- the opposite of everything else this function checks.
//
// unsafe.Pointer is required because most of these fields are unexported:
// reflect.Value.Interface() panics on a field obtained by plain field
// access ("reflect: reflect.Value.Interface: cannot return value obtained
// from unexported field or method"). reflect.NewAt bypasses that read-only
// restriction the same way encoding/json's own field-walking code does.
func assertAllFieldsMatchExceptDB(t *testing.T, open, isolated *PostgresStore) {
	t.Helper()
	ov := reflect.ValueOf(open).Elem()
	iv := reflect.ValueOf(isolated).Elem()
	typ := ov.Type()
	checked := 0
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if name == "db" {
			continue
		}
		ofv := reflect.NewAt(typ.Field(i).Type, unsafe.Pointer(ov.Field(i).UnsafeAddr())).Elem()
		ifv := reflect.NewAt(typ.Field(i).Type, unsafe.Pointer(iv.Field(i).UnsafeAddr())).Elem()
		checked++
		if !reflect.DeepEqual(ofv.Interface(), ifv.Interface()) {
			// A pointer field (encryption, logger, Metrics) prints its
			// pointee's full contents under %#v -- Metrics alone runs to
			// several kilobytes of OpenTelemetry instrument state. The
			// pointer VALUE is what this test actually compares (identity,
			// not contents), so print that instead; everything else keeps
			// %#v, which is short for every other field on this struct.
			if ofv.Kind() == reflect.Ptr {
				t.Errorf("%s: OpenIsolatedStore got %p, OpenStore got %p, want equal (same pointer)",
					name, ifv.Interface(), ofv.Interface())
			} else {
				t.Errorf("%s: OpenIsolatedStore got %#v, OpenStore got %#v, want equal",
					name, ifv.Interface(), ofv.Interface())
			}
		}
	}
	// The known-positive for THIS function: if PostgresStore ever grows a
	// field and this loop somehow stops seeing it -- NumField returning 0 on
	// a bad reflect.Value, say -- checked stays 0 or drops, and a test that
	// silently checks nothing is worse than the named-field version it
	// replaced.
	if checked < 10 {
		t.Fatalf("assertAllFieldsMatchExceptDB only checked %d fields; PostgresStore had "+
			"considerably more than that when this test was written -- the reflection walk is "+
			"broken and every check above passed by checking nothing", checked)
	}
}
