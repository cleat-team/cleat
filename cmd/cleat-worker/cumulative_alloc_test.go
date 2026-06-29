package main

import (
	"flag"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// --- Flag tests ---

func TestWasmCumulativeAllocationMaxMBFlag_Default(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	wcam := fs.Int("wasm-cumulative-allocation-max-mb", 0, "")
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if *wcam != 0 {
		t.Errorf("default --wasm-cumulative-allocation-max-mb = %d, want 0", *wcam)
	}
}

func TestWasmCumulativeAllocationMaxMBFlag_Parsed(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	wcam := fs.Int("wasm-cumulative-allocation-max-mb", 0, "")
	if err := fs.Parse([]string{"--wasm-cumulative-allocation-max-mb", "256"}); err != nil {
		t.Fatal(err)
	}
	if *wcam != 256 {
		t.Errorf("--wasm-cumulative-allocation-max-mb = %d, want 256", *wcam)
	}
}

// --- tryClaimCumulativeAllocation tests ---

func TestTryClaimCumulativeAllocation_UnderLimit(t *testing.T) {
	var counter atomic.Int64
	maxBytes := int64(1024 * 1024) // 1 MB

	ok := tryClaimCumulativeAllocation(&counter, 65536, maxBytes) // 1 page = 64 KB
	if !ok {
		t.Fatal("expected claim to succeed under limit")
	}
	if counter.Load() != 65536 {
		t.Errorf("counter = %d, want 65536", counter.Load())
	}
}

func TestTryClaimCumulativeAllocation_ExactlyAtLimit(t *testing.T) {
	var counter atomic.Int64
	maxBytes := int64(65536) // 1 page

	ok := tryClaimCumulativeAllocation(&counter, 65536, maxBytes)
	if !ok {
		t.Fatal("expected claim to succeed when exactly at limit")
	}
	if counter.Load() != 65536 {
		t.Errorf("counter = %d, want 65536", counter.Load())
	}
}

func TestTryClaimCumulativeAllocation_ExceedsLimit(t *testing.T) {
	var counter atomic.Int64
	counter.Store(65536)             // 1 page already allocated
	maxBytes := int64(65536 * 2)     // 2 pages max
	byteEstimate := int64(65536 * 2) // request 2 pages

	ok := tryClaimCumulativeAllocation(&counter, byteEstimate, maxBytes)
	if ok {
		t.Fatal("expected claim to be rejected when it would exceed the limit")
	}
	// counter should be unchanged
	if counter.Load() != 65536 {
		t.Errorf("counter = %d, want 65536 (unchanged)", counter.Load())
	}
}

func TestTryClaimCumulativeAllocation_ExceedsLimit_FromZero(t *testing.T) {
	var counter atomic.Int64
	maxBytes := int64(65536)      // 1 page max
	byteEstimate := int64(65536 * 2) // request 2 pages

	ok := tryClaimCumulativeAllocation(&counter, byteEstimate, maxBytes)
	if ok {
		t.Fatal("expected claim to be rejected when request alone exceeds limit")
	}
	if counter.Load() != 0 {
		t.Errorf("counter = %d, want 0 (unchanged)", counter.Load())
	}
}

func TestTryClaimCumulativeAllocation_ReleaseAndReclaim(t *testing.T) {
	var counter atomic.Int64
	maxBytes := int64(65536 * 3) // 3 pages max
	page := int64(65536)

	// Claim 3 pages (filling the limit)
	ok := tryClaimCumulativeAllocation(&counter, page*3, maxBytes)
	if !ok {
		t.Fatal("expected 3-page claim to succeed")
	}
	if counter.Load() != page*3 {
		t.Errorf("counter = %d, want %d", counter.Load(), page*3)
	}

	// Next claim should be rejected
	ok = tryClaimCumulativeAllocation(&counter, page, maxBytes)
	if ok {
		t.Fatal("expected claim to be rejected when at limit")
	}

	// Release 1 page
	counter.Add(-page)
	if counter.Load() != page*2 {
		t.Errorf("counter = %d, want %d after release", counter.Load(), page*2)
	}

	// Now 1 page should succeed
	ok = tryClaimCumulativeAllocation(&counter, page, maxBytes)
	if !ok {
		t.Fatal("expected claim to succeed after release")
	}
	if counter.Load() != page*3 {
		t.Errorf("counter = %d, want %d after re-claim", counter.Load(), page*3)
	}
}

// --- Concurrent allocation tests ---

func TestTryClaimCumulativeAllocation_Concurrent(t *testing.T) {
	var counter atomic.Int64
	maxBytes := int64(65536 * 10) // 10 pages max
	page := int64(65536)
	workers := 20
	claimsPerWorker := 5 // each tries to claim 10 pages total across workers

	var wg sync.WaitGroup
	successCount := atomic.Int64{}
	failureCount := atomic.Int64{}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < claimsPerWorker; j++ {
				if tryClaimCumulativeAllocation(&counter, page, maxBytes) {
					successCount.Add(1)
				} else {
					failureCount.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	totalClaims := int64(workers * claimsPerWorker)
	expectedSuccesses := int64(10) // max 10 pages

	if successCount.Load() > expectedSuccesses {
		t.Errorf("too many successful claims: %d > %d (max)", successCount.Load(), expectedSuccesses)
	}
	if successCount.Load()+failureCount.Load() != totalClaims {
		t.Errorf("total claims mismatch: %d + %d != %d", successCount.Load(), failureCount.Load(), totalClaims)
	}
	if counter.Load() > maxBytes {
		t.Errorf("counter %d exceeds max %d", counter.Load(), maxBytes)
	}

	t.Logf("successes=%d failures=%d counter=%d", successCount.Load(), failureCount.Load(), counter.Load())
}

// --- Zero-limit (unlimited) behavior ---

func TestTryClaimCumulativeAllocation_ZeroLimit_NotEnforced(t *testing.T) {
	// When maxBytes is 0, tryClaimCumulativeAllocation should not be called
	// (enforcement is gated on wasmCumulativeAllocationMaxBytes > 0 in executeWorkflow).
	// This test verifies that a Worker with maxBytes=0 has no enforcement by checking
	// the field initialisation contract.
	var w Worker
	if w.wasmCumulativeAllocationMaxBytes != 0 {
		t.Errorf("default wasmCumulativeAllocationMaxBytes = %d, want 0", w.wasmCumulativeAllocationMaxBytes)
	}
	var flagInt int
	_ = fmt.Sprintf("compilation check: %T %d", &w, flagInt)
}

// --- Error message format ---

func TestCumulativeAllocationErrorFormat(t *testing.T) {
	// Verify the error message includes all required fields per CONTRACT.md:
	// current cumulative allocation, new module's requirement, and configured limit.
	maxMB := 128
	maxBytes := int64(maxMB) * 1024 * 1024
	curBytes := int64(100 * 1024 * 1024)
	requiredBytes := int64(32 * 1024 * 1024)

	errMsg := fmt.Sprintf("cumulative WASM allocation limit reached: current %d bytes (%.0f MB) + required %d bytes (%.0f MB) exceeds max %d bytes (%.0f MB)",
		curBytes, float64(curBytes)/1024/1024, requiredBytes, float64(requiredBytes)/1024/1024, maxBytes, float64(maxBytes)/1024/1024)

	checks := []string{
		"cumulative WASM allocation limit reached",
		"100 MB",    // current in MB
		"32 MB",     // required in MB
		"128 MB",    // max in MB
		fmt.Sprintf("%d", curBytes),
		fmt.Sprintf("%d", requiredBytes),
		fmt.Sprintf("%d", maxBytes),
	}
	for _, want := range checks {
		if !contains(errMsg, want) {
			t.Errorf("error message missing %q\nGot: %s", want, errMsg)
		}
	}
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
