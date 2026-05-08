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
	replayStepsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "cleat_replay_steps_total",
		Help: "Replay steps served from cached history",
	})
	freshStepsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "cleat_fresh_steps_total",
		Help: "Fresh steps executed (first-run or past recorded history)",
	})
)

func init() {
	prometheus.MustRegister(durableCallsTotal, replayStepsTotal, freshStepsTotal)
}
