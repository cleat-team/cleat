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

	var threshold = 100 * time.Millisecond
	if p50 > threshold {
		t.Errorf("P50 latency %.0fms exceeds threshold %v", p50.Seconds()*1000, threshold)
	}
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

	var threshold = 500 * time.Millisecond
	if p99 > threshold {
		t.Errorf("P99 latency %v exceeds threshold %v", p99, threshold)
	}
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
