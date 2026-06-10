package main

import (
	"sync"
	"testing"
)

func TestInflightRegistry_New(t *testing.T) {
	r := NewInflightRegistry()
	if r.Len() != 0 {
		t.Errorf("expected empty registry, got Len=%d", r.Len())
	}
}

func TestInflightRegistry_AddGet(t *testing.T) {
	r := NewInflightRegistry()
	r.Add("key1", "value1")
	r.Add("key2", 42)

	v, ok := r.Get("key1")
	if !ok {
		t.Fatal("expected key1 to exist")
	}
	if s, ok := v.(string); !ok || s != "value1" {
		t.Errorf("expected 'value1', got %v", v)
	}

	v, ok = r.Get("key2")
	if !ok {
		t.Fatal("expected key2 to exist")
	}
	if n, ok := v.(int); !ok || n != 42 {
		t.Errorf("expected 42, got %v", v)
	}
}

func TestInflightRegistry_Remove(t *testing.T) {
	r := NewInflightRegistry()
	r.Add("key1", "value1")
	r.Add("key2", "value2")

	r.Remove("key1")

	_, ok := r.Get("key1")
	if ok {
		t.Error("expected key1 to be removed")
	}

	// Other keys should still exist.
	v, ok := r.Get("key2")
	if !ok {
		t.Fatal("expected key2 to still exist")
	}
	if v != "value2" {
		t.Errorf("expected 'value2', got %v", v)
	}
}

func TestInflightRegistry_GetNonExistent(t *testing.T) {
	r := NewInflightRegistry()
	v, ok := r.Get("nonexistent")
	if ok {
		t.Error("expected ok=false for nonexistent key")
	}
	if v != nil {
		t.Errorf("expected nil value, got %v", v)
	}
}

func TestInflightRegistry_Len(t *testing.T) {
	r := NewInflightRegistry()
	if r.Len() != 0 {
		t.Errorf("expected 0, got %d", r.Len())
	}

	r.Add("a", 1)
	r.Add("b", 2)
	r.Add("c", 3)
	if r.Len() != 3 {
		t.Errorf("expected 3, got %d", r.Len())
	}

	r.Remove("a")
	if r.Len() != 2 {
		t.Errorf("expected 2 after remove, got %d", r.Len())
	}

	r.Remove("b")
	r.Remove("c")
	if r.Len() != 0 {
		t.Errorf("expected 0 after removing all, got %d", r.Len())
	}
}

func TestInflightRegistry_Range(t *testing.T) {
	r := NewInflightRegistry()
	r.Add("a", 1)
	r.Add("b", 2)
	r.Add("c", 3)

	seen := make(map[string]int)
	r.Range(func(key string, value any) bool {
		seen[key] = value.(int)
		return true
	})

	if len(seen) != 3 {
		t.Errorf("expected 3 items, got %d", len(seen))
	}
	if seen["a"] != 1 || seen["b"] != 2 || seen["c"] != 3 {
		t.Errorf("unexpected values: %v", seen)
	}
}

func TestInflightRegistry_RangeEarlyExit(t *testing.T) {
	r := NewInflightRegistry()
	r.Add("a", 1)
	r.Add("b", 2)
	r.Add("c", 3)

	count := 0
	r.Range(func(key string, value any) bool {
		count++
		return false // stop after first
	})

	if count != 1 {
		t.Errorf("expected 1 iteration, got %d", count)
	}
}

func TestInflightRegistry_ConcurrencySafe(t *testing.T) {
	r := NewInflightRegistry()
	var wg sync.WaitGroup

	// Concurrently add 100 items.
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			r.Add(string(rune('a'+n%26)), n)
		}(i)
	}

	// Concurrently read while writing.
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Range(func(_ string, _ any) bool { return true })
		}()
	}

	// Concurrently remove while writing and reading.
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Remove("nonexistent")
		}()
	}

	wg.Wait()
	// No panic = concurrency safe.
	_ = r.Len()
}
