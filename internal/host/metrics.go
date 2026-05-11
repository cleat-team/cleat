package host

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Engine-level Prometheus metrics. These are defined in the host package so the
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
	CompactionEventsDeletedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "cleat_compaction_events_deleted_total",
		Help: "Number of events deleted by history compaction",
	})
)

func init() {
	prometheus.MustRegister(durableCallsTotal, replayStepsTotal, freshStepsTotal, replayFailuresTotal, replayChecksumFailuresTotal, CompactionEventsDeletedTotal)
}
