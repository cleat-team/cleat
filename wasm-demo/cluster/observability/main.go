// Built-in observability from the event history.
//
// Because every durable_call is intercepted by the host runtime and recorded
// in the event_history table, observability is FREE. The same data that
// enables replay-after-crash also enables:
//   - Structured audit logging (every external call, with inputs/outputs)
//   - Distributed tracing (workflow_id = trace_id, step = span sequence)
//   - Business metrics (success rates, durations, failure patterns)
//   - Replay-based debugging (re-execute locally against recorded history)
//
// The user writes ZERO observability code. It falls out of the durability
// mechanism.
//
// Build & run:
//   GOTOOLCHAIN=local /home/rcownie/go/bin/go build -o /tmp/observability ./cluster/observability.go
//   /tmp/observability

package main

import (
	"fmt"
	"strings"
	"time"
)

func main() {
	fmt.Println(strings.Repeat("=", 72))
	fmt.Println("  Built-in Observability from Durable Execution")
	fmt.Println(strings.Repeat("=", 72))

	theInsight()
	automaticLogging()
	tracing()
	metrics()
	businessQueries()
	replayDebugging()
	comparisonTable()
}

func theInsight() {
	fmt.Println()
	fmt.Println("── 1. THE INSIGHT ──")
	fmt.Println()
	fmt.Println("  In a durable execution system, every external interaction MUST")
	fmt.Println("  be recorded. Otherwise you can't replay after a crash.")
	fmt.Println()
	fmt.Println("  That recorded data IS observability data. You don't need to add")
	fmt.Println("  logging, tracing, or metrics instrumentation to workflow code.")
	fmt.Println("  The infrastructure captures everything.")
	fmt.Println()
	fmt.Println("  ┌─────────────────────────────────────────────────────────────┐")
	fmt.Println("  │                                                             │")
	fmt.Println("  │  Workflow (WASM)          Host Runtime                      │")
	fmt.Println("  │  ─────────────            ────────────                      │")
	fmt.Println("  │                                                             │")
	fmt.Println("  │  h.DurableCall(           ┌─────────────────────────────┐  │")
	fmt.Println(`  │    "payments",            │ 1. Record start time        │  │`)
	fmt.Println(`  │    "Charge",              │ 2. Make real HTTP call      │  │`)
	fmt.Println("  │    `{\"amount\":3299}`)     │ 3. Record end time          │  │")
	fmt.Println("  │                           │ 4. Record response/error    │  │")
	fmt.Println("  │                           │ 5. Append to event_history  │  │")
	fmt.Println("  │                           │ 6. Emit metric (duration)   │  │")
	fmt.Println("  │                           │ 7. Emit log line            │  │")
	fmt.Println("  │                           │ 8. Emit trace span          │  │")
	fmt.Println("  │                           └─────────────────────────────┘  │")
	fmt.Println("  │                                                             │")
	fmt.Println("  │  The workflow author writes h.DurableCall(...).             │")
	fmt.Println("  │  The infrastructure provides logging, tracing, and          │")
	fmt.Println("  │  metrics WITHOUT the user adding any instrumentation.       │")
	fmt.Println("  │                                                             │")
	fmt.Println("  └─────────────────────────────────────────────────────────────┘")
	fmt.Println()
}

func automaticLogging() {
	fmt.Println("── 2. STRUCTURED LOGGING (free) ──")
	fmt.Println()
	fmt.Println("  Every durable_call produces a structured log entry in the")
	fmt.Println("  event_history table:")
	fmt.Println()
	fmt.Println("  ┌────────────────────────────────────────────────────────────┐")
	fmt.Println("  │  {                                                         │")
	fmt.Println("  │    workflow_id:   \"order-alice-001\",                      │")
	fmt.Println("  │    step:           2,                                      │")
	fmt.Println("  │    timestamp:      \"2026-05-04T09:02:15.123Z\",            │")
	fmt.Println("  │    service:        \"payments\",                            │")
	fmt.Println("  │    operation:      \"Charge\",                              │")
	fmt.Println("  │    request:        {\"amount_cents\": 3299},                │")
	fmt.Println("  │    response:       {\"charge_id\": \"chg_xyz\", \"status\": \"ok\"},│")
	fmt.Println("  │    duration_ms:    87,                                     │")
	fmt.Println("  │    error:          null,                                   │")
	fmt.Println("  │    worker_id:      \"worker-01\",                            │")
	fmt.Println("  │    wasm_version:   1                                       │")
	fmt.Println("  │  }                                                         │")
	fmt.Println("  └────────────────────────────────────────────────────────────┘")
	fmt.Println()
	fmt.Println("  This is MORE detail than most hand-written log statements.")
	fmt.Println("  It tells you: who, what, when, how long, with what input,")
	fmt.Println("  what output, which version of the code, and on which worker.")
	fmt.Println()
	fmt.Println("  The user never calls log.Info(). It's captured automatically")
	fmt.Println("  by the host's DurableCall handler.")
	fmt.Println()
}

func tracing() {
	fmt.Println("── 3. DISTRIBUTED TRACING (free) ──")
	fmt.Println()
	fmt.Println("  Each workflow execution is a trace. Each durable_call is a span.")
	fmt.Println()
	fmt.Println("  ┌────────────────────────────────────────────────────────────┐")
	fmt.Println("  │                                                             │")
	fmt.Println("  │  Trace: order-alice-001 (PlaceOrder v1)                    │")
	fmt.Println("  │  Duration: 3.2 seconds                                      │")
	fmt.Println("  │                                                             │")
	fmt.Println("  │  ├─ span[0] catalog.LookupItem         87ms  ✅            │")
	fmt.Println("  │  ├─ span[1] catalog.LookupItem         92ms  ✅            │")
	fmt.Println("  │  ├─ span[2] inventory.Reserve         145ms  ✅            │")
	fmt.Println("  │  ├─ span[3] payments.GetDefaultMethod 110ms  ✅            │")
	fmt.Println("  │  ├─ span[4] payments.Charge           203ms  ✅            │")
	fmt.Println("  │  ├─ span[5] shipping.CreateShipment   180ms  ✅            │")
	fmt.Println("  │  └─ span[6] notifications.SendEmail    95ms  ✅            │")
	fmt.Println("  │                                                             │")
	fmt.Println("  └────────────────────────────────────────────────────────────┘")
	fmt.Println()
	fmt.Println("  If a workflow calls an external service that itself calls")
	fmt.Println("  another service, the trace context can be propagated through")
	fmt.Println("  the HTTP headers by the host runtime. The workflow code")
	fmt.Println("  doesn't need to do anything.")
	fmt.Println()
	fmt.Println("  The trace data doesn't need a separate store — it's in the")
	fmt.Println("  same event_history table. The trace is just:")
	fmt.Println("    SELECT step, service, operation, duration_ms, error")
	fmt.Println("    FROM event_history WHERE workflow_id = $1 ORDER BY step")
	fmt.Println()
}

func metrics() {
	fmt.Println("── 4. AUTOMATIC METRICS ──")
	fmt.Println()
	fmt.Println("  The host runtime can emit Prometheus metrics from data it")
	fmt.Println("  already has. No instrumentation in workflow code needed:")
	fmt.Println()

	// Simulate some metrics.
	now := time.Now()
	workflows := []struct {
		id       string
		version  int
		status   string
		duration time.Duration
		steps    int
		errors   int
	}{
		{"order-001", 1, "done", 3200 * time.Millisecond, 7, 0},
		{"order-002", 2, "done", 2800 * time.Millisecond, 8, 0},
		{"order-003", 2, "done", 4500 * time.Millisecond, 8, 0},
		{"order-004", 1, "done", 2100 * time.Millisecond, 7, 0},
		{"order-005", 3, "failed", 5100 * time.Millisecond, 4, 1},
		{"order-006", 3, "done", 3100 * time.Millisecond, 9, 0},
		{"order-007", 2, "done", 2900 * time.Millisecond, 8, 0},
		{"order-008", 1, "running", 0, 3, 0},
	}

	var totalDone, totalFailed, totalRunning int
	var totalDur time.Duration
	versionCounts := map[int]int{}
	perService := map[string]struct{ ok, fail int }{}

	for _, wf := range workflows {
		switch wf.status {
		case "done":
			totalDone++
			totalDur += wf.duration
		case "failed":
			totalFailed++
		case "running":
			totalRunning++
		}
		versionCounts[wf.version]++
	}

	_ = now

	fmt.Println("  ┌────────────────────────────────────────────────────────────┐")
	fmt.Println("  │  Metric                           Value                    │")
	fmt.Println("  ├────────────────────────────────────────────────────────────┤")
	fmt.Printf("  │  workflows_completed_total         %d                      │\n", totalDone)
	fmt.Printf("  │  workflows_failed_total            %d                      │\n", totalFailed)
	fmt.Printf("  │  workflows_running_current         %d                      │\n", totalRunning)
	fmt.Printf("  │  workflow_duration_avg_ms          %.0f                    │\n",
		float64(totalDur.Milliseconds())/float64(totalDone))
	fmt.Println("  │                                                            │")
	fmt.Println("  │  # Per version:                                            │")
	for v, count := range versionCounts {
		fmt.Printf("  │  workflows_by_version{v=\"%d\"}      %d                      │\n", v, count)
	}
	fmt.Println("  │                                                            │")
	fmt.Println("  │  # Per service.operation:                                  │")
	for svc, counts := range perService {
		fmt.Printf("  │  calls_total{service=\"%s\"}   ok=%d fail=%d             │\n",
			svc, counts.ok, counts.fail)
	}
	fmt.Println("  │                                                            │")
	fmt.Println("  │  # Host-level metrics (no user code):                      │")
	fmt.Println("  │  replay_from_cache_total            1342                   │")
	fmt.Println("  │  checkpoint_save_duration_avg_ms    4.2                    │")
	fmt.Println("  │  wasm_load_duration_avg_ms          12.7                   │")
	fmt.Println("  │  wasm_cache_hit_ratio               0.94                   │")
	fmt.Println("  └────────────────────────────────────────────────────────────┘")
	fmt.Println()
	fmt.Println("  These metrics are emitted by the host runtime, not by")
	fmt.Println("  workflow code. The workflow author gets monitoring for free.")
	fmt.Println()
}

func businessQueries() {
	fmt.Println("── 5. BUSINESS-LEVEL QUERIES ──")
	fmt.Println()
	fmt.Println("  Because the event history is structured data, you can ask")
	fmt.Println("  business questions directly against it:")
	fmt.Println()
	fmt.Println("  -- How many orders had payment failures after inventory")
	fmt.Println("  -- reservation? (This is a common compensation scenario)")
	fmt.Println("  SELECT COUNT(DISTINCT workflow_id)")
	fmt.Println("  FROM event_history")
	fmt.Println("  WHERE service = 'payments' AND operation = 'Charge'")
	fmt.Println("    AND error IS NOT NULL")
	fmt.Println("    AND workflow_id IN (")
	fmt.Println("      SELECT workflow_id FROM event_history")
	fmt.Println("      WHERE service = 'inventory' AND operation = 'Reserve'")
	fmt.Println("    );")
	fmt.Println()
	fmt.Println("  -- What's the P50/P95/P99 latency for each external service?")
	fmt.Println("  SELECT service, operation,")
	fmt.Println("    percentile_cont(0.50) WITHIN GROUP (ORDER BY duration_ms) as p50,")
	fmt.Println("    percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms) as p95,")
	fmt.Println("    percentile_cont(0.99) WITHIN GROUP (ORDER BY duration_ms) as p99")
	fmt.Println("  FROM event_history")
	fmt.Println("  WHERE recorded_at > now() - INTERVAL '1 hour'")
	fmt.Println("  GROUP BY service, operation;")
	fmt.Println()
	fmt.Println("  -- Which workflows are stuck? (no new steps in 10 minutes)")
	fmt.Println("  SELECT wi.id, wi.def_name, wi.def_version,")
	fmt.Println("    (SELECT MAX(step) FROM event_history eh")
	fmt.Println("     WHERE eh.workflow_id = wi.id) as last_step,")
	fmt.Println("    now() - wi.heartbeat_at as time_since_heartbeat")
	fmt.Println("  FROM workflow_instances wi")
	fmt.Println("  WHERE wi.status = 'running'")
	fmt.Println("    AND wi.heartbeat_at < now() - INTERVAL '10 minutes';")
	fmt.Println()
	fmt.Println("  -- What's the version adoption curve?")
	fmt.Println("  SELECT def_version,")
	fmt.Println("    COUNT(*) FILTER (WHERE status = 'done') as completed,")
	fmt.Println("    COUNT(*) FILTER (WHERE status = 'running') as running,")
	fmt.Println("    COUNT(*) FILTER (WHERE status = 'failed') as failed")
	fmt.Println("  FROM workflow_instances")
	fmt.Println("  WHERE def_name = 'PlaceOrder'")
	fmt.Println("    AND created_at > now() - INTERVAL '7 days'")
	fmt.Println("  GROUP BY def_version;")
	fmt.Println()
	fmt.Println("  These are NOT application-level metrics queries. They're")
	fmt.Println("  standard SQL over the durability tables. Every workflow")
	fmt.Println("  system needs these tables to function. The observability is")
	fmt.Println("  a byproduct of the durability mechanism.")
	fmt.Println()
}

func replayDebugging() {
	fmt.Println("── 6. REPLAY-BASED DEBUGGING ──")
	fmt.Println()
	fmt.Println("  When a workflow fails, you can replay it locally — the same")
	fmt.Println("  WASM bytes, the same event history, the same inputs. The only")
	fmt.Println("  difference: instead of making real API calls, durable_call")
	fmt.Println("  returns the recorded responses from the event history.")
	fmt.Println()
	fmt.Println("  ┌────────────────────────────────────────────────────────────┐")
	fmt.Println("  │                                                            │")
	fmt.Println("  │  $ durable-cli debug order-005                            │")
	fmt.Println("  │                                                            │")
	fmt.Println("  │  Loading WASM: PlaceOrder v3 (55 KB)                      │")
	fmt.Println("  │  Loading event history: 4 steps recorded                  │")
	fmt.Println("  │                                                            │")
	fmt.Println("  │  Replaying in debug mode...                                │")
	fmt.Println("  │                                                            │")
	fmt.Println("  │  step 0: catalog.LookupItem → cached {found: true}        │")
	fmt.Println("  │  step 1: inventory.Reserve → cached {reservation_id: ...} │")
	fmt.Println("  │  step 2: payments.GetDefaultMethod → cached {token: ...}  │")
	fmt.Println(`  │  step 3: payments.Charge → ERROR: "insufficient funds"    │`)
	fmt.Println("  │           ^                                                │")
	fmt.Println("  │           |___ Breakpoint here. You can inspect:          │")
	fmt.Println("  │                • All local variables                       │")
	fmt.Println("  │                • The compensation path about to execute    │")
	fmt.Println("  │                • The exact request that failed             │")
	fmt.Println("  │                                                            │")
	fmt.Println("  │  This is a TIME-TRAVEL DEBUGGER for business processes.   │")
	fmt.Println(`  │  No log grep, no reproduction steps, no "can you send me  │`)
	fmt.Println(`  │  the request ID?" — everything is in the event history.    │`)
	fmt.Println("  │                                                            │")
	fmt.Println("  └────────────────────────────────────────────────────────────┘")
	fmt.Println()
}

func comparisonTable() {
	fmt.Println("── 7. WHAT THE USER WRITES vs WHAT THEY GET ──")
	fmt.Println()
	fmt.Println("  ┌────────────────────────────────────────────────────────────┐")
	fmt.Println("  │  User writes:                          User gets for free: │")
	fmt.Println("  ├────────────────────────────────────────────────────────────┤")
	fmt.Println("  │                                            • Audit log of  │")
	fmt.Println("  │  func PlaceOrder(h *HostCalls,              every external  │")
	fmt.Println("  │      userID string,                         call with full  │")
	fmt.Println("  │      cart []CartItem) (string, error) {     request/response│")
	fmt.Println("  │                                            • Distributed    │")
	fmt.Println("  │    r, err := validateAndReserve(             trace with per- │")
	fmt.Println("  │        h, userID, cart)                     step latency    │")
	fmt.Println("  │    if err != nil {                         + Prometheus      │")
	fmt.Println(`  │        return "", err                       metrics (success │`)
	fmt.Println("  │    }                                        rate, duration,  │")
	fmt.Println("  │                                            • Business-level  │")
	fmt.Println("  │    c, err := processPayment(                dashboards via   │")
	fmt.Println("  │        h, userID, r.TotalCents)            SQL queries      │")
	fmt.Println("  │    if err != nil {                         • Time-travel      │")
	fmt.Println("  │        releaseReservation(                  debugger for     │")
	fmt.Println("  │            h, r.ReservationID)             every failure     │")
	fmt.Println(`  │        return "", err                      + Version adoption │`)
	fmt.Println("  │    }                                        tracking         │")
	fmt.Println("  │                                            • Stuck-workflow  │")
	fmt.Println("  │    return fulfillOrder(                     detection         │")
	fmt.Println("  │        h, r, c)                            • Compensation     │")
	fmt.Println("  │  }                                          path analysis    │")
	fmt.Println("  └────────────────────────────────────────────────────────────┘")
	fmt.Println()
	fmt.Println("  The user's code has ZERO observability concerns. No log")
	fmt.Println("  statements. No span contexts. No metric counters. No debug")
	fmt.Println("  endpoints. It's pure business logic.")
	fmt.Println()
	fmt.Println("  This is possible because the host runtime stands BETWEEN the")
	fmt.Println("  workflow and the outside world. It sees every interaction.")
	fmt.Println("  It records everything — for durability. And that record IS")
	fmt.Println("  observability.")
	fmt.Println()
}
