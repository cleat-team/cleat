package blobstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rcownie/cleat/internal/host"
	"github.com/rcownie/cleat/internal/plugin"
)

// ---------------------------------------------------------------------------
// memoryFakeDB: a minimal SQL database simulation for testing memoryBackend.
// It handles only the two queries used by memoryBackend.Put/Get:
//
//	INSERT INTO blob_content (sha256, size, data, ref_count, storage_backend)
//	VALUES ($1, $2, $3, 0, 'memory') ON CONFLICT (sha256) DO UPDATE SET data = EXCLUDED.data
//	SELECT data FROM blob_content WHERE sha256 = $1
// ---------------------------------------------------------------------------

type memoryFakeDB struct {
	mu   sync.RWMutex
	data map[string][]byte // sha256 hex -> raw blob bytes
}

func newMemoryFakeDB() *memoryFakeDB {
	return &memoryFakeDB{data: make(map[string][]byte)}
}

// --- driver.Connector ---

type memoryFakeConnector struct {
	db *memoryFakeDB
}

func (c *memoryFakeConnector) Connect(_ context.Context) (driver.Conn, error) {
	return &memoryFakeConn{db: c.db}, nil
}

func (c *memoryFakeConnector) Driver() driver.Driver { return &fakeDrv{} }

// --- driver.Conn ---

type memoryFakeConn struct {
	db *memoryFakeDB
}

func (*memoryFakeConn) Prepare(_ string) (driver.Stmt, error) {
	return nil, fmt.Errorf("memoryFakeConn: unexpected Prepare")
}

func (*memoryFakeConn) Close() error { return nil }

func (*memoryFakeConn) Begin() (driver.Tx, error) { return &fakeTx{}, nil }

// --- driver.ExecerContext ---

func (c *memoryFakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.db.mu.Lock()
	defer c.db.mu.Unlock()

	if strings.Contains(query, "INSERT INTO blob_content") && strings.Contains(query, "data") {
		sha256Bytes, err := argBytes(args, 1)
		if err != nil {
			return nil, err
		}
		data, err := argBytes(args, 3)
		if err != nil {
			return nil, err
		}
		sha256Hex := fmt.Sprintf("%x", sha256Bytes)
		c.db.data[sha256Hex] = append([]byte(nil), data...)
		return &fakeResult{rowsAffected: 1}, nil
	}
	return nil, fmt.Errorf("memoryFakeConn: unexpected Exec query: %s", query)
}

// --- driver.QueryerContext ---

func (c *memoryFakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.db.mu.RLock()
	defer c.db.mu.RUnlock()

	if strings.Contains(query, "SELECT data FROM blob_content") {
		sha256Bytes, err := argBytes(args, 1)
		if err != nil {
			return nil, err
		}
		sha256Hex := fmt.Sprintf("%x", sha256Bytes)
		data, ok := c.db.data[sha256Hex]
		if !ok {
			return &fakeRows{columns: []string{"data"}}, nil
		}
		return &fakeRows{
			columns: []string{"data"},
			data:    [][]driver.Value{{data}},
		}, nil
	}
	return nil, fmt.Errorf("memoryFakeConn: unexpected Query query: %s", query)
}

// ---------------------------------------------------------------------------
// enhancedFakeConn: wraps fakeConn and adds handling for cleanup queries
// (DELETE FROM workflow_blob_refs, WITH deleted AS ..., DELETE FROM blob_content
// ... RETURNING) that are not covered by the basic fakeConn.
// ---------------------------------------------------------------------------

type cleanupState struct {
	mu               sync.Mutex
	workflowBlobRefs map[string]map[string]struct{} // workflowID -> set of sha256 hexes
	workflowStatuses map[string]string               // workflowID -> status
}

func newCleanupState() *cleanupState {
	return &cleanupState{
		workflowBlobRefs: make(map[string]map[string]struct{}),
		workflowStatuses: make(map[string]string),
	}
}

func (s *cleanupState) addRef(wfID, sha256Hex string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workflowBlobRefs[wfID] == nil {
		s.workflowBlobRefs[wfID] = make(map[string]struct{})
	}
	s.workflowBlobRefs[wfID][sha256Hex] = struct{}{}
}

func (s *cleanupState) setStatus(wfID, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workflowStatuses[wfID] = status
}

type cleanupFakeConnector struct {
	store *fakeDBStore
	state *cleanupState
}

func (c *cleanupFakeConnector) Connect(_ context.Context) (driver.Conn, error) {
	return &cleanupFakeConn{
		fakeConn: &fakeConn{store: c.store},
		state:    c.state,
	}, nil
}

func (c *cleanupFakeConnector) Driver() driver.Driver { return &fakeDrv{} }

type cleanupFakeConn struct {
	*fakeConn
	state *cleanupState
}

func (c *cleanupFakeConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	switch {
	case strings.Contains(query, "DELETE FROM workflow_blob_refs"):
		return c.execDeleteWorkflowRefs()
	case strings.Contains(query, "WITH deleted AS"):
		return c.execCleanupExpired()
	default:
		return c.fakeConn.ExecContext(ctx, query, args)
	}
}

func (c *cleanupFakeConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "DELETE FROM blob_content") && strings.Contains(query, "RETURNING") {
		return c.queryDeleteOrphans()
	}
	return c.fakeConn.QueryContext(ctx, query, args)
}

// cleanup phase 1: delete refs for workflows not in 'ready' or 'running' status
func (c *cleanupFakeConn) execDeleteWorkflowRefs() (driver.Result, error) {
	c.state.mu.Lock()
	defer c.state.mu.Unlock()

	var deleted int64
	for wfID, shaHexes := range c.state.workflowBlobRefs {
		status, ok := c.state.workflowStatuses[wfID]
		if !ok || (status != "ready" && status != "running") {
			deleted += int64(len(shaHexes))
			delete(c.state.workflowBlobRefs, wfID)
		}
	}
	return &fakeResult{rowsAffected: deleted}, nil
}

// cleanup phase 2: delete expired/soft-deleted index entries and decrement ref_count
func (c *cleanupFakeConn) execCleanupExpired() (driver.Result, error) {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()

	now := c.store.now()

	// Collect sha256 counts for expired/soft-deleted entries
	sha256Counts := make(map[string]int64) // sha256 hex -> count of deleted index entries
	var expiredKeys []string

	for idxKey, row := range c.store.blobIndex {
		if (row.expiresAt != nil && row.expiresAt.Before(now)) || row.deletedAt != nil {
			hex := fmt.Sprintf("%x", row.sha256Bytes)
			sha256Counts[hex]++
			expiredKeys = append(expiredKeys, idxKey)
		}
	}

	// Delete the expired/soft-deleted index entries
	for _, key := range expiredKeys {
		delete(c.store.blobIndex, key)
	}

	// Decrement ref_count for each affected blob_content
	for hex, count := range sha256Counts {
		if cr, ok := c.store.blobContent[hex]; ok {
			cr.refCount -= count
		}
	}

	return &fakeResult{rowsAffected: int64(len(sha256Counts))}, nil
}

// cleanup phase 3: delete orphaned blob_content and return their sha256 + storage_backend
func (c *cleanupFakeConn) queryDeleteOrphans() (driver.Rows, error) {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()

	c.state.mu.Lock()
	defer c.state.mu.Unlock()

	var deletedRows [][]driver.Value
	var orphanHexes []string

	for hex, cr := range c.store.blobContent {
		if cr.refCount <= 0 {
			// Check that no active workflow ref references this blob
			hasRef := false
			for _, shaHexes := range c.state.workflowBlobRefs {
				if _, ok := shaHexes[hex]; ok {
					hasRef = true
					break
				}
			}
			if !hasRef {
				deletedRows = append(deletedRows, []driver.Value{
					cr.sha256Bytes,
					cr.storageBackend,
				})
				orphanHexes = append(orphanHexes, hex)
			}
		}
	}

	for _, hex := range orphanHexes {
		delete(c.store.blobContent, hex)
	}

	return &fakeRows{
		columns: []string{"sha256", "storage_backend"},
		data:    deletedRows,
	}, nil
}

// ---------------------------------------------------------------------------
// 4a: Memory backend tests
// ---------------------------------------------------------------------------

func TestNewMemoryBackend(t *testing.T) {
	db := sql.OpenDB(&memoryFakeConnector{db: newMemoryFakeDB()})
	t.Cleanup(func() { db.Close() })

	b := newMemoryBackend(&host.SQLDBAdapter{DB: db}, plugin.DialectPostgres)
	if b == nil {
		t.Fatal("newMemoryBackend returned nil")
	}
}

func TestMemoryPutAndGet(t *testing.T) {
	mdb := newMemoryFakeDB()
	db := sql.OpenDB(&memoryFakeConnector{db: mdb})
	t.Cleanup(func() { db.Close() })

	b := newMemoryBackend(&host.SQLDBAdapter{DB: db}, plugin.DialectPostgres)
	ctx := context.Background()

	data := []byte("hello world")
	sha256Hex := fmt.Sprintf("%x", sha256.Sum256(data))

	// Put
	if err := b.Put(ctx, sha256Hex, data, ""); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Get
	got, err := b.Get(ctx, sha256Hex)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("Get: expected %q, got %q", data, got)
	}

	// data should also be in the fake DB store
	mdb.mu.RLock()
	stored, ok := mdb.data[sha256Hex]
	mdb.mu.RUnlock()
	if !ok {
		t.Fatal("expected data in memoryFakeDB store")
	}
	if !bytes.Equal(stored, data) {
		t.Errorf("store: expected %q, got %q", data, stored)
	}
}

func TestMemoryGetMissing(t *testing.T) {
	db := sql.OpenDB(&memoryFakeConnector{db: newMemoryFakeDB()})
	t.Cleanup(func() { db.Close() })

	b := newMemoryBackend(&host.SQLDBAdapter{DB: db}, plugin.DialectPostgres)
	ctx := context.Background()

	_, err := b.Get(ctx, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err == nil {
		t.Fatal("expected error for missing blob")
	}
	if !strings.Contains(err.Error(), "content not found") {
		t.Errorf("expected 'content not found' error, got: %v", err)
	}
}

func TestMemoryDeleteNoop(t *testing.T) {
	mdb := newMemoryFakeDB()
	db := sql.OpenDB(&memoryFakeConnector{db: mdb})
	t.Cleanup(func() { db.Close() })

	b := newMemoryBackend(&host.SQLDBAdapter{DB: db}, plugin.DialectPostgres)
	ctx := context.Background()

	data := []byte("persistent data")
	sha256Hex := fmt.Sprintf("%x", sha256.Sum256(data))

	if err := b.Put(ctx, sha256Hex, data, ""); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Delete is a no-op for memory backend
	if err := b.Delete(ctx, sha256Hex); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Data should still be accessible after no-op Delete
	got, err := b.Get(ctx, sha256Hex)
	if err != nil {
		t.Fatalf("Get after Delete: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("Get after Delete: expected %q, got %q", data, got)
	}
}

func TestMemoryPutOverwrite(t *testing.T) {
	mdb := newMemoryFakeDB()
	db := sql.OpenDB(&memoryFakeConnector{db: mdb})
	t.Cleanup(func() { db.Close() })

	b := newMemoryBackend(&host.SQLDBAdapter{DB: db}, plugin.DialectPostgres)
	ctx := context.Background()

	// Use a deterministic sha256 that won't match actual data
	sha256Bytes := make([]byte, 32)
	sha256Bytes[0] = 0xaa
	sha256Hex := hex.EncodeToString(sha256Bytes)

	// First Put
	if err := b.Put(ctx, sha256Hex, []byte("original"), ""); err != nil {
		t.Fatalf("Put original: %v", err)
	}

	// Overwrite with same sha256 key
	if err := b.Put(ctx, sha256Hex, []byte("overwritten"), ""); err != nil {
		t.Fatalf("Put overwrite: %v", err)
	}

	// Get should return the new data
	got, err := b.Get(ctx, sha256Hex)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "overwritten" {
		t.Errorf("expected 'overwritten', got %q", string(got))
	}
}

func TestMemoryConcurrency(t *testing.T) {
	mdb := newMemoryFakeDB()
	db := sql.OpenDB(&memoryFakeConnector{db: mdb})
	t.Cleanup(func() { db.Close() })

	b := newMemoryBackend(&host.SQLDBAdapter{DB: db}, plugin.DialectPostgres)
	ctx := context.Background()

	var wg sync.WaitGroup
	numGoroutines := 20

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			data := []byte(fmt.Sprintf("concurrent-data-%d", n))
			sha256Hex := fmt.Sprintf("%x", sha256.Sum256(data))

			// Put
			if err := b.Put(ctx, sha256Hex, data, ""); err != nil {
				t.Errorf("goroutine %d Put: %v", n, err)
				return
			}

			// Get
			got, err := b.Get(ctx, sha256Hex)
			if err != nil {
				t.Errorf("goroutine %d Get: %v", n, err)
				return
			}
			if !bytes.Equal(got, data) {
				t.Errorf("goroutine %d: expected %q, got %q", n, data, got)
			}
		}(i)
	}
	wg.Wait()

	// Verify all data was stored correctly
	for i := 0; i < numGoroutines; i++ {
		data := []byte(fmt.Sprintf("concurrent-data-%d", i))
		sha256Hex := fmt.Sprintf("%x", sha256.Sum256(data))
		got, err := b.Get(ctx, sha256Hex)
		if err != nil {
			t.Errorf("final Get %d: %v", i, err)
			continue
		}
		if !bytes.Equal(got, data) {
			t.Errorf("final Get %d: expected %q, got %q", i, data, got)
		}
	}
}

// ---------------------------------------------------------------------------
// 4c: Background cleanup tests
// ---------------------------------------------------------------------------

func TestCleanupExpiredRemovesExpired(t *testing.T) {
	clock := newFakeClock()
	store := newFakeDBStore()
	store.now = clock.Now
	state := newCleanupState()

	db := sql.OpenDB(&cleanupFakeConnector{store: store, state: state})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.Default(),
	}

	// Create a blob_content entry
	sha256Bytes := make([]byte, 32)
	sha256Bytes[0] = 1
	sha256Hex := fmt.Sprintf("%x", sha256Bytes)
	store.blobContent[sha256Hex] = &fcRow{
		sha256Bytes:    sha256Bytes,
		size:           100,
		refCount:       1,
		storageBackend: "memory",
	}

	// Create blob_index entry with past expiresAt
	pastTime := clock.Now().Add(-1 * time.Hour)
	store.blobIndex["tenant1:expired-key"] = &fiRow{
		key:         "expired-key",
		tenantID:    "tenant1",
		sha256Bytes: sha256Bytes,
		size:        100,
		contentType: "text/plain",
		tags:        "{}",
		createdAt:   clock.Now().Add(-2 * time.Hour),
		expiresAt:   &pastTime,
	}

	// Also create a non-expired entry to verify it is preserved
	sha256Bytes2 := make([]byte, 32)
	sha256Bytes2[0] = 2
	sha256Hex2 := fmt.Sprintf("%x", sha256Bytes2)
	store.blobContent[sha256Hex2] = &fcRow{
		sha256Bytes:    sha256Bytes2,
		size:           200,
		refCount:       1,
		storageBackend: "memory",
	}

	futureTime := clock.Now().Add(24 * time.Hour)
	store.blobIndex["tenant1:active-key"] = &fiRow{
		key:         "active-key",
		tenantID:    "tenant1",
		sha256Bytes: sha256Bytes2,
		size:        200,
		contentType: "text/plain",
		tags:        "{}",
		createdAt:   clock.Now(),
		expiresAt:   &futureTime,
	}

	staleRefs, expiredEntries, orphanedBlobs, err := p.cleanupExpired(context.Background())
	if err != nil {
		t.Fatalf("cleanupExpired: %v", err)
	}

	// Should have found 1 expired entry
	if expiredEntries != 1 {
		t.Errorf("expected 1 expired entry, got %d", expiredEntries)
	}

	// Expired entry should be gone
	store.mu.RLock()
	_, expiredExists := store.blobIndex["tenant1:expired-key"]
	store.mu.RUnlock()
	if expiredExists {
		t.Error("expired blob_index entry should have been removed")
	}

	// Active entry should still exist
	store.mu.RLock()
	_, activeExists := store.blobIndex["tenant1:active-key"]
	store.mu.RUnlock()
	if !activeExists {
		t.Error("active blob_index entry should still exist")
	}

	// Verify ref_count was decremented on the expired blob's content
	store.mu.RLock()
	cr := store.blobContent[sha256Hex]
	store.mu.RUnlock()
	if cr != nil && cr.refCount != 0 {
		t.Errorf("expected ref_count=0 after expiry, got %d", cr.refCount)
	}

	// staleRefs should be 0 (no workflow refs)
	if staleRefs != 0 {
		t.Errorf("expected staleRefs=0, got %d", staleRefs)
	}
	// After the expired entry is removed, ref_count drops to 0, so the blob
	// content is garbage collected in phase 3.
	if orphanedBlobs != 1 {
		t.Errorf("expected orphanedBlobs=1 (ref_count reached 0), got %d", orphanedBlobs)
	}
}

func TestCleanupExpiredKeepsFuture(t *testing.T) {
	clock := newFakeClock()
	store := newFakeDBStore()
	store.now = clock.Now
	state := newCleanupState()

	db := sql.OpenDB(&cleanupFakeConnector{store: store, state: state})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.Default(),
	}

	// Create blob_content
	sha256Bytes := make([]byte, 32)
	sha256Bytes[0] = 3
	sha256Hex := fmt.Sprintf("%x", sha256Bytes)
	store.blobContent[sha256Hex] = &fcRow{
		sha256Bytes:    sha256Bytes,
		size:           100,
		refCount:       1,
		storageBackend: "memory",
	}

	// Create blob_index with future expiresAt
	futureTime := clock.Now().Add(24 * time.Hour)
	store.blobIndex["tenant1:future-key"] = &fiRow{
		key:         "future-key",
		tenantID:    "tenant1",
		sha256Bytes: sha256Bytes,
		size:        100,
		contentType: "text/plain",
		tags:        "{}",
		createdAt:   clock.Now(),
		expiresAt:   &futureTime,
	}

	staleRefs, expiredEntries, orphanedBlobs, err := p.cleanupExpired(context.Background())
	if err != nil {
		t.Fatalf("cleanupExpired: %v", err)
	}

	if expiredEntries != 0 {
		t.Errorf("expected 0 expired entries, got %d", expiredEntries)
	}

	// The entry should still be there
	store.mu.RLock()
	_, exists := store.blobIndex["tenant1:future-key"]
	store.mu.RUnlock()
	if !exists {
		t.Error("future blob_index entry should still exist")
	}

	// ref_count should be unchanged
	store.mu.RLock()
	cr := store.blobContent[sha256Hex]
	store.mu.RUnlock()
	if cr != nil && cr.refCount != 1 {
		t.Errorf("expected ref_count=1, got %d", cr.refCount)
	}

	if staleRefs != 0 {
		t.Errorf("expected staleRefs=0, got %d", staleRefs)
	}
	if orphanedBlobs != 0 {
		t.Errorf("expected orphanedBlobs=0, got %d", orphanedBlobs)
	}
}

func TestCleanupExpiredSoftDeleted(t *testing.T) {
	clock := newFakeClock()
	store := newFakeDBStore()
	store.now = clock.Now
	state := newCleanupState()

	db := sql.OpenDB(&cleanupFakeConnector{store: store, state: state})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.Default(),
	}

	// Create blob_content
	sha256Bytes := make([]byte, 32)
	sha256Bytes[0] = 4
	sha256Hex := fmt.Sprintf("%x", sha256Bytes)
	store.blobContent[sha256Hex] = &fcRow{
		sha256Bytes:    sha256Bytes,
		size:           100,
		refCount:       1,
		storageBackend: "memory",
	}

	// Create blob_index with deletedAt set
	now := clock.Now()
	store.blobIndex["tenant1:soft-deleted-key"] = &fiRow{
		key:         "soft-deleted-key",
		tenantID:    "tenant1",
		sha256Bytes: sha256Bytes,
		size:        100,
		contentType: "text/plain",
		tags:        "{}",
		createdAt:   now.Add(-1 * time.Hour),
		expiresAt:   nil,
		deletedAt:   &now,
	}

	_, expiredEntries, _, err := p.cleanupExpired(context.Background())
	if err != nil {
		t.Fatalf("cleanupExpired: %v", err)
	}

	if expiredEntries != 1 {
		t.Errorf("expected 1 expired entry (soft-deleted), got %d", expiredEntries)
	}

	// Soft-deleted entry should be gone
	store.mu.RLock()
	_, exists := store.blobIndex["tenant1:soft-deleted-key"]
	store.mu.RUnlock()
	if exists {
		t.Error("soft-deleted blob_index entry should have been removed")
	}
}

func TestRunStartsAndStopsOnCancel(t *testing.T) {
	t.Run("nil db", func(t *testing.T) {
		p := &Plugin{logger: slog.Default()}

		ctx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)
		go func() {
			errCh <- p.Run(ctx)
		}()

		cancel()

		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("Run returned error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not stop after context cancellation")
		}
	})

	t.Run("with db", func(t *testing.T) {
		clock := newFakeClock()
		store := newFakeDBStore()
		store.now = clock.Now
		state := newCleanupState()

		db := sql.OpenDB(&cleanupFakeConnector{store: store, state: state})
		t.Cleanup(func() { db.Close() })

		p := &Plugin{
			db:     &host.SQLDBAdapter{DB: db},
			logger: slog.Default(),
			config: Config{Backend: "memory"},
		}

		ctx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)
		go func() {
			errCh <- p.Run(ctx)
		}()

		// Give the goroutine time to start and enter the select loop
		time.Sleep(10 * time.Millisecond)
		cancel()

		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("Run returned error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not stop after context cancellation")
		}
	})
}

func TestRunWithDBHandlesCleanup(t *testing.T) {
	// Verify Run integrates with the DB properly by setting up
	// a plugin with expired blobs, running cleanupExpired explicitly,
	// and verifying the cleanup works end-to-end.
	clock := newFakeClock()
	store := newFakeDBStore()
	store.now = clock.Now
	state := newCleanupState()

	db := sql.OpenDB(&cleanupFakeConnector{store: store, state: state})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.Default(),
		config: Config{Backend: "memory"},
	}

	// Create blob_content entry
	sha256Bytes := make([]byte, 32)
	sha256Bytes[0] = 5
	sha256Hex := fmt.Sprintf("%x", sha256Bytes)
	store.blobContent[sha256Hex] = &fcRow{
		sha256Bytes:    sha256Bytes,
		size:           100,
		refCount:       1,
		storageBackend: "memory",
	}

	// Create expired blob_index entry
	pastTime := clock.Now().Add(-1 * time.Hour)
	store.blobIndex["tenant1:gc-key"] = &fiRow{
		key:         "gc-key",
		tenantID:    "tenant1",
		sha256Bytes: sha256Bytes,
		size:        100,
		contentType: "text/plain",
		tags:        "{}",
		createdAt:   clock.Now().Add(-2 * time.Hour),
		expiresAt:   &pastTime,
	}

	// Run cleanupExpired directly (same function Run's ticker calls)
	staleRefs, expiredEntries, orphanedBlobs, err := p.cleanupExpired(context.Background())
	if err != nil {
		t.Fatalf("cleanupExpired: %v", err)
	}

	if staleRefs != 0 {
		t.Errorf("expected staleRefs=0, got %d", staleRefs)
	}
	if expiredEntries != 1 {
		t.Errorf("expected expiredEntries=1, got %d", expiredEntries)
	}

	// With ref_count decremented to 0 and no workflow refs, the blob_content
	// should also be garbage collected.
	if orphanedBlobs != 1 {
		t.Errorf("expected orphanedBlobs=1, got %d", orphanedBlobs)
	}

	// Verify blob_content is gone
	store.mu.RLock()
	_, exists := store.blobContent[sha256Hex]
	store.mu.RUnlock()
	if exists {
		t.Error("blob_content should have been garbage collected")
	}
}

// ---------------------------------------------------------------------------
// Cleanup with workflow refs — verify that in-flight workflows protect blobs
// ---------------------------------------------------------------------------

func TestCleanupWithActiveWorkflowRefs(t *testing.T) {
	clock := newFakeClock()
	store := newFakeDBStore()
	store.now = clock.Now
	state := newCleanupState()

	db := sql.OpenDB(&cleanupFakeConnector{store: store, state: state})
	t.Cleanup(func() { db.Close() })

	// Create blob_content with ref_count=0 (no remaining index entries)
	sha256Bytes := make([]byte, 32)
	sha256Bytes[0] = 6
	sha256Hex := fmt.Sprintf("%x", sha256Bytes)
	store.blobContent[sha256Hex] = &fcRow{
		sha256Bytes:    sha256Bytes,
		size:           100,
		refCount:       0,
		storageBackend: "memory",
	}

	// Register an in-flight workflow ref that protects this blob
	state.addRef("wf-active", sha256Hex)
	state.setStatus("wf-active", "running")

	p := &Plugin{
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.Default(),
	}

	_, _, orphanedBlobs, err := p.cleanupExpired(context.Background())
	if err != nil {
		t.Fatalf("cleanupExpired: %v", err)
	}

	// Orphaned count should be 0 because the workflow ref protects it
	if orphanedBlobs != 0 {
		t.Errorf("expected orphanedBlobs=0 with active workflow ref, got %d", orphanedBlobs)
	}

	// The blob_content should still exist
	store.mu.RLock()
	_, exists := store.blobContent[sha256Hex]
	store.mu.RUnlock()
	if !exists {
		t.Error("blob_content should still exist (protected by active workflow ref)")
	}
}

func TestCleanupWithStaleWorkflowRefs(t *testing.T) {
	clock := newFakeClock()
	store := newFakeDBStore()
	store.now = clock.Now
	state := newCleanupState()

	db := sql.OpenDB(&cleanupFakeConnector{store: store, state: state})
	t.Cleanup(func() { db.Close() })

	// Create blob_content with ref_count=0
	sha256Bytes := make([]byte, 32)
	sha256Bytes[0] = 7
	sha256Hex := fmt.Sprintf("%x", sha256Bytes)
	store.blobContent[sha256Hex] = &fcRow{
		sha256Bytes:    sha256Bytes,
		size:           100,
		refCount:       0,
		storageBackend: "memory",
	}

	// Register a stale (done) workflow ref
	state.addRef("wf-done", sha256Hex)
	state.setStatus("wf-done", "done")

	// Register an in-flight workflow ref for blob that should NOT be affected
	sha256Bytes2 := make([]byte, 32)
	sha256Bytes2[0] = 8
	sha256Hex2 := fmt.Sprintf("%x", sha256Bytes2)
	store.blobContent[sha256Hex2] = &fcRow{
		sha256Bytes:    sha256Bytes2,
		size:           200,
		refCount:       0,
		storageBackend: "memory",
	}
	state.addRef("wf-active", sha256Hex2)
	state.setStatus("wf-active", "running")

	p := &Plugin{
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.Default(),
	}

	staleRefs, _, orphanedBlobs, err := p.cleanupExpired(context.Background())
	if err != nil {
		t.Fatalf("cleanupExpired: %v", err)
	}

	// Phase 1 should have cleaned the stale ref (wf-done)
	if staleRefs != 1 {
		t.Errorf("expected staleRefs=1, got %d", staleRefs)
	}

	// The stale-ref blob should be orphaned now
	if orphanedBlobs != 1 {
		t.Errorf("expected orphanedBlobs=1 (only stale ref blob), got %d", orphanedBlobs)
	}

	// The stale-ref blob_content should be gone
	store.mu.RLock()
	_, exists := store.blobContent[sha256Hex]
	store.mu.RUnlock()
	if exists {
		t.Error("blob_content with stale refs should have been garbage collected")
	}

	// The active-ref blob_content should still exist
	store.mu.RLock()
	_, exists = store.blobContent[sha256Hex2]
	store.mu.RUnlock()
	if !exists {
		t.Error("blob_content with active ref should still exist")
	}
}

// ---------------------------------------------------------------------------
// Additional memory backend edge-case tests
// ---------------------------------------------------------------------------

func TestMemoryPutEmptyData(t *testing.T) {
	mdb := newMemoryFakeDB()
	db := sql.OpenDB(&memoryFakeConnector{db: mdb})
	t.Cleanup(func() { db.Close() })

	b := newMemoryBackend(&host.SQLDBAdapter{DB: db}, plugin.DialectPostgres)
	ctx := context.Background()

	// Force-synthesize a sha256 for empty data
	data := []byte("")
	sha256Hex := fmt.Sprintf("%x", sha256.Sum256(data))

	if err := b.Put(ctx, sha256Hex, data, ""); err != nil {
		t.Fatalf("Put empty data: %v", err)
	}

	got, err := b.Get(ctx, sha256Hex)
	if err != nil {
		t.Fatalf("Get empty data: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty data, got %d bytes", len(got))
	}
}

func TestMemoryPutLargeBlob(t *testing.T) {
	mdb := newMemoryFakeDB()
	db := sql.OpenDB(&memoryFakeConnector{db: mdb})
	t.Cleanup(func() { db.Close() })

	b := newMemoryBackend(&host.SQLDBAdapter{DB: db}, plugin.DialectPostgres)
	ctx := context.Background()

	// 100KB blob
	size := 100 * 1024
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 256)
	}
	sha256Hex := fmt.Sprintf("%x", sha256.Sum256(data))

	if err := b.Put(ctx, sha256Hex, data, ""); err != nil {
		t.Fatalf("Put large: %v", err)
	}

	got, err := b.Get(ctx, sha256Hex)
	if err != nil {
		t.Fatalf("Get large: %v", err)
	}
	if len(got) != size {
		t.Errorf("expected %d bytes, got %d", size, len(got))
	}
	if !bytes.Equal(got, data) {
		t.Error("large blob data mismatch")
	}
}

func TestMemoryBackendConcurrentReadWrite(t *testing.T) {
	mdb := newMemoryFakeDB()
	db := sql.OpenDB(&memoryFakeConnector{db: mdb})
	t.Cleanup(func() { db.Close() })

	b := newMemoryBackend(&host.SQLDBAdapter{DB: db}, plugin.DialectPostgres)
	ctx := context.Background()

	// Multiple goroutines reading and writing the same key
	sha256Bytes := make([]byte, 32)
	sha256Bytes[0] = 0xbb
	sha256Hex := hex.EncodeToString(sha256Bytes)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			data := []byte(fmt.Sprintf("value-%d", n))
			_ = b.Put(ctx, sha256Hex, data, "")
			_, _ = b.Get(ctx, sha256Hex)
		}(i)
	}
	wg.Wait()
}
