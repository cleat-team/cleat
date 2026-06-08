package engine

import (
	"sync"
	"testing"
)

func TestNewWASMCache(t *testing.T) {
	c := NewWASMCache(100, 1<<20)
	if c == nil {
		t.Fatal("NewWASMCache returned nil")
	}
	if c.Len() != 0 {
		t.Errorf("Len() = %d, want 0", c.Len())
	}
	if c.TotalBytes() != 0 {
		t.Errorf("TotalBytes() = %d, want 0", c.TotalBytes())
	}
}

func TestWASMCache_Get_Miss(t *testing.T) {
	c := NewWASMCache(10, 1<<20)
	data, ok := c.Get("nonexistent")
	if ok {
		t.Error("Get on empty cache returned ok=true, want false")
	}
	if data != nil {
		t.Errorf("Get on empty cache returned %v, want nil", data)
	}
}

func TestWASMCache_Get_Hit(t *testing.T) {
	c := NewWASMCache(10, 1<<20)
	c.Put("key1", []byte("hello"))
	data, ok := c.Get("key1")
	if !ok {
		t.Fatal("Get returned ok=false, want true")
	}
	if string(data) != "hello" {
		t.Errorf("Get returned %q, want %q", data, "hello")
	}
}

func TestWASMCache_Get_PromotesLRU(t *testing.T) {
	// With maxEntries=2, insert 3 entries. Then Get the oldest
	// (which would be evicted), and verify it survives subsequent inserts.
	c := NewWASMCache(2, 1<<20)
	c.Put("a", []byte("a"))
	c.Put("b", []byte("b"))

	// Access "a" to make "b" the oldest.
	c.Get("a")

	// Inserting "c" should evict "b", not "a".
	c.Put("c", []byte("c"))

	if _, ok := c.Get("a"); !ok {
		t.Error("Get(\"a\") after promotion returned miss, want hit")
	}
	if _, ok := c.Get("b"); ok {
		t.Error("Get(\"b\") after eviction returned hit, want miss")
	}
}

func TestWASMCache_Put_Insert(t *testing.T) {
	c := NewWASMCache(10, 1<<20)
	c.Put("key1", []byte("data"))
	if c.Len() != 1 {
		t.Errorf("Len() = %d after insert, want 1", c.Len())
	}
	if c.TotalBytes() != 4 {
		t.Errorf("TotalBytes() = %d after insert, want 4", c.TotalBytes())
	}
}

func TestWASMCache_Put_Update(t *testing.T) {
	c := NewWASMCache(10, 1<<20)
	c.Put("key", []byte("abc"))   // 3 bytes
	c.Put("key", []byte("abcde")) // 5 bytes
	if c.Len() != 1 {
		t.Errorf("Len() = %d, want 1 (same key)", c.Len())
	}
	if c.TotalBytes() != 5 {
		t.Errorf("TotalBytes() = %d, want 5", c.TotalBytes())
	}
	data, ok := c.Get("key")
	if !ok || string(data) != "abcde" {
		t.Errorf("Get returned %q, %v; want %q", data, ok, "abcde")
	}
}

func TestWASMCache_EvictByCount(t *testing.T) {
	c := NewWASMCache(3, 1<<20)
	c.Put("a", []byte("a"))
	c.Put("b", []byte("b"))
	c.Put("c", []byte("c"))
	c.Put("d", []byte("d")) // should evict "a"

	if c.Len() != 3 {
		t.Errorf("Len() = %d, want 3", c.Len())
	}
	if _, ok := c.Get("a"); ok {
		t.Error("Get(\"a\") returned hit, want miss (evicted)")
	}
	for _, k := range []string{"b", "c", "d"} {
		if _, ok := c.Get(k); !ok {
			t.Errorf("Get(%q) returned miss, want hit", k)
		}
	}
}

func TestWASMCache_EvictByBytes(t *testing.T) {
	c := NewWASMCache(100, 10) // 10 byte limit
	c.Put("a", []byte("12345"))    // 5 bytes
	c.Put("b", []byte("67890"))    // 5 bytes, total=10 OK
	c.Put("c", []byte("!"))        // 1 byte → total=11 > 10, evict

	if c.TotalBytes() > 10 {
		t.Errorf("TotalBytes() = %d, want <= 10", c.TotalBytes())
	}
}

func TestWASMCache_MaxBytesZero(t *testing.T) {
	// maxBytes=0 means unlimited bytes; only entry count matters.
	c := NewWASMCache(2, 0)
	c.Put("a", make([]byte, 1000000)) // large
	c.Put("b", make([]byte, 1000000))
	if c.Len() != 2 {
		t.Errorf("Len() = %d, want 2", c.Len())
	}
	c.Put("c", make([]byte, 1000000)) // evicts oldest by count
	if c.Len() != 2 {
		t.Errorf("Len() = %d after 3rd insert, want 2", c.Len())
	}
}

func TestWASMCache_MaxEntriesZero(t *testing.T) {
	// maxEntries=0 means every Put immediately evicts.
	c := NewWASMCache(0, 1<<20)
	c.Put("a", []byte("a"))
	if c.Len() != 0 {
		t.Errorf("Len() = %d, want 0 (evicted immediately)", c.Len())
	}
}

func TestWASMCache_UpdateExistingZeroLength(t *testing.T) {
	c := NewWASMCache(10, 1<<20)
	c.Put("key", []byte("hello")) // 5 bytes
	c.Put("key", []byte(""))      // 0 bytes
	if c.TotalBytes() != 0 {
		t.Errorf("TotalBytes() = %d, want 0", c.TotalBytes())
	}
	data, ok := c.Get("key")
	if !ok || string(data) != "" {
		t.Errorf("Get returned %q, %v; want %q", data, ok, "")
	}
}

func TestWASMCache_Concurrent(t *testing.T) {
	c := NewWASMCache(50, 1<<20)
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			key := string(rune('A' + (id % 26)))
			c.Put(key, []byte{byte(id)})
			c.Get(key)
			c.Len()
			c.TotalBytes()
		}(i)
	}
	wg.Wait()
	// No assertion needed — success is no panic.
}
