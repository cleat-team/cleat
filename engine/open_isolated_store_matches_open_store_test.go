package engine

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

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
// opens a store through each, and asserts the two agree on every field
// OpenIsolatedStore's doc comment claims to match. db and dsn are
// deliberately excluded from the comparison: OpenIsolatedStore opens a
// SEPARATE pool by design (that is the entire feature), so those two must
// differ.
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

	if isolated.encryption != open.encryption {
		t.Errorf("encryption: OpenIsolatedStore got %p, OpenStore got %p, want equal", isolated.encryption, open.encryption)
	}
	if isolated.encryptSensitivePayloads != open.encryptSensitivePayloads {
		t.Errorf("encryptSensitivePayloads: OpenIsolatedStore got %v, OpenStore got %v",
			isolated.encryptSensitivePayloads, open.encryptSensitivePayloads)
	}
	if isolated.idempotencyKeyTTL != open.idempotencyKeyTTL {
		t.Errorf("idempotencyKeyTTL: OpenIsolatedStore got %v, OpenStore got %v",
			isolated.idempotencyKeyTTL, open.idempotencyKeyTTL)
	}
	if isolated.notifyChannel != open.notifyChannel {
		t.Errorf("notifyChannel: OpenIsolatedStore got %q, OpenStore got %q",
			isolated.notifyChannel, open.notifyChannel)
	}
	if isolated.metrics != open.metrics {
		t.Errorf("metrics: OpenIsolatedStore got %p, OpenStore got %p, want equal", isolated.metrics, open.metrics)
	}
	if isolated.syncCommitOff != open.syncCommitOff {
		t.Errorf("syncCommitOff: OpenIsolatedStore got %v, OpenStore got %v",
			isolated.syncCommitOff, open.syncCommitOff)
	}
	if isolated.logger != open.logger {
		t.Errorf("logger: OpenIsolatedStore got %p, OpenStore got %p, want equal", isolated.logger, open.logger)
	}
}
