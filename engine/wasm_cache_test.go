package engine

import (
	"testing"
)

// ---------------------------------------------------------------------------
// NewWASMCache tests
// ---------------------------------------------------------------------------

func TestNewWASMCache(t *testing.T) {
	c := NewWASMCache(5, 100)
	if c == nil {
		t.Fatal("NewWASMCache returned nil")
	}
	if c.Len() != 0 {
		t.Errorf("new cache Len() = %d, want 0", c.Len())
	}
	if c.TotalBytes() != 0 {
		t.Errorf("new cache TotalBytes() = %d, want 0", c.TotalBytes())
	}
	if val, ok := c.Get("nonexistent"); val != nil || ok {
		t.Errorf("Get on empty cache: got (%v, %v), want (nil, false)", val, ok)
	}
}

// ---------------------------------------------------------------------------
// Get tests
// ---------------------------------------------------------------------------

func TestGet_Miss(t *testing.T) {
	c := NewWASMCache(10, 1000)

	if val, ok := c.Get("nonexistent"); val != nil || ok {
		t.Errorf("Get on empty cache: got (%v, %v), want (nil, false)", val, ok)
	}

	c.Put("key1", []byte("value1"))
	c.Put("key2", []byte("value2"))

	if val, ok := c.Get("nonexistent"); val != nil || ok {
		t.Errorf("Get after puts for missing key: got (%v, %v), want (nil, false)", val, ok)
	}
}

func TestGet_Hit(t *testing.T) {
	c := NewWASMCache(10, 1000)

	want := []byte("hello wasm")
	c.Put("mykey", want)

	got, ok := c.Get("mykey")
	if !ok {
		t.Fatal("Get after Put should return ok=true")
	}
	if string(got) != string(want) {
		t.Errorf("Get returned wrong bytes: got %q, want %q", got, want)
	}

	c.Put("key2", []byte("second"))
	got1, ok1 := c.Get("mykey")
	got2, ok2 := c.Get("key2")
	if !ok1 || string(got1) != string(want) {
		t.Errorf("first entry: got (%q, %v), want (%q, true)", got1, ok1, want)
	}
	if !ok2 || string(got2) != "second" {
		t.Errorf("second entry: got (%q, %v), want (%q, true)", got2, ok2, "second")
	}
}

func TestGet_LRUPromotion(t *testing.T) {
	c := NewWASMCache(2, 1000)

	c.Put("A", []byte("data-a"))
	c.Put("B", []byte("data-b"))

	// Access A — this promotes it ahead of B in LRU order.
	if _, ok := c.Get("A"); !ok {
		t.Fatal("Get A failed")
	}

	// Put C with maxEntries=2. Since A was just accessed, B is oldest.
	c.Put("C", []byte("data-c"))

	// A should survive (was promoted), B should be evicted.
	if _, ok := c.Get("A"); !ok {
		t.Error("A should survive eviction after LRU promotion")
	}
	if _, ok := c.Get("C"); !ok {
		t.Error("C should survive eviction")
	}
	if val, ok := c.Get("B"); val != nil || ok {
		t.Errorf("B should have been evicted: got (%v, %v)", val, ok)
	}
	if c.Len() != 2 {
		t.Errorf("Len() = %d, want 2", c.Len())
	}
}

// ---------------------------------------------------------------------------
// Put tests
// ---------------------------------------------------------------------------

func TestPut_NewEntry(t *testing.T) {
	c := NewWASMCache(10, 1000)

	c.Put("key1", []byte("first"))
	if c.Len() != 1 {
		t.Errorf("Len after 1 put = %d, want 1", c.Len())
	}
	if val, ok := c.Get("key1"); !ok || string(val) != "first" {
		t.Errorf("Get after Put: got (%q, %v), want (%q, true)", val, ok, "first")
	}

	c.Put("key2", []byte("second"))
	if c.Len() != 2 {
		t.Errorf("Len after 2 puts = %d, want 2", c.Len())
	}
}

func TestPut_UpdateExisting(t *testing.T) {
	c := NewWASMCache(10, 1000)

	c.Put("key", []byte("12345")) // 5 bytes
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1", c.Len())
	}
	if c.TotalBytes() != 5 {
		t.Fatalf("TotalBytes = %d, want 5", c.TotalBytes())
	}

	c.Put("key", []byte("1234567890")) // 10 bytes
	if c.Len() != 1 {
		t.Errorf("Len after update = %d, want 1 (update, not new entry)", c.Len())
	}
	if c.TotalBytes() != 10 {
		t.Errorf("TotalBytes after update = %d, want 10 (old 5 removed, new 10 added)", c.TotalBytes())
	}

	// Get returns updated value.
	got, ok := c.Get("key")
	if !ok || string(got) != "1234567890" {
		t.Errorf("Get after update: got (%q, %v), want (%q, true)", got, ok, "1234567890")
	}
}

func TestPut_EmptyBytes(t *testing.T) {
	c := NewWASMCache(10, 1000)

	c.Put("key", []byte{})
	if c.Len() != 1 {
		t.Errorf("Len after Put empty bytes = %d, want 1", c.Len())
	}
	if c.TotalBytes() != 0 {
		t.Errorf("TotalBytes after Put empty bytes = %d, want 0", c.TotalBytes())
	}

	got, ok := c.Get("key")
	if !ok {
		t.Fatal("Get after Put empty bytes should return ok=true")
	}
	if len(got) != 0 {
		t.Errorf("Get after Put empty bytes: got %q, want empty", got)
	}
}

func TestPut_SameKeySameData(t *testing.T) {
	c := NewWASMCache(10, 1000)

	c.Put("key", []byte("12345"))
	c.Put("other", []byte("other-data"))
	lenBefore := c.Len()
	bytesBefore := c.TotalBytes()

	// Update with same data.
	c.Put("key", []byte("12345"))

	if c.Len() != lenBefore {
		t.Errorf("Len() changed from %d to %d after same-data update", lenBefore, c.Len())
	}
	if c.TotalBytes() != bytesBefore {
		t.Errorf("TotalBytes() changed from %d to %d after same-data update", bytesBefore, c.TotalBytes())
	}

	// Other entry should still exist (update didn't evict it).
	if val, ok := c.Get("other"); !ok || string(val) != "other-data" {
		t.Errorf("other entry should survive same-data update: got (%q, %v)", val, ok)
	}
}

// ---------------------------------------------------------------------------
// Len / TotalBytes tests
// ---------------------------------------------------------------------------

func TestLen(t *testing.T) {
	c := NewWASMCache(5, 1000)

	if c.Len() != 0 {
		t.Errorf("Len on empty = %d, want 0", c.Len())
	}

	c.Put("a", []byte("1"))
	c.Put("b", []byte("2"))
	c.Put("c", []byte("3"))
	if c.Len() != 3 {
		t.Errorf("Len after 3 puts = %d, want 3", c.Len())
	}

	// Put 3 more entries to exceed maxEntries=5, triggering eviction.
	c.Put("d", []byte("4"))
	c.Put("e", []byte("5"))
	c.Put("f", []byte("6"))
	if c.Len() != 5 {
		t.Errorf("Len after eviction = %d, want 5 (maxEntries=5)", c.Len())
	}
}

func TestTotalBytes(t *testing.T) {
	c := NewWASMCache(100, 1000)

	if c.TotalBytes() != 0 {
		t.Errorf("TotalBytes on empty = %d, want 0", c.TotalBytes())
	}

	c.Put("a", []byte("123"))
	if c.TotalBytes() != 3 {
		t.Errorf("TotalBytes after 1 put = %d, want 3", c.TotalBytes())
	}

	c.Put("b", []byte("4567890"))
	if c.TotalBytes() != 10 {
		t.Errorf("TotalBytes after 2 puts = %d, want 10", c.TotalBytes())
	}

	// Update a with smaller data: old 3 removed, new 1 added → 10 - 3 + 1 = 8.
	c.Put("a", []byte("1"))
	if c.TotalBytes() != 8 {
		t.Errorf("TotalBytes after update = %d, want 8", c.TotalBytes())
	}
}

// ---------------------------------------------------------------------------
// Eviction tests
// ---------------------------------------------------------------------------

func TestEvict_MaxEntries(t *testing.T) {
	c := NewWASMCache(3, 1000)

	// Put 5 entries. Only 3 should survive.
	c.Put("1", []byte("entry-1"))
	c.Put("2", []byte("entry-2"))
	c.Put("3", []byte("entry-3"))
	c.Put("4", []byte("entry-4"))
	c.Put("5", []byte("entry-5"))

	if c.Len() != 3 {
		t.Errorf("Len = %d, want 3", c.Len())
	}

	// Oldest 2 ("1", "2") should be evicted.
	for _, key := range []string{"1", "2"} {
		if val, ok := c.Get(key); val != nil || ok {
			t.Errorf("key %q should have been evicted, got (%v, %v)", key, val, ok)
		}
	}
	// Newest 3 ("3", "4", "5") should survive.
	for _, key := range []string{"3", "4", "5"} {
		if _, ok := c.Get(key); !ok {
			t.Errorf("key %q should survive eviction", key)
		}
	}
}

func TestEvict_MaxBytes(t *testing.T) {
	c := NewWASMCache(100, 15)

	c.Put("a", []byte("12345")) // 5 bytes; oldest
	c.Put("b", []byte("67890")) // 5 bytes
	c.Put("c", []byte("abcde")) // 5 bytes
	// Total: 15 bytes, ok.

	// Adding 5 more bytes triggers bytes eviction.
	c.Put("d", []byte("fghij")) // 5 bytes; total=20 > 15

	// After eviction: oldest (a) removed, remaining 15 bytes.
	if c.TotalBytes() > 15 {
		t.Errorf("TotalBytes = %d, want <= 15", c.TotalBytes())
	}

	if val, ok := c.Get("a"); val != nil || ok {
		t.Errorf("a (oldest) should have been evicted: got (%v, %v)", val, ok)
	}
	// Newer entries survive.
	for _, key := range []string{"b", "c", "d"} {
		if _, ok := c.Get(key); !ok {
			t.Errorf("key %q should survive bytes eviction", key)
		}
	}
}

func TestEvict_MaxBytesZero(t *testing.T) {
	// maxBytes=0 means byte limit is not enforced.
	c := NewWASMCache(2, 0)

	c.Put("a", []byte("very-long-data-that-would-normally-blow-past-a-byte-limit"))
	c.Put("b", []byte("another-very-long-entry"))
	c.Put("c", []byte("third-entry"))

	// Only maxEntries=2 matters. "a" should be evicted (oldest).
	if c.Len() != 2 {
		t.Errorf("Len = %d, want 2 (only maxEntries enforced)", c.Len())
	}
	if val, ok := c.Get("a"); val != nil || ok {
		t.Error("a should have been evicted (maxEntries)")
	}
	if _, ok := c.Get("b"); !ok {
		t.Error("b should survive")
	}
	if _, ok := c.Get("c"); !ok {
		t.Error("c should survive")
	}
}

func TestEvict_MaxEntriesZero(t *testing.T) {
	c := NewWASMCache(0, 1000)

	c.Put("key", []byte("data"))
	if c.Len() != 0 {
		t.Errorf("Len with maxEntries=0 = %d, want 0 (entry evicted immediately)", c.Len())
	}

	// Multiple puts also immediately evicted.
	c.Put("a", []byte("1"))
	c.Put("b", []byte("2"))
	c.Put("c", []byte("3"))
	if c.Len() != 0 {
		t.Errorf("Len after multiple puts = %d, want 0", c.Len())
	}
	if c.TotalBytes() != 0 {
		t.Errorf("TotalBytes = %d, want 0", c.TotalBytes())
	}
}

func TestEvict_BothLimits(t *testing.T) {
	c := NewWASMCache(3, 25)

	// Fill to 3 entries, 25 bytes (at limit).
	c.Put("a", []byte("12345")) // 5 bytes; oldest
	c.Put("b", []byte("67890")) // 5 bytes
	c.Put("c", []byte("abcdefghijklmno")) // 15 bytes
	// Total: 3 entries, 25 bytes. At both limits.

	// Add one more entry: 4 entries, 30 bytes. Both limits exceeded.
	c.Put("d", []byte("12345")) // 5 bytes; total=30, entries=4

	// Eviction should remove oldest until both satisfied.
	// After removing "a" (5 bytes): 3 entries, 25 bytes. Both satisfied.
	if c.Len() != 3 {
		t.Errorf("Len = %d, want 3", c.Len())
	}
	if c.TotalBytes() > 25 {
		t.Errorf("TotalBytes = %d, want <= 25", c.TotalBytes())
	}

	// "a" should be gone, others survive.
	if val, ok := c.Get("a"); val != nil || ok {
		t.Errorf("a should have been evicted: got (%v, %v)", val, ok)
	}
	for _, key := range []string{"b", "c", "d"} {
		if _, ok := c.Get(key); !ok {
			t.Errorf("key %q should survive eviction", key)
		}
	}
}
