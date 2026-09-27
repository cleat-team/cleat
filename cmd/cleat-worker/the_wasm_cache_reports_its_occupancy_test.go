package main

import "testing"

// TestWasmLRUCacheStats covers the accessor behind cleat_wasm_cache_entries and
// cleat_wasm_cache_bytes (cleat#1317).
//
// The gauges are the observable for --wasm-cache-max-entries and
// --wasm-cache-max-mb, so a wrong number here is worse than none: an operator
// sizing the cache would tune against it. Asserting BYTES and not just the
// count is the point -- a stats() returning len(index) and zero would satisfy
// any entries-only test while reporting an empty cache forever.
func TestWasmLRUCacheStats(t *testing.T) {
	c := newWasmLRUCache(10, 1)

	if ents, b := c.stats(); ents != 0 || b != 0 {
		t.Fatalf("a fresh cache reports %d entries / %d bytes, want 0/0", ents, b)
	}

	c.put("a", make([]byte, 100))
	c.put("b", make([]byte, 250))

	ents, bytes := c.stats()
	if ents != 2 {
		t.Errorf("entries = %d, want 2", ents)
	}
	if bytes != 350 {
		t.Errorf("bytes = %d, want 350 (100+250). A count-only implementation "+
			"would pass the entries check above and report an empty cache forever", bytes)
	}

	// Removal must move both numbers, not just the count.
	c.remove("a")
	ents, bytes = c.stats()
	if ents != 1 || bytes != 250 {
		t.Errorf("after removing a 100-byte entry: %d entries / %d bytes, want 1/250", ents, bytes)
	}
}
