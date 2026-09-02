package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/tetratelabs/wazero/api"
)

// writeResult writes a string to WASM linear memory. In the normal (wazero)
// path, it writes through the api.Memory obtained from m. In the wasmtime
// path, it uses a raw byte buffer stored in the context via
// contextWithRawMemBuf, in which case m can be nil.
func (s *execSession) writeResult(ctx context.Context, m api.Module, ptr uint32, val string, maxLen uint32) (uint32, error) {
	if rawBuf, ok := ctx.Value(wasmMemBufKey{}).([]byte); ok && rawBuf != nil {
		data := []byte(val)
		if uint32(len(data)) > maxLen {
			data = data[:maxLen]
		}
		// ptr comes from the guest. Unchecked, `rawBuf[ptr:]` panics for any
		// ptr past the end of linear memory -- reachable from guest code by
		// calling e.g. cleat_workflow_id with a bad output pointer, and
		// demonstrated by TestWriteResult_GuestPointerOutOfRange.
		//
		// It did not crash the worker: the recover around fn.Call in
		// backend_wasmtime.go catches it. But it surfaced as `wasmtime panic
		// in "run": runtime error: slice bounds out of range`, which reads as
		// an engine defect rather than a guest passing a bad pointer, and it
		// leaned on a recover to handle ordinary malformed guest input. The
		// wazero path returns a clean error for the same input (mem.Write
		// reports out-of-bounds), and wasmtimeWriteString in wasmtime_memory.go
		// already does exactly this check -- writeResult was the one raw-buffer
		// writer that skipped it.
		//
		// uint64 arithmetic so ptr+len cannot itself wrap.
		if uint64(ptr)+uint64(len(data)) > uint64(len(rawBuf)) {
			return 0, fmt.Errorf("writeResult: write %d bytes at ptr %d exceeds the %d-byte guest memory",
				len(data), ptr, len(rawBuf))
		}
		n := copy(rawBuf[ptr:], data)
		return uint32(n), nil
	}
	if m != nil {
		if mem := m.Memory(); mem != nil {
			return writeWasmString(mem, ptr, val, maxLen)
		}
	}
	return 0, nil
}

// insertEventSQL is the shared INSERT statement for both fast and quota paths.
//
// # Fencing is folded into this statement, not a separate round trip (B4)
//
// An earlier version of this fix called WorkflowStore.Heartbeat as a
// separate statement before this INSERT: renew the lease, then write if it
// held. That was correct -- see engine/db.go's Heartbeat doc for why the
// renew-then-write gap is safe against the reaper -- but it cost a second
// round trip on every single event, on the specific path
// docs/architecture identifies as the throughput bottleneck (~312-1,562
// claims/sec, per-step DB work as the binding constraint). It was also an
// argument for safety rather than a guarantee of it: two statements are
// never atomic with each other, even when the argument for why that is fine
// holds up.
//
// Measured, not assumed: a counting driver.Conn wrapper around the real
// Postgres test database, intercepting BeginTx/Exec/Query/Commit (each one
// real network round trip), put the tenanted flushEvent path at 4 round
// trips per event with this single-statement fence -- identical to 4 with
// fencing supplied but not required (same SQL, so this is the expected
// result, not a second data point) -- versus 8 for the discarded
// Heartbeat-then-write version reconstructed against the same database:
// Heartbeat's own beginTxWithRLS/UPDATE/Commit (4, RLS setup included) plus
// this INSERT's own BeginTx/RLS/INSERT/Commit (4). Folding the fence into
// the statement is not just cheaper reasoning, it measures out to exactly
// half the round trips on the tenanted path, and the fencing itself now
// costs nothing marginal over not fencing at all.
//
// $32/$33 (workerID/generation) fold the same (assigned_to, generation)
// check into the INSERT's own SELECT list via a WHERE clause, so a fenced
// write costs nothing beyond the write it was already making, and the fence
// and the write are the same statement -- there is no gap to argue about.
// An empty $32 is the escape hatch for every caller that does not have a
// claim to fence to (engine.fencingEnabled() false): the OR makes the WHERE
// unconditionally true, so this is exactly the unfenced INSERT it always
// was. See flushEvent's doc for how a caller that does supply $32/$33
// distinguishes "fenced out" from "this row was already terminal" when the
// statement reports zero rows affected -- the two are not the same thing,
// and this WHERE composes with, rather than replaces, the ON CONFLICT ...
// WHERE clause below that already encodes the second one.
//
// This shape -- SELECT $1, $2, ... WHERE EXISTS (...), no FROM, no CTE --
// was checked against a real PostgreSQL instance before relying on it:
// PREPARE with this exact form (placeholders repeated between the SELECT
// list and the WHERE EXISTS subquery, no explicit ::type casts) resolves
// every parameter's type from the INSERT target column list alone. That is
// not true of every INSERT ... SELECT shape -- the tenanted path below still
// cannot use a `WITH cfg AS (SELECT set_config(...))` CTE the way
// adaptive_flush.go's batch insert does, because joining a CTE into the FROM
// clause is what actually loses that inference, forcing an explicit cast per
// parameter. A bare WHERE EXISTS, with no FROM clause at all, does not have
// that problem.
const insertEventSQL = `
	INSERT INTO event_history (workflow_id, step, event_type, service, operation, request, response, error,
		duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
		defer_description, defer_id, child_name, child_input, run_id, new_input,
		plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
		promise_name, promise_id, promise_result, promise_error, payload,
		checksum, created_at, tenant_id)
	SELECT $1, $2, $3, $4, $5, $6, $7, $8,
		$9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19,
		$20, $21, $22, $23, $24, $25, $26, $27, $28, $29,
		$30, NOW(), $31
	WHERE ($32 = '' OR EXISTS (
		SELECT 1 FROM workflow_instances WHERE id = $1 AND assigned_to = $32 AND generation = $33
	))
	ON CONFLICT (workflow_id, step) DO UPDATE SET response = EXCLUDED.response, error = EXCLUDED.error WHERE event_history.response = '' AND event_history.error IS NULL`

// setRLSOnTx sets the transaction-local tenant context that the row-level
// security policies require.
//
// event_history's policy is `tenant_id = assert_tenant_set()`, and
// assert_tenant_set() raises if cleat.tenant_id is unset. Until this existed,
// no flush path set it: flushEvent wrote through e.db directly and the quota
// path opened a transaction without ever scoping it. On the owner connection
// that is invisible, because a superuser is exempt from RLS -- and every
// database test in this package connects as the owner. On the cleat_app
// connection the worker actually serves traffic on, every insert was rejected,
// and engine/lifecycle.go logged the rejection and carried on. See
// TestEventFlushPersistsUnderRLS.
//
// Transaction-local (set_config's third argument is true) rather than session
// level: e.db is a pool, and a session-scoped setting would outlive the
// statement and leak one workflow's tenant onto the next connection borrower.
//
// The adaptive flusher already did this, with a `WITH cfg AS (SELECT
// set_config(...))` CTE joined into its FROM clause. That is why the defect was
// intermittent rather than total: once a workflow's event rate pushed the
// flusher into batch mode its events persisted, and below that threshold --
// which is where most workflows live -- they did not. Only the two paths that
// bypassed the adaptive flusher were affected.
func setRLSOnFlushTx(ctx context.Context, tx *sql.Tx, tenantID string) error {
	_, err := tx.ExecContext(ctx, "SELECT set_config('cleat.tenant_id', $1, true)", tenantID)
	return err
}

// flushEvent persists a single event to event_history. Each step is one INSERT
// that auto-commits; no explicit transaction is needed. The checksum chain is
// tracked in-memory (execSession.lastChecksum) to avoid a DB round-trip.
//
// # Fencing (B4)
//
// This is the per-step write B4 found unfenced: a worker that stalled, was
// reaped (generation bumped, assigned_to cleared, workflow reclaimed), and
// then resumed could flush an event here and have it persist permanently,
// interleaved with its successor's writes -- even though the same worker's
// eventual FinalizeWorkflowSegment would correctly fail its own fence check
// and roll back. The event row does not roll back with it, because it was
// never in that transaction.
//
// The fence is folded into insertEventSQL itself -- see that constant's doc
// for why, and for the round-trip cost this avoids relative to an earlier,
// discarded version of this fix that called Heartbeat as a separate
// statement before every write. fenceParams returns ("", 0) when the engine
// was not constructed with both WithWorkerID and WithGeneration (see
// fencingEnabled), which the SQL's empty-$32 disjunct treats as "no fence
// requested" -- exactly the unfenced INSERT this was before B4.
//
// A fenced write can report zero rows affected for two different reasons,
// and they are not the same thing: the fence was lost, or the row already
// carries a terminal response/error and the pre-existing
//
//	ON CONFLICT ... WHERE event_history.response = '' AND error IS NULL
//
// clause correctly declined to overwrite it (an idempotent re-flush, not a
// bug). afterFencedInsert disambiguates the rare zero-rows case with one
// extra Heartbeat call -- paid only there, not on every write, since the
// common case (a row was actually written) never reaches it.
//
// On fence loss this returns ErrFenceLost rather than silently dropping the
// write. It does not abort the workflow's execution session itself -- that
// would require recordEvent (lifecycle.go) and its callers to change control
// flow on a specific error, which is a session-lifetime decision belonging to
// that code, not to the write path. What this function guarantees on its own
// is narrower and sufficient for B4: a write made under a lost fence does not
// become a permanent row. A zombie that keeps running after this returns
// ErrFenceLost keeps failing every subsequent flush and call-intent write the
// same way, and its FinalizeWorkflowSegment fails its own fence at the end
// exactly as it did before this change -- so it burns CPU until then, but it
// can no longer leave anything durable behind for its successor to collide
// with. That residual (a reaped worker is not stopped early, only prevented
// from writing) is the same trade-off CLAUDE.md's wazero section describes
// for compute-bound guests: detecting a lost lease is not the same problem as
// halting execution, and this change solves the first, not the second.
func (e *Engine) flushEvent(ctx context.Context, workflowID string, rec EventRecord, prevChecksum string) error {
	if e.db == nil || e.noPerStepFlush {
		return nil
	}

	// Everything below this point is PostgreSQL-dialect SQL. When the store
	// says otherwise, hand the event to it instead -- see perStepEventFlusher.
	// PostgresStore does not implement the interface, so the primary dialect
	// still takes the path it always has.
	//
	// MySQLStore's and MSSQLStore's per-step insert goes through
	// appendEventsInTxOpts, a function FinalizeWorkflowSegment and
	// AppendEventHistoryBatch also call for genuinely unfenced, multi-event
	// batch writes. Folding a fence predicate into it the way insertEventSQL
	// does below would fence those other callers too, or require forking a
	// second, single-purpose insert implementation to insulate them -- see
	// flush_dialect.go's perStepEventFlusher doc for why that trade was not
	// taken. Those two dialects keep a Heartbeat-before-write check instead,
	// right here rather than inside flushEventForStep, so it reads as what it
	// is: a narrower fallback for the two dialects that cannot use the
	// single-statement form, not a parallel fencing mechanism.
	if f, ok := e.workflowStore.(perStepEventFlusher); ok {
		if e.fencingEnabled() {
			held, err := e.workflowStore.Heartbeat(ctx, workflowID, e.workerID, e.generation)
			if err != nil {
				return fmt.Errorf("flush event: fence check: %w", err)
			}
			if !held {
				return ErrFenceLost
			}
		}
		return f.flushEventForStep(ctx, workflowID, rec)
	}

	flushStart := time.Now()
	defer func() {
		if DebugTiming {
			e.log().InfoContext(ctx, "TIMING: flushEvent db tx", "workflow_id", workflowID, "step", rec.Step, "total_ms", time.Since(flushStart).Milliseconds())
		}
	}()

	checksum := computeEventChecksum(rec, prevChecksum)
	payloadJSON, _ := eventRecordToPayload(rec)
	payloadArg := nullStr("")
	if len(payloadJSON) > 0 {
		payloadArg = sql.NullString{String: string(payloadJSON), Valid: true}
	}

	requestStr := tryEncodeBase64(rec.Request)
	responseStr := tryEncodeBase64(rec.Response)
	errStr := rec.Err
	sigPayload := rec.SignalPayload
	childInput := rec.ChildInput
	newInput := rec.NewInput
	pluginInput := rec.PluginInput
	pluginOutput := rec.PluginOutput
	promiseResult := rec.PromiseResult
	promiseError := rec.PromiseError

	// Encrypt sensitive payload fields when encryption is enabled.
	if e.encryptSensitivePayloads && e.encryption != nil {
		var encErr error
		if requestStr, encErr = e.encryption.EncryptString(rec.Request); encErr != nil {
			return fmt.Errorf("flush event: encrypt request: %w", encErr)
		}
		if responseStr, encErr = e.encryption.EncryptString(rec.Response); encErr != nil {
			return fmt.Errorf("flush event: encrypt response: %w", encErr)
		}
		if errStr, encErr = e.encryption.EncryptString(rec.Err); encErr != nil {
			return fmt.Errorf("flush event: encrypt err: %w", encErr)
		}
		if rec.SignalPayload != "" {
			if sigPayload, encErr = e.encryption.EncryptString(rec.SignalPayload); encErr != nil {
				return fmt.Errorf("flush event: encrypt signal_payload: %w", encErr)
			}
		}
		if rec.ChildInput != "" {
			if childInput, encErr = e.encryption.EncryptString(rec.ChildInput); encErr != nil {
				return fmt.Errorf("flush event: encrypt child_input: %w", encErr)
			}
		}
		if rec.NewInput != "" {
			if newInput, encErr = e.encryption.EncryptString(rec.NewInput); encErr != nil {
				return fmt.Errorf("flush event: encrypt new_input: %w", encErr)
			}
		}
		if rec.PluginInput != "" {
			if pluginInput, encErr = e.encryption.EncryptString(rec.PluginInput); encErr != nil {
				return fmt.Errorf("flush event: encrypt plugin_input: %w", encErr)
			}
		}
		if rec.PluginOutput != "" {
			if pluginOutput, encErr = e.encryption.EncryptString(rec.PluginOutput); encErr != nil {
				return fmt.Errorf("flush event: encrypt plugin_output: %w", encErr)
			}
		}
		if rec.PromiseResult != "" {
			if promiseResult, encErr = e.encryption.EncryptString(rec.PromiseResult); encErr != nil {
				return fmt.Errorf("flush event: encrypt promise_result: %w", encErr)
			}
		}
		if rec.PromiseError != "" {
			if promiseError, encErr = e.encryption.EncryptString(rec.PromiseError); encErr != nil {
				return fmt.Errorf("flush event: encrypt promise_error: %w", encErr)
			}
		}
		if len(payloadJSON) > 0 && e.encryption != nil {
			encrypted, encErr := e.encryption.EncryptJSON(payloadJSON)
			if encErr != nil {
				return fmt.Errorf("flush event: encrypt payload: %w", encErr)
			}
			payloadArg = sql.NullString{String: string(encrypted), Valid: true}
		}
	}

	fenceWorkerID, fenceGeneration := e.fenceParams()

	// Quota check path: needs explicit transaction for atomic read-then-insert.
	if e.maxQuotaEvents > 0 && e.workflowStore != nil {
		tx, err := e.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("flush event (quota): begin tx: %w", err)
		}
		defer tx.Rollback()
		if e.tenantID != "" {
			if err := setRLSOnFlushTx(ctx, tx, e.tenantID); err != nil {
				return fmt.Errorf("flush event (quota): set tenant context: %w", err)
			}
		}
		var currentCount int
		if err := tx.QueryRowContext(ctx, `SELECT event_count FROM workflow_instances WHERE id = $1`, workflowID).Scan(&currentCount); err != nil {
			return fmt.Errorf("flush event (quota): %w", err)
		}
		if currentCount >= e.maxQuotaEvents {
			return fmt.Errorf("flush event: event quota exceeded (max %d)", e.maxQuotaEvents)
		}
		res, err := tx.ExecContext(ctx, insertEventSQL, workflowID, rec.Step, rec.EventType,
			nullStr(rec.Service), nullStr(rec.Op), nullStr(requestStr), nullStr(responseStr), nullStr(errStr),
			nullInt64(rec.DurationMs), nullStr(rec.SignalNames), nullInt64(rec.TimeoutMs),
			nullStr(rec.SignalName), nullStr(sigPayload),
			nullStr(rec.DeferDescription), nullStr(rec.DeferID),
			nullStr(rec.ChildName), nullStr(childInput), nullStr(rec.RunID), nullStr(newInput),
			nullStr(rec.PluginName), nullStr(rec.PluginFunc), nullStr(pluginInput), nullStr(pluginOutput), nullStr(rec.PluginError),
			nullStr(rec.PromiseName), nullStr(rec.PromiseID), nullStr(promiseResult), nullStr(promiseError),
			payloadArg, checksum, e.tenantID, fenceWorkerID, fenceGeneration)
		if err != nil {
			return fmt.Errorf("flush event (quota): %w", err)
		}
		if err := e.afterFencedInsert(ctx, res, workflowID, fenceWorkerID, fenceGeneration); err != nil {
			return err
		}
		return tx.Commit()
	}

	args := []any{workflowID, rec.Step, rec.EventType,
		nullStr(rec.Service), nullStr(rec.Op), nullStr(requestStr), nullStr(responseStr), nullStr(errStr),
		nullInt64(rec.DurationMs), nullStr(rec.SignalNames), nullInt64(rec.TimeoutMs),
		nullStr(rec.SignalName), nullStr(sigPayload),
		nullStr(rec.DeferDescription), nullStr(rec.DeferID),
		nullStr(rec.ChildName), nullStr(childInput), nullStr(rec.RunID), nullStr(newInput),
		nullStr(rec.PluginName), nullStr(rec.PluginFunc), nullStr(pluginInput), nullStr(pluginOutput), nullStr(rec.PluginError),
		nullStr(rec.PromiseName), nullStr(rec.PromiseID), nullStr(promiseResult), nullStr(promiseError),
		payloadArg, checksum, e.tenantID, fenceWorkerID, fenceGeneration}

	// Tenanted path: the insert must carry the RLS context, which is
	// transaction-scoped, so it needs an explicit transaction. That costs two
	// extra round trips per event versus the auto-commit insert below. It buys
	// the insert actually happening: without it every flush on an RLS-enforced
	// connection is rejected and silently dropped.
	//
	// Fencing does not add a further round trip here, or to the untenanted
	// path below -- see insertEventSQL's doc for why the WHERE EXISTS form
	// (not a CTE) keeps this to one statement with no extra parameter casts.
	if e.tenantID != "" {
		tx, err := e.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("flush event: begin tx: %w", err)
		}
		defer tx.Rollback()
		if err := setRLSOnFlushTx(ctx, tx, e.tenantID); err != nil {
			return fmt.Errorf("flush event: set tenant context: %w", err)
		}
		res, err := tx.ExecContext(ctx, insertEventSQL, args...)
		if err != nil {
			return fmt.Errorf("flush event: %w", err)
		}
		if err := e.afterFencedInsert(ctx, res, workflowID, fenceWorkerID, fenceGeneration); err != nil {
			return err
		}
		return tx.Commit()
	}

	// Untenanted path: single INSERT auto-commits. No explicit BEGIN/COMMIT.
	res, err := e.db.ExecContext(ctx, insertEventSQL, workflowID, rec.Step, rec.EventType,
		nullStr(rec.Service), nullStr(rec.Op), nullStr(requestStr), nullStr(responseStr), nullStr(errStr),
		nullInt64(rec.DurationMs), nullStr(rec.SignalNames), nullInt64(rec.TimeoutMs),
		nullStr(rec.SignalName), nullStr(sigPayload),
		nullStr(rec.DeferDescription), nullStr(rec.DeferID),
		nullStr(rec.ChildName), nullStr(childInput), nullStr(rec.RunID), nullStr(newInput),
		nullStr(rec.PluginName), nullStr(rec.PluginFunc), nullStr(pluginInput), nullStr(pluginOutput), nullStr(rec.PluginError),
		nullStr(rec.PromiseName), nullStr(rec.PromiseID), nullStr(promiseResult), nullStr(promiseError),
		payloadArg, checksum, e.tenantID, fenceWorkerID, fenceGeneration)
	if err != nil {
		return fmt.Errorf("flush event: %w", err)
	}
	return e.afterFencedInsert(ctx, res, workflowID, fenceWorkerID, fenceGeneration)
}

// fenceParams returns the (workerID, generation) pair the fence clause in
// insertEventSQL expects -- its empty-$32-or-EXISTS disjunct, documented on
// that constant -- which is the real claim identity when
// fencingEnabled() is true, or ("", 0) -- the documented "no fence
// requested" sentinel -- otherwise.
func (e *Engine) fenceParams() (string, int64) {
	if !e.fencingEnabled() {
		return "", 0
	}
	return e.workerID, e.generation
}

// afterFencedInsert interprets the result of an insertEventSQL exec.
//
// Zero rows affected is not on its own evidence of a lost fence: the
// pre-existing ON CONFLICT ... WHERE clause also reports zero rows when the
// row already carries a terminal response/error (an idempotent re-flush,
// not a bug -- true before B4 and still true now). This disambiguates with
// one extra Heartbeat call, paid only on that rare path: fenceWorkerID == ""
// means no fence was requested, so a zero-row result can only be the
// idempotent case; otherwise, if the lease no longer holds, this is the
// zero-row result's real explanation and ErrFenceLost says so, and if the
// lease still holds, this falls back to the pre-existing "row already
// terminal" silence.
func (e *Engine) afterFencedInsert(ctx context.Context, res sql.Result, workflowID, fenceWorkerID string, fenceGeneration int64) error {
	if fenceWorkerID == "" {
		return nil
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("flush event: rows affected: %w", err)
	}
	if n > 0 {
		return nil
	}
	held, herr := e.workflowStore.Heartbeat(ctx, workflowID, fenceWorkerID, fenceGeneration)
	if herr != nil {
		return fmt.Errorf("flush event: fence check: %w", herr)
	}
	if !held {
		return ErrFenceLost
	}
	return nil
}

// runDefers invokes registered defer functions on a fresh module instance.
// Called on non-suspend errors to ensure cleanup runs even when the workflow fails.
//
// The pass is bounded as a whole, which is the part that was missing.
//
// Every caller passes context.Background() -- deliberately, so cleanup still
// happens when the workflow's own context has timed out or been cancelled
// (executor.go). The consequence nobody had measured is that each RunDefer then
// reaches configureStore with no deadline to reconcile against, so every defer
// gets a *fresh* copy of the backend-wide budget and the total scales with the
// number of defers. Measured 2026-09-01, backend timeout 2s, three runaway
// defers:
//
//	ctx with a 200ms deadline   150ms   ("199.778625ms wall-clock budget")
//	context.Background()        2.001s  ("2s wall-clock budget")
//	3 defers, Background        6.001s  -- 2s each, no ceiling
//
// On a worker that is 30s each (DefaultWasmtimeExecutionTimeout, settable with
// --wasm-instance-timeout), so a workflow registering twenty runaway defers
// held its slot for ten minutes. IMPROVEMENT-PLAN 3.31 recorded this as read
// off the code and explicitly not measured; the numbers above are that gap
// closed, and the multiplication is worse than the section predicted.
//
// One WithTimeout over the loop is the whole fix: configureStore already takes
// the tighter of ctx's remaining time and the backend's own timeout, so a
// single deadline shared by every iteration bounds both the pass and each
// defer within it, without touching the base context and therefore without
// re-coupling cleanup to the workflow's cancellation.
func (e *Engine) runDefers(ctx context.Context, wasmBytes []byte, deferrals map[string]string) {
	budget := e.deferPassBudget
	if budget <= 0 {
		budget = DefaultDeferPassBudget
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	type defEntry struct {
		id     string
		desc   string
		stepNo int // parsed from defer ID "defer-N"
	}
	var entries []defEntry
	for id, desc := range deferrals {
		stepNo := parseDeferStepNo(id)
		entries = append(entries, defEntry{id: id, desc: desc, stepNo: stepNo})
	}
	// Sort defers in LIFO order (higher stepNo first) so the most recently
	// registered defer runs first. Uses sort.Slice for clarity.
	sort.Slice(entries, func(i, j int) bool {
		return parseDeferStepNo(entries[i].id) < parseDeferStepNo(entries[j].id)
	})
	for _, entry := range entries {
		// Use the same naming convention as invokeDefersOnTrap:
		// "cleat_defer_" + deferID so both paths find the same export.
		deferName := "cleat_defer_" + entry.id
		if wasmBytes != nil {
			// Not propagated -- cleanup is best-effort and the original failure
			// takes priority -- but logged, which it was not. This was an `if`
			// with an empty body and a comment, so a defer that never ran was
			// indistinguishable from one that ran and succeeded.
			//
			// That is not hypothetical. A defer export declared with the wrong
			// signature is rejected before it executes with "expected 0 params,
			// but passed 4", and an author would have seen their cleanup
			// quietly not happen with nothing anywhere saying so. The same
			// silence made a test in IMPROVEMENT-PLAN 3.32 pass while executing
			// nothing at all. cmd/cleat-worker's own runDefers has always
			// logged here; this brings the two into line.
			if _, err := e.RunDefer(ctx, wasmBytes, deferName, nil); err != nil {
				// A missing export is not a failed cleanup.
				//
				// This pass is the LEGACY per-defer convention: one export per
				// defer, named cleat_defer_<id>. No guest in any language
				// emits one -- `grep -rn "cleat_defer_"` finds consumers and no
				// producers -- so for every SDK-built guest this branch was
				// reached once per registered defer and said "defer execution
				// failed" about a convention the guest was never expected to
				// follow. Since #559 and #560 it could say it immediately
				// AFTER the real cleanup had succeeded, which is worse than
				// useless: an operator reading it concludes their cleanup did
				// not happen when it did.
				//
				// errors.Is, not a substring match on the message. Matching the
				// wording is the same mistake one layer up, and this repo has
				// already had a check that matched an error message rather than
				// the condition and reported a broken database as healthy.
				if errors.Is(err, ErrExportNotFound) {
					e.log().DebugContext(ctx, "no per-defer export for this defer; the guest drains its own table",
						"workflow_id", e.workflowID, "defer_id", entry.id,
						"description", entry.desc, "export", deferName)
					continue
				}
				e.log().WarnContext(ctx, "defer execution failed",
					"workflow_id", e.workflowID, "defer_id", entry.id,
					"description", entry.desc, "export", deferName, "error", err)
			}
		}
	}
}
