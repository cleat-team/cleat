# Formal Verification Opportunities in Cleat

May 2026. An assessment of which core components would benefit from formal
verification (TLA+, Alloy, or similar) and what the models would look like.

---

## 1. Why Formal Verification for Cleat

Durable execution engines are notoriously hard to test empirically. The bugs
that matter — race conditions, lost signals, double execution — manifest
rarely and are nearly impossible to reproduce. The cost of these bugs in
production is catastrophic: workflows that silently skip steps, run twice,
or deadlock forever.

Cleat's core engine is ~4,700 lines of Go implementing a concurrent,
distributed protocol. The components that need verification are not the
business logic (which differs per workflow) but the **infrastructure
protocols** that every workflow depends on:

1. Workflow state machine (correctness of status transitions)
2. SKIP LOCKED claim protocol (at-most-once execution across workers)
3. Signal delivery protocol (no lost signals, correct suspend/resume)
4. Concurrency key protocol (mutual exclusion with expiry)
5. History compaction (replay equivalence)

All five are finite-state or algorithmically tractable — well within the
sweet spot for TLA+.

---

## 2. Component 1: Workflow Instance State Machine

**Files:** `internal/host/engine.go` (1,529 lines), `internal/host/db.go` (1,584 lines), `schema.sql`

**States observed in code and schema:**

```
ready → running → completed
                → failed
                → timed_out
                → cancelled
       → running → suspended → ready (wake-up)
                 → suspended → timed_out (timeout during suspend)
       → timed_out (scheduled workflow never claimed)
```

**Key transitions:**

| From | Trigger | To | Atomic? |
|------|---------|----|---------|
| `ready` | Worker claims via SKIP LOCKED | `running` | Yes (DB transaction) |
| `running` | Workflow calls DurableSleep/AwaitSignals | `suspended` | Yes (in Execute) |
| `suspended` | next_wake_at reached | `ready` | Yes (ClaimWorkflow WHERE filter) |
| `running` | Execute completes normally | `completed` | Yes (in Execute) |
| `running` | Execute returns error | `failed` | Yes (in Execute) |
| `running` | Heartbeat timeout detected by reaper | `timed_out` | Yes (ReapStaleInstances) |
| `running` | Cancel requested | `cancelled` | No (two operations) |
| `suspended` | Timeout during suspend | `timed_out` | Yes (in ReapStaleInstances) |

**What TLA+ would verify:**

```
Safety properties:
- A workflow is never in 'running' on two workers simultaneously
  (assigned_to is unique per non-terminated instance)
- A workflow never transitions from a terminal state (completed, failed,
  timed_out, cancelled)
- Heartbeat reaping only targets instances where heartbeat_at < now() - threshold

Liveness properties:
- A 'ready' workflow is eventually claimed by some worker
- A 'suspended' workflow wakes up when next_wake_at is reached
- A 'running' workflow with a stale heartbeat is eventually reaped
```

**Value:** HIGH. This is the foundation. A bug here corrupts every workflow.
The state machine is small enough (6 states, ~10 transitions) for a complete
TLA+ model in under 200 lines.

**Effort:** ~2 days for a comprehensive TLA+ specification.

---

## 3. Component 2: SKIP LOCKED Claim Protocol

**Files:** `internal/host/db.go` lines 277-370, `cmd/cleat-worker/main.go`

**The algorithm:**

```
ClaimWorkflows(workerID, namespace, limit):
  BEGIN
    UPDATE workflow_instances
    SET status = 'running',
        assigned_to = workerID,
        claimed_at = now(),
        heartbeat_at = now() + heartbeat_interval
    WHERE id IN (
      SELECT id FROM workflow_instances
      WHERE status = 'ready'
        AND namespace = $namespace
        AND (task_queue IS NULL OR task_queue = ANY($task_queues))
        AND next_wake_at <= now()
      ORDER BY priority DESC, created_at ASC
      LIMIT $limit
      FOR UPDATE SKIP LOCKED
    )
    RETURNING *
  COMMIT
```

**The protocol:**
1. Multiple workers concurrently poll this query
2. PostgreSQL's `SKIP LOCKED` ensures each row is claimed by at most one worker
3. Worker executes the workflow, heartbeating periodically
4. On completion/failure: worker updates status and releases claim
5. If worker crashes: ReapStaleInstances detects stale heartbeat and resets
   status to 'ready'

**Critical race conditions (from the implementation):**

**Race A: Double-claim during heartbeat gap.** Worker A claims instance, worker
A crashes, heartbeat expires, worker B claims same instance, worker A recovers.
If worker A didn't actually die, two workers execute the same workflow.

Mitigation in code: heartbeat check in `ReapStaleInstances` uses a timeout
threshold (default 30s). Worker A must be dead for 30s before worker B claims.
If worker A recovers within 30s, it restarts heartbeating and B never claims.
But this is a *probabilistic* guarantee, not a proof.

**Race B: Claim-then-crash before updating status.** Worker claims, process
crashes before executing. Instance is `running` with stale heartbeat.
ReapStaleInstances resets to `ready`. But if the worker partially executed
before crashing (WASM side effects?), replay must be deterministic.

Mitigation: WASM execution is deterministic given the event history. If the
worker wrote zero events before crashing, replay is a no-op. If it wrote
partial events, replay continues from the last event.

**What TLA+ would verify:**

```
Safety:
- At most one worker executes a given instance at any time
- An instance claimed by worker W is never claimed by worker W' while
  W's heartbeat is live
- ReapStaleInstances only resets instances with genuinely stale heartbeats

Liveness:
- If a worker crashes, its claimed instances are eventually reclaimed
- If instances are ready and workers are available, some worker eventually
  claims them
- The system makes progress (no infinite claim-abandon loops)
```

**The TLA+ model structure:**

```tla
CONSTANTS Workers, Instances, HeartbeatTimeout
VARIABLES instance_status, assigned_to, heartbeat_at

Claim(w, i) ==
  /\ instance_status[i] = "ready"
  /\ heartbeat_at[i] < now
  /\ instance_status' = [instance_status EXCEPT ![i] = "running"]
  /\ assigned_to' = [assigned_to EXCEPT ![i] = w]
  /\ heartbeat_at' = [heartbeat_at EXCEPT ![i] = now + HeartbeatInterval]

ReapStale(w, i) ==
  /\ instance_status[i] = "running"
  /\ heartbeat_at[i] + HeartbeatTimeout < now
  /\ instance_status' = [instance_status EXCEPT ![i] = "ready"]
  /\ assigned_to' = [assigned_to EXCEPT ![i] = None]
```

Then model-check properties like `[] (running_instances_count <= 1)` per instance.

**Value:** VERY HIGH. This is the hardest component to test empirically —
concurrent claim races are rare but catastrophic (double execution). TLA+ can
exhaustively check all possible interleavings of N workers claiming from M
instances.

**Effort:** ~3 days for the model + model checking with varying worker/instance
counts.

---

## 4. Component 3: Signal Delivery Protocol

**Files:** `internal/host/engine.go` lines 824-895, `internal/host/db.go` (signal store)

**The algorithm:**

```
Signal delivery (external → workflow):
  POST /api/workflows/:id/signals/:name
    → SignalWorkflow(workflowID, signalName, payload)
    → INSERT INTO workflow_signals (workflow_id, name, payload)
    → If workflow is suspended waiting for this signal:
        → Wake the workflow (update next_wake_at to now)

Signal consumption (inside workflow):
  PollSignal(signalName):
    → SELECT FROM workflow_signals WHERE workflow_id = ? AND name = ?
    → If exists: return payload, DELETE row
    → If not exists: return "not found"

  AwaitSignals([signalNames], timeout):
    → For each name: call PollSignal
    → If any found: return immediately
    → If none found: record AwaitSignals event in history, SuspendError
    → On wake-up (signal delivered or timeout): re-check signals, return result
```

**The known race condition (from DX_COMPARISON.md issue #5):**
```
Time →
Worker:    PollSignal("approve") → not found
External:                          POST /signals/approve → INSERT signal
Worker:    AwaitSignals(["approve"]) → Suspend (signal already delivered but not seen)
Result:    Workflow suspended even though signal was delivered before AwaitSignals.
           Signal consumed on next wake-up (could be seconds/minutes later).
```

The fix described in the code (engine.go lines ~858-877): "AwaitSignals checks
the signal store first before suspending." But this is a TOCTOU race — the
signal could arrive between the last PollSignal check and the INSERT of the
event record that triggers suspension.

**What TLA+ would verify:**

```
Safety:
- Every signal delivered during or before an AwaitSignals call is observed
  by that AwaitSignals call or a subsequent PollSignal
- No signal is silently dropped (every INSERT into workflow_signals is
  eventually consumed or the workflow terminates)

Liveness:
- If a signal is delivered while a workflow is suspended in AwaitSignals,
  the workflow eventually wakes up and processes it
- If a signal is delivered before AwaitSignals begins, AwaitSignals returns
  immediately without suspending
```

**The tricky part:** Modeling the interleaving of the external signal delivery
(HTTP handler) and the WASM execution (serial within a workflow, but the
signal handler runs in a different goroutine or even a different process).

**Value:** HIGH. Signal ordering bugs are the hardest to reproduce in testing
because they depend on precise timing. TLA+ would exhaustively explore all
orderings.

**Effort:** ~2 days for the model. The signal delivery path is ~70 lines of
engine code — small enough for a tight TLA+ spec.

---

## 5. Component 4: Concurrency Key Protocol

**Files:** `internal/host/db.go` lines 1220-1243, `internal/host/concurrency_test.go` (361 lines)

**The algorithm:**

```
AcquireConcurrencyKey(key, workflowID, ttl):
  DELETE FROM concurrency_keys WHERE key_hash = digest(key) AND expires_at < now()
  INSERT INTO concurrency_keys (key_hash, key_text, workflow_id, expires_at)
    VALUES (digest(key), key, workflowID, now() + ttl)
    ON CONFLICT (key_hash) DO NOTHING
  RETURNING (rows affected > 0)

ReleaseConcurrencyKey(key):
  DELETE FROM concurrency_keys WHERE key_hash = digest(key)

ReapExpiredConcurrencyKeys():
  DELETE FROM concurrency_keys WHERE expires_at < now()
```

**Properties to verify:**

```
Safety:
- At most one workflow holds a given key at any time
  (the ON CONFLICT DO NOTHING ensures this at the DB level)
- A workflow that fails to acquire a key gets a clean false, not an error
- Releasing a key makes it immediately available for acquisition

Liveness:
- An expired key is eventually reaped (reaper runs periodically)
- A key acquired by a completed workflow is eventually released
  (ReleaseWorkflowConcurrencyKeys called on complete/fail)
```

**The interesting verification question:** The protocol uses `DELETE expired`
+ `INSERT ... ON CONFLICT` as a two-step acquire. Is there a window where two
workers could both acquire the same key? The answer should be No because
`ON CONFLICT (key_hash) DO NOTHING` is atomic — but the `DELETE expired` step
runs in the same transaction. If two workers both see the key as expired and
both DELETE, then both INSERT, only one INSERT succeeds. The other gets
rows_affected = 0. This is correct but deserves model-checking.

**Value:** MEDIUM. The algorithm is simpler than the claim protocol and the
database-level constraints provide stronger guarantees. But concurrency key
bugs would cause silent data corruption (two workflows processing the same
order simultaneously).

**Effort:** ~1 day. Simple model — two operations (acquire, release), one
reaper, 3-4 workers.

---

## 6. Component 5: History Compaction Correctness

**Files:** `internal/host/compaction.go` (437 lines)

**The algorithm:**

```
CompactWorkflowHistory(workflowID, threshold):
  IF event_count < threshold: skip
  Load all events for workflow
  Extract CompactionState:
    - Completed steps (completed_events counter)
    - Active deferrals (deferred cleanup functions)
    - Active children (child workflow runIDs)
    - Last query_state, last version, namespace
    - ContinueAsNew counter
  Build compacted history:
    - Virtual event at position 0: compaction state marker
    - Tail events: the last N events that couldn't be compacted
  Save compacted events back to event_history
  Update workflow_instances.compaction_state = JSONB(CompactionState)
```

**The correctness property:**

> Replaying a workflow from the compacted history (virtual events + tail)
> produces the same result as replaying from the full original history.

This is a **refinement mapping**. The compacted history is a coarser-grained
representation that must produce the same final state.

**What TLA+ would verify:**

```
For all histories H, compaction state CS = extractCompactionState(H),
    tail = last N events of H:
  buildFullHistoryFromCompaction(tail, CS) is replay-equivalent to H

Where "replay-equivalent" means:
  - Same final result (workflow output)
  - Same query_state after replay
  - Same active deferrals after replay
  - Same active children after replay
```

**Value:** HIGH for long-running workflows. Compaction bugs would manifest as
workflows producing different results after compaction — a nightmare to debug
because it only triggers after the threshold (default 1000 events), which
might take weeks of production running.

**Approach:** This is more suited to property-based testing than TLA+, because
it's a functional correctness property (does compaction preserve semantics?)
rather than a concurrency property (can two things happen at the same time?).
A Go fuzzing approach — generate random event histories, compact them, replay
both, assert equivalence — would be more practical and equally rigorous.

**Effort:** ~3 days for property-based testing with Go fuzzing. TLA+ could
model the compaction algorithm but would require encoding the event history
semantics, which is better done in Go.

---

## 7. Summary and Recommended Order

| Component | Risk | TLA+ fit | Effort | Priority |
|-----------|------|----------|--------|----------|
| SKIP LOCKED claim protocol | Double execution | Excellent | ~3 days | **P0** |
| Workflow state machine | Invalid transitions | Excellent | ~2 days | **P0** |
| Signal delivery protocol | Lost signals | Excellent | ~2 days | **P1** |
| History compaction | Wrong replay | Better for fuzzing | ~3 days | **P1** |
| Concurrency key protocol | Double processing | Good | ~1 day | **P2** |

**Why P0 on claim protocol and state machine:** These two form the foundation.
Every other component assumes they're correct. A TLA+ model of these two
together (the worker claim protocol operating on the state machine) would
cover the most critical correctness properties in the entire system. Combined
model: ~4 days.

**Why not TLA+ for everything:** Compaction correctness is better served by
property-based testing (Go fuzzing) because it involves complex data
transformations that are natural to express in Go but awkward in TLA+.
Start with TLA+ for the concurrent protocols, then fuzz the data
transformations.

---

## 8. What the TLA+ Models Would Look Like

### State Machine + Claim Protocol (combined model)

```tla
---- MODULE CleatCore ----
EXTENDS Naturals, TLC, Sequences

CONSTANTS
  Workers,          \* Set of worker IDs
  Instances,        \* Set of workflow instance IDs
  HeartbeatInterval,\* How often workers heartbeat (e.g., 10s)
  HeartbeatTimeout, \* When a heartbeat is considered stale (e.g., 30s)
  MaxSteps          \* Max workflow steps before completion

VARIABLES
  status,           \* [Instances -> {"ready","running","suspended","completed","failed","timed_out"}]
  assigned_to,      \* [Instances -> Workers UNION {None}]
  heartbeat_at,     \* [Instances -> Int]  (logical clock)
  next_wake_at,     \* [Instances -> Int UNION {Infinity}]
  step_count,       \* [Instances -> Int]
  clock             \* Int (global logical clock)

vars == <<status, assigned_to, heartbeat_at, next_wake_at, step_count, clock>>

\* Initial state
Init ==
  /\ status = [i \in Instances |-> "ready"]
  /\ assigned_to = [i \in Instances |-> None]
  /\ heartbeat_at = [i \in Instances |-> 0]
  /\ next_wake_at = [i \in Instances |-> 0]
  /\ step_count = [i \in Instances |-> 0]
  /\ clock = 0

\* Worker claims a ready instance
Claim(w, i) ==
  /\ status[i] = "ready"
  /\ next_wake_at[i] <= clock
  /\ status' = [status EXCEPT ![i] = "running"]
  /\ assigned_to' = [assigned_to EXCEPT ![i] = w]
  /\ heartbeat_at' = [heartbeat_at EXCEPT ![i] = clock + HeartbeatInterval]
  /\ UNCHANGED <<next_wake_at, step_count>>
  /\ clock' = clock + 1

\* Worker executes one step
ExecuteStep(w, i) ==
  /\ status[i] = "running"
  /\ assigned_to[i] = w
  /\ step_count[i] < MaxSteps
  /\ heartbeat_at' = [heartbeat_at EXCEPT ![i] = clock + HeartbeatInterval]
  /\ step_count' = [step_count EXCEPT ![i] = step_count[i] + 1]
  /\ IF step_count[i] + 1 >= MaxSteps
     THEN /\ status' = [status EXCEPT ![i] = "completed"]
          /\ assigned_to' = [assigned_to EXCEPT ![i] = None]
     ELSE /\ \E sleep \in BOOLEAN:
              IF sleep
              THEN /\ status' = [status EXCEPT ![i] = "suspended"]
                   /\ next_wake_at' = [next_wake_at EXCEPT ![i] = clock + 5]
              ELSE UNCHANGED <<status, next_wake_at>>
  /\ UNCHANGED <<(except as above)>>
  /\ clock' = clock + 1

\* Worker heartbeats (keeps claim alive)
Heartbeat(w, i) ==
  /\ status[i] = "running"
  /\ assigned_to[i] = w
  /\ heartbeat_at' = [heartbeat_at EXCEPT ![i] = clock + HeartbeatInterval]
  /\ UNCHANGED <<status, assigned_to, next_wake_at, step_count>>
  /\ clock' = clock + 1

\* Worker crashes or heartbeats stop; reaper resets
ReapStale(i) ==
  /\ status[i] = "running"
  /\ heartbeat_at[i] + HeartbeatTimeout < clock
  /\ status' = [status EXCEPT ![i] = "ready"]
  /\ assigned_to' = [assigned_to EXCEPT ![i] = None]
  /\ UNCHANGED <<heartbeat_at, next_wake_at, step_count>>
  /\ clock' = clock + 1

\* Suspended workflow wakes up
WakeUp(i) ==
  /\ status[i] = "suspended"
  /\ next_wake_at[i] <= clock
  /\ status' = [status EXCEPT ![i] = "ready"]
  /\ UNCHANGED <<assigned_to, heartbeat_at, next_wake_at, step_count>>
  /\ clock' = clock + 1

\* Safety: no double execution
DoubleExecutionSafety ==
  \A i \in Instances:
    \A w1, w2 \in Workers:
      w1 /= w2 =>
        ~(/\ status[i] = "running"
          /\ assigned_to[i] = w1
          /\ assigned_to[i] = w2)  \* impossible by definition, but checks invariant

\* Safety: terminal states stay terminal
TerminalStable ==
  \A i \in Instances:
    status[i] \in {"completed", "failed", "timed_out"} =>
      [] (status[i] = status[i]')  \* never changes

\* Liveness: ready instances are eventually claimed
Liveness ==
  \A i \in Instances:
    status[i] = "ready" ~> \E w \in Workers: assigned_to[i] = w
```

**Model-checking parameters:**
- 3 workers, 5 instances, 50 steps — takes seconds in TLC
- 5 workers, 10 instances, 100 steps — takes minutes
- Can check all interleavings because the state space is bounded by
  `(status_states ^ instances) * (workers ^ running_instances)`

### Signal Delivery Protocol (separate model)

```tla
---- MODULE CleatSignals ----

CONSTANTS Signals, Timeout
VARIABLES
  signal_store,     \* Set of delivered signals
  workflow_status,  \* {"executing", "suspended_awaiting", "suspended_sleep"}
  awaiting_signals, \* Set of signal names workflow is waiting for
  clock

\* External signal delivery
DeliverSignal(sig) ==
  /\ signal_store' = signal_store \union {sig}
  /\ IF workflow_status = "suspended_awaiting" /\ sig \in awaiting_signals
     THEN /\ workflow_status' = "executing"  \* wake up
     ELSE UNCHANGED workflow_status
  /\ clock' = clock + 1

\* Workflow checks for signals
PollSignal(sig) ==
  /\ workflow_status = "executing"
  /\ IF sig \in signal_store
     THEN /\ signal_store' = signal_store \ {sig}
          /\ \* signal found, continue executing
     ELSE UNCHANGED signal_store
  /\ clock' = clock + 1

\* Workflow suspends waiting for signals
AwaitSignals(sigs, timeout) ==
  /\ workflow_status = "executing"
  /\ \A sig \in sigs: sig \notin signal_store  \* none pending
  /\ workflow_status' = "suspended_awaiting"
  /\ awaiting_signals' = sigs
  /\ clock' = clock + timeout  \* will wake after timeout if no signal

\* Safety: no signal is lost
NoLostSignal ==
  \A sig \in Signals:
    \/ sig \notin signal_store      \* already consumed
    \/ workflow_status = "executing" \* being processed
    \/ sig \in awaiting_signals     \* workflow is waiting for it
    \/ <> (sig \notin signal_store)  \* eventually consumed
```

The key insight: by modeling the interleaving of `DeliverSignal` and
`PollSignal`/`AwaitSignals`, TLC can find the TOCTOU race condition —
the exact sequence of operations where a signal is lost.

---

## 9. Recommendations

### What to do now (next sprint)

1. **Model the claim protocol + state machine in TLA+.** ~4 days. This is the
   highest-value, highest-risk component. The model would be ~200 lines of TLA+
   and would exhaustively verify at-most-once execution for all possible
   worker/instance count combinations up to some bound.

2. **Model the signal delivery protocol in TLA+.** ~2 days. Smaller model, but
   addresses the known race condition from the DX comparison.

### What to do next

3. **Property-based testing for compaction correctness.** ~3 days. Generate
   random event histories, compact, replay both, assert equivalence. Use Go's
   native fuzzing (`testing.F`). This is more practical than TLA+ for data
   transformation correctness.

4. **Concurrency key model.** ~1 day. Simple model, lower priority because
   database constraints provide stronger guarantees.

### What NOT to do

- Don't model the WASM execution semantics (determinism of Go compiled to WASM).
  This is a compiler correctness problem, not a protocol problem.
- Don't model the entire system in one TLA+ spec. Model each protocol separately
  with clearly defined interfaces between them.
- Don't model performance properties (throughput, latency). TLA+ is for
  correctness, not performance. Use load testing for that.

### Total investment

~7 days for the two P0 models (claim protocol + signal delivery). The ROI is
that you find concurrency bugs *before* they hit production — where the cost
of a double-execution or lost-signal bug is measured in corrupted business
data, not in debugging hours.

---

## Appendix: What Other Infrastructure Projects Do

| Project | Formal methods used | Result |
|---------|-------------------|--------|
| **Amazon S3** | TLA+ for replication protocol | Found 2 bugs in the design before implementation |
| **MongoDB** | TLA+ for replication consensus | Found edge case in leader election |
| **CockroachDB** | TLA+ for parallel commits | Verified serializable isolation |
| **Temporal** | None publicly documented | Relies on extensive integration testing |
| **FoundationDB** | Flow (deterministic simulation) | Used instead of TLA+; similar guarantees |
| **CompCert** | Coq (full compiler verification) | C compiler proven correct |

Temporal's approach is noteworthy: they rely on a deterministic simulation
framework (similar to FoundationDB's Flow) rather than TLA+. For cleat,
TLA+ is a better fit because the protocols are smaller and the team is smaller —
you can model the critical ~200 lines of the claim protocol in TLA+ faster
than you can build a deterministic simulation framework.
