package main

import (
	"testing"
	"time"
)

// cleat#3540. The byte cache was keyed by "tenant/name:version" and each key held
// its own copy, so a library installed by N tenants was N copies of up to ~19MB
// and the byte budget evicted real work to hold duplicates. The bytes are now
// shared by content: entries stay per tenant (each is revalidated against its own
// row), and a blob is looked up by the SHA-256 the WORKER computed of the bytes
// it holds.

func sharedModule(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i)*31 + seed
	}
	return b
}

func TestWasmCacheHoldsIdenticalBytesOnceAcrossTenants(t *testing.T) {
	c := newWasmLRUCache(100, 8)
	module := sharedModule(4096, 1)

	const tenants = 5
	for i := 0; i < tenants; i++ {
		// A fresh slice per tenant, as five reads from the store would be.
		c.put(string(rune('a'+i))+"/lib-def:1", append([]byte(nil), module...))
	}

	entries, bytes := c.stats()
	if entries != tenants {
		t.Errorf("entries = %d, want %d: sharing is of the bytes, each tenant keeps its own entry", entries, tenants)
	}
	if bytes != int64(len(module)) {
		t.Errorf("bytes = %d, want %d: %d tenants installed one library version and the cache holds it %.1f times",
			bytes, len(module), tenants, float64(bytes)/float64(len(module)))
	}
	first, _ := c.get("a/lib-def:1")
	last, _ := c.get("e/lib-def:1")
	if &first[0] != &last[0] {
		t.Error("two tenants' entries are over different backing arrays: the bytes are copies, not shared")
	}
}

func TestWasmCacheKeepsDifferentBytesApart(t *testing.T) {
	c := newWasmLRUCache(100, 8)
	one, two := sharedModule(4096, 1), sharedModule(4096, 2)
	c.put("a/def:1", one)
	c.put("b/def:1", two)

	// Both entries are marked as the store's agreed digest, the SAME string. A
	// blob found by what the store REPORTED would now serve one tenant the other's
	// bytes. It is found by the worker's own hash of the bytes, so it cannot.
	c.markChecked("a/def:1", "the-same-digest", time.Now())
	c.markChecked("b/def:1", "the-same-digest", time.Now())

	if got, _ := c.get("a/def:1"); string(got) != string(one) {
		t.Error("tenant a was served bytes that are not its own")
	}
	if got, _ := c.get("b/def:1"); string(got) != string(two) {
		t.Error("tenant b was served bytes that are not its own")
	}
	if _, bytes := c.stats(); bytes != int64(len(one)+len(two)) {
		t.Errorf("bytes = %d, want %d: different bytes are different blobs", bytes, len(one)+len(two))
	}
}

func TestWasmCacheFreesABlobWithItsLastEntry(t *testing.T) {
	c := newWasmLRUCache(100, 8)
	module := sharedModule(4096, 3)
	c.put("a/def:1", module)
	c.put("b/def:1", module)

	c.remove("a/def:1")
	if _, bytes := c.stats(); bytes != int64(len(module)) {
		t.Errorf("after one of two holders left: %d bytes, want %d. The other tenant still needs them", bytes, len(module))
	}
	if got, ok := c.get("b/def:1"); !ok || string(got) != string(module) {
		t.Error("removing one tenant's entry took the shared bytes from the other")
	}
	c.remove("b/def:1")
	if ents, bytes := c.stats(); ents != 0 || bytes != 0 {
		t.Errorf("after the last holder left: %d entries / %d bytes, want 0/0: the bytes leaked", ents, bytes)
	}
}

// REPLACING one tenant's definition must release its old bytes and leave every
// other holder of them alone.
func TestWasmCacheReplacementReleasesOnlyThatEntrysShare(t *testing.T) {
	c := newWasmLRUCache(100, 8)
	old, fresh := sharedModule(4096, 4), sharedModule(2048, 5)
	c.put("a/def:1", old)
	c.put("b/def:1", old)

	c.put("a/def:1", fresh) // tenant a redeploys different bytes

	if got, _ := c.get("b/def:1"); string(got) != string(old) {
		t.Error("tenant a's replacement changed what tenant b is served")
	}
	if got, _ := c.get("a/def:1"); string(got) != string(fresh) {
		t.Error("tenant a was not given its replacement")
	}
	if _, bytes := c.stats(); bytes != int64(len(old)+len(fresh)) {
		t.Errorf("bytes = %d, want %d (the old bytes, still held by b, plus the new)", bytes, len(old)+len(fresh))
	}
	// A replacement is unverified until the store is asked again.
	if digest, _ := c.verification("a/def:1"); digest != "" {
		t.Errorf("a replaced entry kept the previous bytes' verification (%q)", digest)
	}
}

// THE BENEFIT, as the byte budget sees it. A budget that holds one 400KB module
// twice cannot hold it three times; shared, it holds it for any number of tenants.
func TestWasmCacheByteBudgetCountsASharedBlobOnce(t *testing.T) {
	c := newWasmLRUCache(100, 1) // 1 MiB
	module := sharedModule(400*1024, 6)

	for i := 0; i < 10; i++ {
		c.put(string(rune('a'+i))+"/lib-def:1", append([]byte(nil), module...))
	}
	if entries, _ := c.stats(); entries != 10 {
		t.Errorf("entries = %d, want 10: ten tenants' entries over one 400KB blob fit a 1MiB budget, and the cache evicted %d "+
			"of them to hold copies", entries, 10-entries)
	}
	// And a different module still competes for the budget as before.
	c.put("z/other:1", sharedModule(400*1024, 7))
	c.put("y/other:1", sharedModule(400*1024, 8))
	if _, bytes := c.stats(); bytes > 1024*1024 {
		t.Errorf("bytes = %d exceeds the 1MiB budget", bytes)
	}
}

// An entry larger than the whole budget is held rather than looping forever
// looking for room that cannot exist.
func TestWasmCacheAnOversizeEntryIsHeldNotSpunOn(t *testing.T) {
	c := newWasmLRUCache(100, 1)
	done := make(chan struct{})
	go func() {
		c.put("a/huge:1", sharedModule(2*1024*1024, 9))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("put never returned for an entry larger than the byte budget")
	}
	if got, ok := c.get("a/huge:1"); !ok || len(got) != 2*1024*1024 {
		t.Error("the oversize entry was not held")
	}
}

// AT THE WORKER: two tenants' stores hold one library member, each read through
// its own store (cleat#1931), and the worker's cache ends up with one copy.
func TestLoadWASM_TwoTenantsOfOneLibraryShareOneCopy(t *testing.T) {
	member := sharedModule(8192, 10)
	storeA := newWasmProbeStore(tenantSelf, map[string][]byte{"lib-def": member})
	storeB := newWasmProbeStore(tenantOther, map[string][]byte{"lib-def": member})
	w := newTestWorker(storeA.mockStore)
	defer w.cancel()
	w.storeTenantID = tenantSelf

	a, err := w.loadWASM(storeA.mockStore, tenantSelf, "lib-def", 1)
	if err != nil {
		t.Fatalf("tenant A: %v", err)
	}
	b, err := w.loadWASM(storeB.mockStore, tenantOther, "lib-def", 1)
	if err != nil {
		t.Fatalf("tenant B: %v", err)
	}
	if string(a) != string(member) || string(b) != string(member) {
		t.Fatal("a tenant was not given the member's bytes")
	}
	if entries, bytes := w.wasmCache.stats(); entries != 2 || bytes != int64(len(member)) {
		t.Errorf("the cache holds %d entries / %d bytes for two tenants of one %d-byte member, want 2 / %d",
			entries, bytes, len(member), len(member))
	}
	// Each tenant is still validated against ITS OWN store: both were asked.
	if len(storeA.asked) == 0 || len(storeB.asked) == 0 {
		t.Error("a tenant's bytes were obtained without asking that tenant's store")
	}
}
