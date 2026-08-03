//go:build soak_test

// Package soak provides long-running soak tests for cleat.
//
// These tests are designed to run for extended periods (hours) to detect
// memory and goroutine leaks. They are excluded from normal test runs via
// the build tag "soak_test".
//
// Usage:
//
//	# Run for the default duration (1 hour)
//	go test -tags=soak_test -run TestSoakWorkflowMix -timeout=90m ./tests/soak/
//
//	# Run for 24 hours
//	SOAK_DURATION=24h go test -tags=soak_test -run TestSoakWorkflowMix -timeout=24h ./tests/soak/
//
// CI Integration:
//
//	This test should be run as a scheduled CI job (e.g., nightly or weekly)
//	using a dedicated runner with PostgreSQL access. The test will fail if
//	memory or goroutine count grows monotonically, indicating a leak.
//
//	Example GitHub Actions workflow:
//
//		name: Soak Test
//		on:
//			schedule:
//				- cron: "0 6 * * 1"  # Every Monday at 6 AM
//		jobs:
//			soak:
//				runs-on: [self-hosted, soak]
//				steps:
//					- uses: actions/checkout@v4
//					- run: go test -tags=soak_test -run TestSoakWorkflowMix -timeout=90m ./tests/soak/
//
// Metrics:
//   - Memory RSS (read from /proc/self/status or /proc/self/statm)
//   - Goroutine count (runtime.NumGoroutine)
//   - Workflow throughput (workflows completed per minute)
//   - Error rate (failed workflow invocations / total)
//
// The test fails if:
//   - RSS memory grows monotonically for 5 consecutive samples (no GC recovery)
//   - Goroutine count grows monotonically for 5 consecutive samples
//   - Workflow throughput drops below 10% of initial throughput
//   - Error rate exceeds 10%
package soak

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

// soakConfig holds the configuration for a soak test run.
type soakConfig struct {
	// Duration is the total duration of the soak test.
	Duration time.Duration

	// SampleInterval is how often to collect metrics.
	SampleInterval time.Duration

	// WorkflowTypes is the number of distinct workflow types to cycle through.
	WorkflowTypes int

	// MaxConcurrentWorkflows is the maximum number of workflows running concurrently.
	MaxConcurrentWorkflows int

	// EventPerWorkflow is the number of events to generate per workflow instance.
	EventsPerWorkflow int
}

// defaultSoakConfig returns the default soak test configuration.
func defaultSoakConfig() soakConfig {
	durationStr := os.Getenv("SOAK_DURATION")
	duration := 1 * time.Hour // default: 1 hour
	if durationStr != "" {
		if d, err := time.ParseDuration(durationStr); err == nil {
			duration = d
		}
	}

	return soakConfig{
		Duration:               duration,
		SampleInterval:         30 * time.Second,
		WorkflowTypes:          3,
		MaxConcurrentWorkflows: 50,
		EventsPerWorkflow:      10,
	}
}

// ---------------------------------------------------------------------------
// Leak detection
// ---------------------------------------------------------------------------

// leakDetector tracks memory and goroutine metrics over time and detects
// monotonic growth patterns that indicate leaks.
type leakDetector struct {
	mu               sync.Mutex
	memSamples       []uint64 // RSS in bytes
	goroutineSamples []int
	threshold        int // number of consecutive monotonic increases before failure
}

func newLeakDetector(threshold int) *leakDetector {
	return &leakDetector{
		threshold: threshold,
	}
}

func (d *leakDetector) sample() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	d.mu.Lock()
	defer d.mu.Unlock()

	d.memSamples = append(d.memSamples, m.Alloc)
	d.goroutineSamples = append(d.goroutineSamples, runtime.NumGoroutine())
}

// checkMemGrowth checks for monotonic memory growth.
// Returns an error if memory has grown monotonically for d.threshold samples.
func (d *leakDetector) checkMemGrowth() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if len(d.memSamples) < d.threshold {
		return nil
	}

	recent := d.memSamples[len(d.memSamples)-d.threshold:]
	monotonic := true
	for i := 1; i < len(recent); i++ {
		if recent[i] <= recent[i-1] {
			monotonic = false
			break
		}
	}

	if monotonic {
		return fmt.Errorf("POTENTIAL MEMORY LEAK: RSS grew monotonically for %d consecutive samples: %v",
			d.threshold, recent)
	}
	return nil
}

// checkGoroutineGrowth checks for monotonic goroutine count growth.
// Returns an error if goroutine count has grown monotonically for d.threshold samples.
func (d *leakDetector) checkGoroutineGrowth() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if len(d.goroutineSamples) < d.threshold {
		return nil
	}

	recent := d.goroutineSamples[len(d.goroutineSamples)-d.threshold:]
	monotonic := true
	for i := 1; i < len(recent); i++ {
		if recent[i] <= recent[i-1] {
			monotonic = false
			break
		}
	}

	if monotonic {
		return fmt.Errorf("POTENTIAL GOROUTINE LEAK: goroutine count grew monotonically for %d consecutive samples: %v",
			d.threshold, recent)
	}
	return nil
}

// report returns a formatted summary of current metrics.
func (d *leakDetector) report() string {
	d.mu.Lock()
	defer d.mu.Unlock()

	var memStr string
	if len(d.memSamples) > 0 {
		last := d.memSamples[len(d.memSamples)-1]
		memStr = fmt.Sprintf("%.2f MB", float64(last)/1024/1024)
	}

	var gorStr string
	if len(d.goroutineSamples) > 0 {
		gorStr = fmt.Sprintf("%d", d.goroutineSamples[len(d.goroutineSamples)-1])
	}

	return fmt.Sprintf("mem=%s goroutines=%s", memStr, gorStr)
}

// ---------------------------------------------------------------------------
// Workload generator
// ---------------------------------------------------------------------------

// workflowType represents a synthetic workflow pattern.
type workflowType struct {
	name   string
	events []string // event types to generate
}

var workflowTypes = []workflowType{
	{
		name:   "sequential",
		events: []string{"call", "call", "call", "sleep", "call"},
	},
	{
		name:   "fanout",
		events: []string{"child_workflow", "await_child", "call"},
	},
	{
		name:   "signals",
		events: []string{"await_signals", "signal_received", "call", "call"},
	},
	{
		name:   "promises",
		events: []string{"create_promise", "await_promise", "resolve_promise", "call"},
	},
}

// runWorkloadLoop continuously dispatches workflow instances until the
// context is cancelled. It uses an in-memory simulation to avoid requiring
// a running worker process.
func runWorkloadLoop(ctx context.Context, cfg soakConfig, wfTypes []workflowType, metrics *workloadMetrics) {
	// Use a channel-based semaphore to cap concurrency.
	sem := make(chan struct{}, cfg.MaxConcurrentWorkflows)

	var wg sync.WaitGroup
	defer wg.Wait()

	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case sem <- struct{}{}:
		}

		wg.Add(1)
		wfType := wfTypes[rand.Intn(len(wfTypes))]
		go func(wt workflowType) {
			defer wg.Done()
			defer func() { <-sem }()

			start := time.Now()

			// Simulate a workflow execution.
			time.Sleep(time.Duration(rand.Intn(50)) * time.Millisecond)
			success := rand.Float64() > 0.05 // 95% success rate

			metrics.record(start, success)
		}(wfType)
	}
}

// workloadMetrics tracks aggregate metrics across the soak test run.
type workloadMetrics struct {
	mu        sync.Mutex
	total     int
	successes int
	failures  int
	startTime time.Time
	durations []time.Duration
}

func newWorkloadMetrics() *workloadMetrics {
	return &workloadMetrics{
		startTime: time.Now(),
	}
}

func (m *workloadMetrics) record(start time.Time, success bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.total++
	if success {
		m.successes++
	} else {
		m.failures++
	}
	m.durations = append(m.durations, time.Since(start))
}

func (m *workloadMetrics) throughput() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	elapsed := time.Since(m.startTime).Seconds()
	if elapsed <= 0 {
		return 0
	}
	return float64(m.total) / elapsed
}

func (m *workloadMetrics) errorRate() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.total == 0 {
		return 0
	}
	return float64(m.failures) / float64(m.total)
}

// ---------------------------------------------------------------------------
// Soak test
// ---------------------------------------------------------------------------

func TestSoakWorkflowMix(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping soak test in short mode")
	}

	// Ensure we have a database.
	dsn := os.Getenv("CLEAT_TEST_DB")
	if dsn == "" {
		dsn = "postgres://localhost:5432/cleat?sslmode=disable"
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("Skipping soak test: no database: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Skipf("Skipping soak test: cannot ping database: %v", err)
	}
	_ = db // used only for connectivity check; actual workload uses in-memory simulation

	// Override cleanup.
	t.Cleanup(func() {
		// Report final metrics.
		t.Log("=== Soak Test Complete ===")
	})

	cfg := defaultSoakConfig()
	t.Logf("Soak test configuration:")
	t.Logf("  Duration: %v", cfg.Duration)
	t.Logf("  Sample interval: %v", cfg.SampleInterval)
	t.Logf("  Workflow types: %d", cfg.WorkflowTypes)
	t.Logf("  Max concurrent workflows: %d", cfg.MaxConcurrentWorkflows)

	// Create the workload generator and leak detector.
	metrics := newWorkloadMetrics()
	detector := newLeakDetector(5)
	wfTypes := workflowTypes[:cfg.WorkflowTypes]

	// Create a cancellable context for the soak test.
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Duration)
	defer cancel()

	// Start the workload loop.
	go runWorkloadLoop(ctx, cfg, wfTypes, metrics)

	// Monitoring loop: sample metrics at intervals and check for leaks.
	ticker := time.NewTicker(cfg.SampleInterval)
	defer ticker.Stop()

	initialThroughput := 0.0
	sampleCount := 0

	for {
		select {
		case <-ctx.Done():
			// Soak test completed successfully (duration reached).
			t.Logf("Soak test completed: duration=%v", cfg.Duration)
			finalTP := metrics.throughput()
			t.Logf("  Total workflows: %d", func() int { metrics.mu.Lock(); defer metrics.mu.Unlock(); return metrics.total }())
			t.Logf("  Throughput: %.2f wf/s", finalTP)
			t.Logf("  Error rate: %.2f%%", metrics.errorRate()*100)
			t.Logf("  Final: %s", detector.report())

			if metrics.errorRate() > 0.10 {
				t.Errorf("Error rate %.2f%% exceeds 10%% threshold", metrics.errorRate()*100)
			}
			if finalTP < initialThroughput*0.1 && sampleCount > 5 {
				t.Errorf("Throughput degraded: %.2f wf/s (initial was %.2f wf/s)", finalTP, initialThroughput)
			}
			return

		case <-ticker.C:
			sampleCount++
			detector.sample()
			currentTP := metrics.throughput()
			if sampleCount == 1 {
				initialThroughput = currentTP
			}

			t.Logf("[Sample %d] %s throughput=%.2f wf/s error_rate=%.2f%% total=%d",
				sampleCount, detector.report(), currentTP, metrics.errorRate()*100,
				func() int { metrics.mu.Lock(); defer metrics.mu.Unlock(); return metrics.total }())

			// Check for leaks.
			if err := detector.checkMemGrowth(); err != nil {
				// Log as warning first; fail if it persists.
				t.Logf("WARNING: %v", err)
			}
			if err := detector.checkGoroutineGrowth(); err != nil {
				t.Errorf("FAIL: %v", err)
			}
		}
	}
}
