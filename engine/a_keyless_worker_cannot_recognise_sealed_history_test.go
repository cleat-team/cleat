package engine

// cleat#2324: engine/encryption.go documents, deliberately, that a sealed
// column carries no envelope or version prefix -- see PayloadEncryption's
// doc comment, "The transition needs no envelope", for why one would be
// strictly worse. So a worker with no key ring configured cannot recognise
// ciphertext by inspecting a row. PayloadEncryptionState (store_payload_encryption_state.go)
// answers a different question instead: has any worker on this database
// ever had a key ring configured. These tests cover the two implementations
// directly -- *PostgresStore against a real database, and *ShardedStore's
// aggregation over fake shards that need no database at all.

import (
	"context"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

func TestPayloadEncryptionEverEnabled_StartsFalseAndBecomesTrueOnceMarked(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)
	defer testutil.CleanupPostgresTestData(t, adminDB)
	// payload_encryption_ever_enabled is a singleton table with no tenant
	// scoping, so CleanupPostgresTestData's blanket DELETE does not reach it
	// -- clear it explicitly so this test does not inherit a mark some
	// earlier test in the same run left behind.
	if _, err := adminDB.Exec(`DELETE FROM payload_encryption_ever_enabled`); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	s := NewPostgresStore(adminDB)

	before, err := s.PayloadEncryptionEverEnabled(ctx)
	if err != nil {
		t.Fatalf("PayloadEncryptionEverEnabled (before): %v", err)
	}
	if before {
		t.Fatal("PayloadEncryptionEverEnabled reported true before anything marked it")
	}

	if err := s.MarkPayloadEncryptionEnabled(ctx); err != nil {
		t.Fatalf("MarkPayloadEncryptionEnabled: %v", err)
	}

	after, err := s.PayloadEncryptionEverEnabled(ctx)
	if err != nil {
		t.Fatalf("PayloadEncryptionEverEnabled (after): %v", err)
	}
	if !after {
		t.Fatal("PayloadEncryptionEverEnabled reported false after MarkPayloadEncryptionEnabled")
	}
}

func TestMarkPayloadEncryptionEnabled_IsIdempotent(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)
	defer testutil.CleanupPostgresTestData(t, adminDB)
	if _, err := adminDB.Exec(`DELETE FROM payload_encryption_ever_enabled`); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	s := NewPostgresStore(adminDB)

	if err := s.MarkPayloadEncryptionEnabled(ctx); err != nil {
		t.Fatalf("first MarkPayloadEncryptionEnabled: %v", err)
	}
	if err := s.MarkPayloadEncryptionEnabled(ctx); err != nil {
		t.Fatalf("second MarkPayloadEncryptionEnabled (must be a no-op, not an error): %v", err)
	}

	var count int
	if err := adminDB.QueryRowContext(ctx, `SELECT count(*) FROM payload_encryption_ever_enabled`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("marking twice left %d rows, want exactly 1 -- ON CONFLICT DO NOTHING should have made the second call a no-op", count)
	}
}

// fakePayloadEncryptionShardStore is a WorkflowStore that answers nothing
// real (every other method panics via the embedded nil stubWorkflowStore)
// except the two PayloadEncryptionState methods, held in memory. Used to
// test ShardedStore's aggregation logic without a database -- the shape
// already established by intentCapableShardStore in
// a_sharded_store_routes_call_intent_to_the_owning_shard_test.go.
type fakePayloadEncryptionShardStore struct {
	stubWorkflowStore
	enabled bool
}

func (s *fakePayloadEncryptionShardStore) MarkPayloadEncryptionEnabled(context.Context) error {
	s.enabled = true
	return nil
}

func (s *fakePayloadEncryptionShardStore) PayloadEncryptionEverEnabled(context.Context) (bool, error) {
	return s.enabled, nil
}

var (
	_ PayloadEncryptionState = (*fakePayloadEncryptionShardStore)(nil)
	_ WorkflowStore          = (*fakePayloadEncryptionShardStore)(nil)
)

func TestShardedStore_PayloadEncryptionEverEnabled_TrueIfAnyShardWasMarked(t *testing.T) {
	a := &fakePayloadEncryptionShardStore{}
	b := &fakePayloadEncryptionShardStore{}
	ss, err := NewShardedStore(
		[]ShardConfig{{Name: "shard-a"}, {Name: "shard-b"}},
		[]WorkflowStore{a, b},
		[]func() error{func() error { return nil }, func() error { return nil }},
	)
	if err != nil {
		t.Fatalf("NewShardedStore: %v", err)
	}

	ctx := context.Background()
	before, err := ss.PayloadEncryptionEverEnabled(ctx)
	if err != nil {
		t.Fatalf("PayloadEncryptionEverEnabled (before): %v", err)
	}
	if before {
		t.Fatal("PayloadEncryptionEverEnabled reported true before either shard was marked")
	}

	// Mark only ONE shard directly (bypassing ShardedStore.Mark, which marks
	// every shard) -- a keyless worker reading from any shard is at risk, so
	// ONE sealed shard must be enough to refuse the whole worker.
	b.enabled = true

	after, err := ss.PayloadEncryptionEverEnabled(ctx)
	if err != nil {
		t.Fatalf("PayloadEncryptionEverEnabled (after): %v", err)
	}
	if !after {
		t.Fatal("PayloadEncryptionEverEnabled reported false with one shard marked -- one sealed shard must be enough to refuse the whole worker")
	}
}

func TestShardedStore_MarkPayloadEncryptionEnabled_MarksEveryShard(t *testing.T) {
	a := &fakePayloadEncryptionShardStore{}
	b := &fakePayloadEncryptionShardStore{}
	ss, err := NewShardedStore(
		[]ShardConfig{{Name: "shard-a"}, {Name: "shard-b"}},
		[]WorkflowStore{a, b},
		[]func() error{func() error { return nil }, func() error { return nil }},
	)
	if err != nil {
		t.Fatalf("NewShardedStore: %v", err)
	}

	if err := ss.MarkPayloadEncryptionEnabled(context.Background()); err != nil {
		t.Fatalf("MarkPayloadEncryptionEnabled: %v", err)
	}

	// A worker configured with a key ring can route any workflow to any
	// shard (routing is a hash of the workflow ID), so marking must not stop
	// at the first shard -- BOTH must carry the marker.
	if !a.enabled {
		t.Error("shard-a was not marked")
	}
	if !b.enabled {
		t.Error("shard-b was not marked")
	}
}
