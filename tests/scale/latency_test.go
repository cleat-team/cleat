package scale

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// TestLatencyP50 measures the median (P50) latency for a single event append
// in a 1-step workflow.
func TestLatencyP50(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := engine.NewPostgresStore(db, suiteQueue)
	ctx := context.Background()

	const numSamples = 100
	latencies := make([]time.Duration, numSamples)

	for i := 0; i < numSamples; i++ {
		// Create a workflow instance for this sample.
		id := fmt.Sprintf("scale-lat-p50-%d-%d", i, time.Now().UnixNano())
		_, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
			VALUES ($1, 'test', 1, 'ready', '{}', '`+suiteQueue+`') ON CONFLICT DO NOTHING`, id)
		if err != nil {
			t.Fatalf("create workflow %d: %v", i, err)
		}
		defer db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, id)
		defer db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id)

		rec := engine.EventRecord{
			Step:      0,
			EventType: engine.EventTypeCall,
			Service:   "svc",
			Op:        "op",
			Request:   `{}`,
			Response:  `{"ok":true}`,
		}

		start := time.Now()
		if err := store.AppendEventHistory(ctx, id, rec); err != nil {
			t.Fatalf("append event %d: %v", i, err)
		}
		latencies[i] = time.Since(start)
	}

	// Sort latencies and compute P50 (median).
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p50 := latencies[numSamples/2]
	mean := meanDuration(latencies)

	t.Logf("Latency P50 test (%d samples):", numSamples)
	t.Logf("  P50 (median): %v", p50)
	t.Logf("  Mean: %v", mean)
	t.Logf("  Min: %v", latencies[0])
	t.Logf("  Max: %v", latencies[numSamples-1])

	// No wall-clock assertion here. See the comment on TestLatencyP99 for the
	// measurement that removed it from both tests; this one carried a 100ms
	// P50 threshold, and its own Max ran to 219ms on the run that failed.
	//
	// What is asserted instead is that every sample was actually measured. A
	// t.Fatalf above stops the test on an append error, so a zero here would
	// mean a sample that never ran, and zeros sort to the front and pull the
	// median DOWN -- a measurement hole that reads as "fast".
	assertAllSampled(t, latencies)
}

// TestLatencyP99 measures the 99th percentile latency for event appends under
// moderate load.
func TestLatencyP99(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := engine.NewPostgresStore(db, suiteQueue)
	ctx := context.Background()

	const numSamples = 200
	latencies := make([]time.Duration, numSamples)
	var wg sync.WaitGroup
	var mu sync.Mutex

	// Generate moderate load by appending with 4 concurrent goroutines.
	sem := make(chan struct{}, 4)
	for i := 0; i < numSamples; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()

			id := fmt.Sprintf("scale-lat-p99-%d-%d", idx, time.Now().UnixNano())
			_, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
				VALUES ($1, 'test', 1, 'ready', '{}', '`+suiteQueue+`') ON CONFLICT DO NOTHING`, id)
			if err != nil {
				t.Errorf("create workflow %d: %v", idx, err)
				return
			}
			defer db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, id)
			defer db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id)

			rec := engine.EventRecord{
				Step:      0,
				EventType: engine.EventTypeCall,
				Service:   "svc",
				Op:        "op",
				Request:   `{}`,
				Response:  `{"ok":true}`,
			}

			start := time.Now()
			if err := store.AppendEventHistory(ctx, id, rec); err != nil {
				t.Errorf("append event %d: %v", idx, err)
				return
			}
			d := time.Since(start)

			mu.Lock()
			latencies[idx] = d
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	// Sort and compute P99.
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p99Idx := int(math.Ceil(float64(numSamples)*0.99)) - 1
	if p99Idx >= numSamples {
		p99Idx = numSamples - 1
	}
	p99 := latencies[p99Idx]
	p50 := latencies[numSamples/2]

	t.Logf("Latency P99 test (%d samples under load):", numSamples)
	t.Logf("  P50: %v", p50)
	t.Logf("  P99: %v", p99)
	t.Logf("  Min: %v", latencies[0])
	t.Logf("  Max: %v", latencies[numSamples-1])

	// This test carried `if p99 > 500ms { t.Errorf(...) }` until 2026-09-04,
	// when it failed on develop at 491a0f7 (#720) with a P99 of 623.876463ms
	// over a P50 of 2.676839ms. The threshold is removed rather than widened,
	// per CLAUDE.md: "If an assertion depends on wall-clock time, remove the
	// timing rather than widening it."
	//
	// It was removed rather than repaired because the stall is not ours. The
	// first reading of that failure was that a pair of near-identical outliers
	// (623.876ms and 625.203ms) over a 2.7ms median looked like a fixed stall
	// -- a lock wait, a retry backoff, a pool timeout -- rather than runner
	// noise. Three measurements over the scale job's last 20 develop runs say
	// otherwise:
	//
	//   1. The magnitude is not fixed. P99 across those runs is a continuum
	//      spanning three orders of magnitude, with no cluster at 624ms:
	//      4.4, 4.8, 5.2, 5.2, 5.9, 6.0, 6.4, 6.5, 7.3, 13.7, 14.5, 15.1,
	//      15.2, 16.2, 18.4, 27.1, 33.0, 92.1, 377.4, 623.9 (ms).
	//      Median 14.1ms, max 44x that.
	//   2. The "near-identical pair" recurs at other magnitudes -- b35c52f's
	//      top two were 377.4ms and 387.9ms. Four goroutines are in flight at
	//      once here, so whatever is in flight during one host stall window
	//      records that window's length. The pair is a signature of the
	//      concurrency, not of a constant in the code.
	//   3. TestLatencyP50 above is SEQUENTIAL -- one goroutine, no lock
	//      contention, no pool competition, no retry path on this code path at
	//      all -- and shows the same tail (219.3ms max on the failing run,
	//      126.9ms on b35c52f). Across the 20 runs its Max correlates with
	//      this test's Max at Pearson r = 0.937. No lock or backoff inside
	//      AppendEventHistory can make a single-goroutine test slow.
	//
	// So the tail is a per-run property of the CI host, and any fixed
	// threshold under ~700ms is below its noise floor. Re-derive with
	// scripts/scale-latency-history.py, which reproduces all three.
	//
	// TestLatencyUnderConcurrency below already worked this way: it measures,
	// logs, and asserts nothing about the clock. These two now match it.
	assertAllSampled(t, latencies)
}

// TestLatencyUnderConcurrency measures latency as the number of concurrent
// workflows increases.
func TestLatencyUnderConcurrency(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := engine.NewPostgresStore(db, suiteQueue)
	ctx := context.Background()

	concurrencyLevels := []int{1, 5, 10, 25, 50}
	const eventsPerWF = 5

	for _, conc := range concurrencyLevels {
		t.Run(fmt.Sprintf("concurrency-%d", conc), func(t *testing.T) {
			latencies := make([]time.Duration, 0, conc*eventsPerWF)
			var mu sync.Mutex
			var wg sync.WaitGroup

			for i := 0; i < conc; i++ {
				wg.Add(1)
				go func(idx int) {
					defer wg.Done()

					id := fmt.Sprintf("scale-lat-conc-%d-%d-%d", conc, idx, time.Now().UnixNano())
					_, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
						VALUES ($1, 'test', 1, 'ready', '{}', '`+suiteQueue+`') ON CONFLICT DO NOTHING`, id)
					if err != nil {
						t.Errorf("create workflow: %v", err)
						return
					}
					defer db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, id)
					defer db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id)

					for step := 0; step < eventsPerWF; step++ {
						rec := engine.EventRecord{
							Step:      step,
							EventType: engine.EventTypeCall,
							Service:   "svc",
							Op:        "op",
							Request:   `{}`,
							Response:  `{"ok":true}`,
						}
						start := time.Now()
						if err := store.AppendEventHistory(ctx, id, rec); err != nil {
							t.Errorf("append event: %v", err)
							return
						}
						mu.Lock()
						latencies = append(latencies, time.Since(start))
						mu.Unlock()
					}
				}(i)
			}
			wg.Wait()

			if len(latencies) == 0 {
				t.Fatal("no latencies recorded")
			}

			sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
			p50 := latencies[len(latencies)/2]
			p99Idx := int(math.Ceil(float64(len(latencies))*0.99)) - 1
			if p99Idx >= len(latencies) {
				p99Idx = len(latencies) - 1
			}
			p99 := latencies[p99Idx]
			mean := meanDuration(latencies)

			t.Logf("  Concurrency=%d: P50=%v P99=%v mean=%v samples=%d",
				conc, p50, p99, mean, len(latencies))
		})
	}
}

// assertAllSampled fails if any slot in a fixed-size latency slice was never
// written, which is what a zero duration means here: no append completes in
// 0ns, and the slices are allocated at full length before the samples are
// taken.
//
// This replaces the wall-clock thresholds these two tests used to carry, and
// unlike them it does not depend on how fast the host is. It closes a real
// hole rather than restating one the appends already check: in
// TestLatencyP99 a goroutine that fails its INSERT calls t.Errorf and returns
// without ever writing latencies[idx], and a zero left behind sorts to the
// FRONT, pulling both the median and the P99 down. A run that measured fewer
// samples than it claimed would report itself as faster, not as broken.
func assertAllSampled(t *testing.T, latencies []time.Duration) {
	t.Helper()
	var unsampled int
	for _, d := range latencies {
		if d == 0 {
			unsampled++
		}
	}
	if unsampled > 0 {
		t.Errorf("%d of %d samples were never recorded; the percentiles above "+
			"are computed over zeros and are lower than the truth",
			unsampled, len(latencies))
	}
}

// meanDuration computes the arithmetic mean of a duration slice.
func meanDuration(durations []time.Duration) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	var total time.Duration
	for _, d := range durations {
		total += d
	}
	return total / time.Duration(len(durations))
}
