package engine

import (
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

// Engine-level Prometheus metrics. These are defined in the engine package so the
// engine (engine.go) can increment them without a circular import. The worker
// package (cmd/cleat-worker) registers additional metrics in metrics.go.
var (
	durableCallsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "cleat_calls_total",
		Help: "DurableCall invocations (fresh, not replay)",
	})
	replayStepsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cleat_replay_steps_counter",
		Help: "Replay steps served from cached history",
	}, []string{"def_name"})
	freshStepsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "cleat_fresh_steps_total",
		Help: "Fresh steps executed (first-run or past recorded history)",
	})
	// Replay failures counter, incremented on replay divergence.
	replayFailuresTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "cleat_replay_failures_total",
		Help: "Number of replay divergence errors",
	})
	// Checksum failures counter for event history integrity verification.
	replayChecksumFailuresTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "cleat_replay_checksum_failures_total",
		Help: "Number of event history checksum verification failures on replay",
	})
	// Ambiguous calls counter, incremented when replay detects a pending sentinel
	// (a DurableCall whose external dispatch was recorded but whose outcome was
	// never persisted before a crash).
	ambiguousCallsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "cleat_ambiguous_calls_total",
		Help: "Total number of ambiguous call outcomes detected during replay (pending sentinel found)",
	})
	compactionEventsDeletedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "cleat_compaction_events_deleted_total",
		Help: "Number of events deleted by history compaction",
	})
	encryptionErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "cleat_encryption_errors_total",
		Help: "Total number of encryption failures during event flush (fail-secure aborts)",
	})
	decryptionErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "cleat_decryption_errors_total",
		Help: "Total number of decryption failures on the read path",
	})
	continueAsNewTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cleat_continue_as_new_total",
		Help: "Total ContinueAsNew triggers by reason",
	}, []string{"reason"})
	wasmFuelExhaustedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "cleat_wasm_fuel_exhausted_total",
		Help: "Total number of WASM fuel exhaustion events",
	})
	pluginCallDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cleat_plugin_call_duration_seconds",
		Help:    "Plugin call duration by plugin and function",
		Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30},
	}, []string{"plugin", "func"})
)

// Atomic counters for throughput computation. Incremented alongside the
// Prometheus step counters so the worker can sample them without prometheus.Collect.
var (
	replayStepCount int64
	freshStepCount  int64
)

// ReplayStepCount returns the total replay step count from the atomic counter.
func ReplayStepCount() int64 { return atomic.LoadInt64(&replayStepCount) }

// FreshStepCount returns the total fresh step count from the atomic counter.
func FreshStepCount() int64 { return atomic.LoadInt64(&freshStepCount) }

// AmbiguousCallsTotalCounter returns the ambiguous calls total counter for test access.
// The counter itself is unexported (ambiguousCallsTotal); this accessor allows tests in
// external packages (e.g., integrity) to read the metric value.
func AmbiguousCallsTotalCounter() prometheus.Counter { return ambiguousCallsTotal }

func init() {
	prometheus.MustRegister(durableCallsTotal, replayStepsTotal, freshStepsTotal, replayFailuresTotal, replayChecksumFailuresTotal, compactionEventsDeletedTotal, ambiguousCallsTotal, encryptionErrorsTotal, decryptionErrorsTotal, continueAsNewTotal, wasmFuelExhaustedTotal, pluginCallDuration)
}

// PluginCallDuration returns the plugin call duration histogram for use by the
// engine and tests.
func PluginCallDuration() *prometheus.HistogramVec { return pluginCallDuration }
