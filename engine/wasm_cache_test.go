package engine

import (
	"sync"
	"testing"
)

func TestNewWASMCache(t *testing.T) {
	c := NewWASMCache(10, 1024)
	if c == nil {
		t.Fatal("expected non-nil WASMCache")
	}
	if c.Len() != 0 {
		t.Errorf("expected Len=0, got %d", c.Len())
	}
	if c.TotalBytes() != 0 {
		t.Errorf("expected TotalBytes=0, got %d", c.TotalBytes())
	}
}

func TestWASMCache_Get_Miss(t *testing.T) {
	c := NewWASMCache(10, 1024)

	data, ok := c.Get("nonexistent")
	if ok {
		t.Error("expected false for missing key")
	}
	if data != nil {
		t.Error("expected nil data for missing key")
	}

	c.Put("key1", []byte("value1"))
	data, ok = c.Get("key2")
	if ok {
		t.Error("expected false for different key")
	}
	if data != nil {
		t.Error("expected nil data for different key")
	}
}

func TestWASMCache_Get_Hit(t *testing.T) {
	c := NewWASMCache(10, 1024)

	c.Put("key1", []byte("value1"))
	data, ok := c.Get("key1")
	if !ok {
		t.Fatal("expected true for existing key")
	}
	if string(data) != "value1" {
		t.Errorf("expected 'value1', got %q", string(data))
	}
}

func TestWASMCache_Get_LRUPromotion(t *testing.T) {
	// maxEntries=2: insert A, B, then Get A (promotes to front),
	// then insert C — B should be evicted, A and C remain.
	c := NewWASMCache(2, 0)

	c.Put("A", []byte{1})
	c.Put("B", []byte{2})

	// Get A to promote it in front of B.
	if _, ok := c.Get("A"); !ok {
		t.Fatal("expected to find A")
	}

	// Insert C — should evict B (now the oldest).
	c.Put("C", []byte{3})

	if c.Len() != 2 {
		t.Fatalf("expected Len=2, got %d", c.Len())
	}
	if _, ok := c.Get("A"); !ok {
		t.Error("A should still be in cache")
	}
	if _, ok := c.Get("B"); ok {
		t.Error("B should have been evicted")
	}
	if _, ok := c.Get("C"); !ok {
		t.Error("C should be in cache")
	}
}

func TestWASMCache_Put_Insert(t *testing.T) {
	c := NewWASMCache(10, 1024)

	c.Put("key1", []byte("hello"))
	if c.Len() != 1 {
		t.Errorf("expected Len=1, got %d", c.Len())
	}
	if c.TotalBytes() != 5 {
		t.Errorf("expected TotalBytes=5, got %d", c.TotalBytes())
	}

	c.Put("key2", []byte("world"))
	if c.Len() != 2 {
		t.Errorf("expected Len=2, got %d", c.Len())
	}
	if c.TotalBytes() != 10 {
		t.Errorf("expected TotalBytes=10, got %d", c.TotalBytes())
	}
}

func TestWASMCache_Put_Update(t *testing.T) {
	c := NewWASMCache(10, 1024)

	c.Put("key1", []byte("hello"))
	c.Put("key1", []byte("hi"))
	if c.Len() != 1 {
		t.Errorf("expected Len=1 after update, got %d", c.Len())
	}
	if c.TotalBytes() != 2 {
		t.Errorf("expected TotalBytes=2 after update, got %d", c.TotalBytes())
	}
	data, ok := c.Get("key1")
	if !ok {
		t.Fatal("expected key1 to exist")
	}
	if string(data) != "hi" {
		t.Errorf("expected updated value 'hi', got %q", string(data))
	}

	// Update with same-size data.
	c.Put("key1", []byte("ab"))
	if c.TotalBytes() != 2 {
		t.Errorf("expected TotalBytes=2 after same-size update, got %d", c.TotalBytes())
	}

	// Update with larger data.
	c.Put("key1", []byte("longer"))
	if c.TotalBytes() != 6 {
		t.Errorf("expected TotalBytes=6 after larger update, got %d", c.TotalBytes())
	}
}

func TestWASMCache_Put_EvictByCount(t *testing.T) {
	c := NewWASMCache(2, 0)

	c.Put("A", []byte{1})
	c.Put("B", []byte{2})
	c.Put("C", []byte{3})

	// Oldest (A) should be evicted.
	if c.Len() != 2 {
		t.Fatalf("expected Len=2, got %d", c.Len())
	}
	if _, ok := c.Get("A"); ok {
		t.Error("A should have been evicted")
	}
	if _, ok := c.Get("B"); !ok {
		t.Error("B should be present")
	}
	if _, ok := c.Get("C"); !ok {
		t.Error("C should be present")
	}
}

func TestWASMCache_Put_EvictByBytes(t *testing.T) {
	c := NewWASMCache(100, 20)

	c.Put("A", make([]byte, 10))
	c.Put("B", make([]byte, 8))
	// totalBytes = 18

	c.Put("C", make([]byte, 5))
	// totalBytes would be 23, evicts oldest (A, 10 bytes) → 13

	if c.Len() != 2 {
		t.Fatalf("expected Len=2, got %d", c.Len())
	}
	if c.TotalBytes() != 13 {
		t.Errorf("expected TotalBytes=13, got %d", c.TotalBytes())
	}
	if _, ok := c.Get("A"); ok {
		t.Error("A should have been evicted")
	}
	if _, ok := c.Get("B"); !ok {
		t.Error("B should be present")
	}
	if _, ok := c.Get("C"); !ok {
		t.Error("C should be present")
	}
}

func TestWASMCache_Put_EvictByBoth(t *testing.T) {
	c := NewWASMCache(2, 20)

	c.Put("A", make([]byte, 10)) // total=10, len=1
	c.Put("B", make([]byte, 10)) // total=20, len=2
	c.Put("C", make([]byte, 10)) // total=30, len=3 → evict oldest (A)

	// After evicting A: total=20, len=2 — both limits satisfied.
	if c.Len() != 2 {
		t.Fatalf("expected Len=2, got %d", c.Len())
	}
	if c.TotalBytes() != 20 {
		t.Errorf("expected TotalBytes=20, got %d", c.TotalBytes())
	}
	if _, ok := c.Get("A"); ok {
		t.Error("A should have been evicted")
	}
	if _, ok := c.Get("B"); !ok {
		t.Error("B should be present")
	}
	if _, ok := c.Get("C"); !ok {
		t.Error("C should be present")
	}
}

func TestWASMCache_Len(t *testing.T) {
	c := NewWASMCache(10, 1024)
	if c.Len() != 0 {
		t.Errorf("expected Len=0 for empty cache, got %d", c.Len())
	}

	for i := 0; i < 3; i++ {
		c.Put(string(rune('a'+i)), []byte{byte(i)})
	}
	if c.Len() != 3 {
		t.Errorf("expected Len=3, got %d", c.Len())
	}
}

func TestWASMCache_TotalBytes(t *testing.T) {
	c := NewWASMCache(10, 1024)
	if c.TotalBytes() != 0 {
		t.Errorf("expected TotalBytes=0 for empty cache, got %d", c.TotalBytes())
	}

	c.Put("a", []byte{1, 2, 3})
	c.Put("b", []byte{4, 5})
	expected := int64(5)
	if c.TotalBytes() != expected {
		t.Errorf("expected TotalBytes=%d, got %d", expected, c.TotalBytes())
	}
}

func TestWASMCache_Concurrent(t *testing.T) {
	c := NewWASMCache(100, 0)
	const numKeys = 50

	// Pre-populate with known keys.
	for i := 0; i < numKeys; i++ {
		c.Put(string(rune('A'+i%26))+string(rune('0'+i/26)), []byte{byte(i)})
	}

	var wg sync.WaitGroup
	// Concurrent reads — no mutations, deterministic result.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.Get(string(rune('A'+j%26)) + string(rune('0'+j/26)))
			}
		}()
	}
	wg.Wait()

	// State should be unchanged.
	if c.Len() != numKeys {
		t.Errorf("expected Len=%d, got %d", numKeys, c.Len())
	}
}

func TestWASMCache_Put_MaxEntriesZero(t *testing.T) {
	c := NewWASMCache(0, 0)

	c.Put("key1", []byte("data"))
	if c.Len() != 0 {
		t.Errorf("expected Len=0 with maxEntries=0, got %d", c.Len())
	}
	if c.TotalBytes() != 0 {
		t.Errorf("expected TotalBytes=0 with maxEntries=0, got %d", c.TotalBytes())
	}
	_, ok := c.Get("key1")
	if ok {
		t.Error("entry should have been evicted immediately")
	}
}

func TestWASMCache_Put_ZeroByteData(t *testing.T) {
	c := NewWASMCache(10, 1024)

	c.Put("empty", []byte{})
	if c.Len() != 1 {
		t.Errorf("expected Len=1, got %d", c.Len())
	}
	if c.TotalBytes() != 0 {
		t.Errorf("expected TotalBytes=0 for zero-byte data, got %d", c.TotalBytes())
	}
	data, ok := c.Get("empty")
	if !ok {
		t.Fatal("expected empty key to exist")
	}
	if len(data) != 0 {
		t.Errorf("expected empty data, got %v", data)
	}
}

func TestWASMCache_Put_EvictContinuesUntilBothLimitsSatisfied(t *testing.T) {
	// maxEntries=1, maxBytes=2: inserting a large entry after a small one
	// requires evicting multiple entries because a single eviction leaves
	// the byte limit still exceeded.
	c := NewWASMCache(1, 2)

	c.Put("A", make([]byte, 2)) // total=2, len=1 — both limits satisfied

	// Insert a 20-byte entry. This pushes:
	//   total=22 > 2, len=2 > 1 → evict A (2 bytes): total=20, len=1
	//   Loop: 1>1? No. 2>0 && 20>2? Yes → evict B (20 bytes): total=0, len=0
	// Both entries evicted, cache becomes empty.
	c.Put("B", make([]byte, 20))

	if c.Len() != 0 {
		t.Errorf("expected Len=0 after multi-eviction, got %d", c.Len())
	}
	if c.TotalBytes() != 0 {
		t.Errorf("expected TotalBytes=0 after multi-eviction, got %d", c.TotalBytes())
	}
	if _, ok := c.Get("A"); ok {
		t.Error("A should have been evicted")
	}
	if _, ok := c.Get("B"); ok {
		t.Error("B should have been evicted (too large for byte limit)")
	}
}
