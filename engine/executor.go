package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	"github.com/cleat-team/cleat/internal/telemetry"
	"github.com/cleat-team/cleat/wasm"
)

// backendForWasm looks up a WasmBackend for the given WASM binary by
// detecting its language and checking the registered backends map.
// Returns nil if no backend is registered for the detected language.
//
// Prefer resolveBackend: a nil from here is ambiguous between "this engine does
// no backend routing" and "this engine routes, but not for that language", and
// those two need opposite handling.
func (e *Engine) backendForWasm(wasmBytes []byte) WasmBackend {
	if e.backends == nil {
		return nil
	}
	lang := wasm.DetectLanguage(wasmBytes)
	if backend, ok := e.backends[lang]; ok {
		return backend
	}
	return nil
}

// resolveBackend picks the backend for a module, distinguishing the two cases a
// bare nil from backendForWasm conflates:
//
//   - (backend, nil)  — routed; execute on it.
//   - (nil, nil)      — this engine registers no backends at all, so the wazero
//     Runtime it was constructed with is the intended executor. That is the
//     cmd/cleatctl replay|debug, cmd/cleat run_embedded and cmd/cleat-bench
//     shape, and it stays working.
//   - (nil, err)      — this engine DOES route, but has no backend for this
//     module's language. Fail closed.
//
// That last case is the one worth spelling out, because three call sites used to
// treat it three different ways and all three were wrong.
//
// wasm.DetectLanguage returns the Language field of the guest's own
// "cleat.metadata" custom section verbatim (wasm/metadata.go), with no
// validation against WasmtimeLanguages. So the string that selects the execution
// path is supplied by whoever built the module. Measured 2026-08-31 against an
// engine registered exactly as cmd/cleat-worker registers it
// (WithBackends(WasmtimeLanguages, ...), nil Runtime):
//
//	declared "go"     -> routed to the backend
//	declared "cobol"  -> no backend
//	declared "tinygo" -> no backend
//	declared "GO"     -> no backend   (case alone is enough)
//
// and with no backend, the three paths did this:
//
//	RunDefer  compiled and ran the guest on a wazero Runtime it created on
//	          demand -- and CLAUDE.md records that wazero cannot be fenced for a
//	          compute-bound guest. A guest-chosen string selected an unstoppable
//	          runtime.
//	Replay    dereferenced e.rt, which the worker sets to nil, and panicked.
//	Execute   returned a clean error, having been given a nil check the other
//	          two never got.
//
// tiers.yaml grants no language outside WasmtimeLanguages (tier 1 is
// [go, python], tier 2 adds rust, java, assemblyscript), so failing closed here
// can only reject what was never claimed to work.
func (e *Engine) resolveBackend(wasmBytes []byte) (WasmBackend, error) {
	if backend := e.backendForWasm(wasmBytes); backend != nil {
		return backend, nil
	}
	if len(e.backends) == 0 {
		return nil, nil
	}
	return nil, fmt.Errorf(
		"host: no WASM backend registered for guest language %q (registered: %v); "+
			"the language comes from the module's own cleat.metadata section and is "+
			"not a supported guest language",
		wasm.DetectLanguage(wasmBytes), e.registeredLanguages())
}

// registeredLanguages returns the routed languages in sorted order, for error
// messages that are stable enough to assert on.
func (e *Engine) registeredLanguages() []string {
	langs := make([]string, 0, len(e.backends))
	for lang := range e.backends {
		langs = append(langs, lang)
	}
	sort.Strings(langs)
	return langs
}

// executeWithBackend runs a workflow execution (fresh or replay) using the
// given WasmBackend. The backend handles compilation and execution; the
// Engine manages the execSession, history, timeouts, and result handling.
func (e *Engine) executeWithBackend(
	ctx context.Context,
	backend WasmBackend,
	wasmBytes []byte,
	entryPoint string,
	input json.RawMessage,
	history []EventRecord,
) (result string, resultHistory []EventRecord, suspended *SuspendResult, deferrals map[string]string, queryState map[string]string, err error) {
	// If compaction state is set, merge virtual compacted events with tail
	// history to produce a complete replay history for deterministic replay.
	compactedStep := 0
	replayHistory := history
	if e.compactionState != nil && len(history) > 0 {
		replayHistory = buildFullHistoryFromCompaction(history, e.compactionState)
		compactedStep = e.compactionState.CompactedStep
	}

	now := nowMs.Load()
	if len(replayHistory) > 0 && replayHistory[0].TimestampMs > 0 {
		now = replayHistory[0].TimestampMs
	}

	session := &execSession{
		engine:       e,
		history:      replayHistory,
		isReplay:     len(replayHistory) > 0,
		nowMs:        now,
		deferrals:    make(map[string]string),
		workflowID:   e.workflowID,
		defName:      e.defName,
		execRunID:    e.workflowID,
		tenantID:     e.tenantID,
		stepCallback: e.stepCallback,
	}

	execCtx, stepCancel := context.WithCancel(ctx)
	session.stepCancel = stepCancel
	execCtx = withHandler(execCtx, session)

	execCtx, workflowSpan := telemetry.WorkflowSpan(execCtx,
		e.workflowID, e.defName, e.defVersion, e.tenantID, e.traceID)
	defer workflowSpan.End()

	// Apply overall workflow execution timeout if configured.
	if e.defaultWorkflowTimeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(execCtx, e.defaultWorkflowTimeout)
		defer cancel()
	}

	// Apply per-execution WASM instance timeout if configured.
	if e.wasmInstanceTimeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(execCtx, e.wasmInstanceTimeout)
		defer cancel()
	}

	// If replaying, verify event history integrity (checksums) and
	// validate version compatibility before proceeding.
	if len(replayHistory) > 0 {
		// (a) Checksum verification.
		if e.workflowEventVerifier != nil {
			if verr := e.workflowEventVerifier(ctx, e.workflowID); verr != nil {
				e.log().WarnContext(ctx, "checksum verification failed", "workflow_id", e.workflowID, "tenant_id", e.tenantID, "error", verr)
				if e.Metrics != nil {
					e.Metrics.RecordReplayChecksumFailure(ctx)
				}
				if e.failOnChecksumMismatch {
					return "", nil, nil, nil, nil, fmt.Errorf("host: workflow %s: checksum verification failed: %w", e.workflowID, verr)
				}
				e.log().WarnContext(ctx, "checksum verification failed but proceeding (failOnChecksumMismatch=false)", "workflow_id", e.workflowID, "tenant_id", e.tenantID)
			}
		}

		// (b) Version validation (always-on unless allowVersionMismatch).
		if e.versionValidateFn != nil && !e.allowVersionMismatch {
			if verr := e.versionValidateFn(); verr != nil {
				return "", nil, nil, nil, nil, fmt.Errorf("host: workflow %s: version validation failed: %w", e.workflowID, verr)
			}
		}
	}

	// Use a per-execution backend instance to prevent data races on
	// the handler/work-data fields when Execute is called concurrently.
	execBackend := backend.PerExecution()
	res, callErr := execBackend.Execute(execCtx, wasmBytes, entryPoint, input, session)
	// Defensive fallback, not the primary timeout mechanism. The wasmtime
	// backend bounds its own execution via epoch interruption tied to this
	// same execCtx deadline (see wasmtimeBackend.configureStore and
	// NewWasmtimeBackend in backend_wasmtime.go) and returns a non-nil
	// callErr when that bound is hit, so the timeout case is normally
	// already handled by the callErr != nil branch below. This check only
	// catches the residual case of a backend returning callErr == nil
	// after execCtx's deadline has already passed without detecting it
	// itself. It is cheap insurance, not a fencing mechanism: on wazero
	// specifically, a compute-bound guest that never yields back to the
	// host (a tight loop) is not interrupted by execCtx's deadline at
	// all, so fn.Call blocks past the deadline and this check is never
	// reached for that case either -- see CLAUDE.md, "wazero cannot be
	// fenced for a compute-bound guest" (measured three ways, all
	// failing, 2026-08-05). This only helps when the guest itself
	// returns control to the host before or around the deadline.
	//
	// This used to be the *only* timeout enforcement wasmtime had, and it
	// did not work: wasmtime-go does not observe ctx.Done() while fn.Call
	// is in progress, so for an infinite loop fn.Call never returns and
	// this line was never reached. See IMPROVEMENT-PLAN.md 1.5.
	if callErr == nil && execCtx.Err() == context.DeadlineExceeded {
		if len(session.deferrals) > 0 {
			e.runDefers(context.Background(), wasmBytes, session.deferrals)
		}
		session.releaseHeldScopes(context.Background())
		return "", stripCompactedEvents(session.history, compactedStep), nil, nil, nil, fmt.Errorf("host: workflow %s: execution timed out", e.workflowID)
	}
	if callErr != nil && session.suspendErr == nil {
		// Non-suspend error (trap, panic, timeout, or cancellation).
		// Try running defers on a fresh module.
		if len(session.deferrals) > 0 {
			e.runDefers(context.Background(), wasmBytes, session.deferrals)
		}
		session.releaseHeldScopes(context.Background())
		// A guest that returned an error is not a trap. resolveWasmTrap
		// prefixes "wasm trap: " onto any non-empty message, so before this
		// check an operator whose workflow simply returned an error read
		// "execution failed: wasm trap: host: export ... failed: <their
		// error>" -- a claim of a memory fault over their own error text.
		// The guest stopped cleanly and said it had failed. See 3.23.
		var guestErr *GuestReturnedError
		if errors.As(callErr, &guestErr) {
			return "", stripCompactedEvents(session.history, compactedStep), nil, nil, nil, session.classifyFailure(fmt.Errorf("host: workflow %s: execution failed: %w", e.workflowID, callErr))
		}
		if enriched := resolveWasmTrap(wasmBytes, callErr.Error()); enriched != "" {
			// wasmTrapError, not fmt.Errorf("%s"): resolveWasmTrap returns an
			// enriched *string*, and formatting it with %s dropped callErr out
			// of the chain, so errors.Is/errors.As stopped working for exactly
			// the errors that carry the most information -- traps. That is the
			// opposite of what wasmTrapError.Unwrap was written for. Keeping
			// the enriched text as the message and callErr as the cause gives
			// both.
			return "", stripCompactedEvents(session.history, compactedStep), nil, nil, nil, session.classifyFailure(&wasmTrapError{
				cause: callErr,
				msg:   fmt.Sprintf("host: workflow %s: execution failed: %s", e.workflowID, enriched),
			})
		}
		return "", stripCompactedEvents(session.history, compactedStep), nil, nil, nil, session.classifyFailure(fmt.Errorf("host: workflow %s: execution failed: %w", e.workflowID, callErr))
	}

	if res.Suspended || session.suspendErr != nil {
		se := session.suspendErr
		if se == nil {
			se = &SuspendError{Reason: "workflow suspended"}
		}
		if se.Until.IsZero() {
			se.Until = time.Now().Add(30 * time.Second)
		}

		susResult := &SuspendResult{
			History:      session.history,
			SuspendUntil: se.Until,
			Reason:       se.Reason,
			NewInput:     se.NewInput,
			NewVersion:   se.NewVersion,
			Deferrals:    session.deferrals,
		}
		if se.Reason == "continue_as_new" && e.continueAsNewHandler != nil && !session.isReplay {
			newEvents := session.history[len(replayHistory):]
			// generation is 0 because the engine does not yet track generation
			// for continue-as-new; this code path is dormant (handler is never
			// wired in current deployments).
			priority := 0
			if e.state != nil {
				priority = e.state.Priority()
			}
			newRunID, cnErr := e.continueAsNewHandler(ctx, e.workflowID, e.workerID, int64(0), e.defName, e.defVersion, se.NewInput, newEvents, res.Result, session.queryState, priority)
			if cnErr != nil {
				return "", stripCompactedEvents(session.history, compactedStep), nil, nil, nil, fmt.Errorf("host: workflow %s: continue_as_new handler failed: %w", e.workflowID, cnErr)
			}
			susResult.ContinueAsNewHandled = true
			susResult.NewRunID = newRunID
		}

		return "", stripCompactedEvents(session.history, compactedStep), susResult, session.deferrals, session.queryState, nil
	}

	// Workflow completed successfully. Release any held scopes.
	session.releaseHeldScopes(ctx)
	return res.Result, stripCompactedEvents(session.history, compactedStep), nil, session.deferrals, session.queryState, nil
}

// executeCompiled runs a fresh execution using a pre-compiled module.
// history is the event history to replay (nil for fresh execution).
func (e *Engine) executeCompiled(ctx context.Context, compiled wazero.CompiledModule, entryPoint string, input json.RawMessage, history []EventRecord, wasmBytes []byte) (string, []EventRecord, *SuspendResult, map[string]string, map[string]string, error) {
	if e.rt == nil {
		return "", nil, nil, nil, nil, fmt.Errorf("host: no runtime available for WASM execution")
	}
	mod, err := e.rt.InstantiateModule(ctx, compiled)
	if err != nil {
		return "", nil, nil, nil, nil, fmt.Errorf("host: instantiate module: %w", err)
	}
	defer mod.Close(ctx)

	if err := e.rt.InitModule(ctx, mod); err != nil {
		return "", nil, nil, nil, nil, fmt.Errorf("host: init module: %w", err)
	}

	// If compaction state is set, merge virtual compacted events with tail history
	// to produce a complete replay history for deterministic replay.
	compactedStep := 0
	replayHistory := history
	if e.compactionState != nil && len(history) > 0 {
		replayHistory = buildFullHistoryFromCompaction(history, e.compactionState)
		compactedStep = e.compactionState.CompactedStep
	}

	now := nowMs.Load()
	if len(replayHistory) > 0 && replayHistory[0].TimestampMs > 0 {
		now = replayHistory[0].TimestampMs
	}

	session := &execSession{
		engine:        e,
		history:       replayHistory,
		isReplay:      len(replayHistory) > 0,
		nowMs:         now,
		deferrals:     make(map[string]string),
		workflowID:    e.workflowID,
		defName:       e.defName,
		execRunID:     e.workflowID,
		tenantID:      e.tenantID,
		originalInput: string(input),
		eventCount:    e.initialEventCount,
		stepCallback:  e.stepCallback,
	}

	execCtx, stepCancel := context.WithCancel(ctx)
	session.stepCancel = stepCancel
	execCtx = withHandler(execCtx, session)

	execCtx, workflowSpan := telemetry.WorkflowSpan(execCtx,
		e.workflowID, e.defName, e.defVersion, e.tenantID, e.traceID)
	defer workflowSpan.End()

	// Apply overall workflow execution timeout if configured.
	// This wraps the entire execution including replay and fresh run.
	if e.defaultWorkflowTimeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(execCtx, e.defaultWorkflowTimeout)
		defer cancel()
	}

	// Apply per-execution WASM instance timeout if configured.
	if e.wasmInstanceTimeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(execCtx, e.wasmInstanceTimeout)
		defer cancel()
	}

	// If replaying, verify event history integrity (checksums) and
	// validate version compatibility before proceeding.
	if len(replayHistory) > 0 {
		// (a) Checksum verification.
		if e.workflowEventVerifier != nil {
			if err := e.workflowEventVerifier(ctx, e.workflowID); err != nil {
				e.log().WarnContext(ctx, "replay checksum verification failed", "workflow_id", e.workflowID, "tenant_id", e.tenantID, "error", err)
				if e.Metrics != nil {
					e.Metrics.RecordReplayChecksumFailure(ctx)
				}
				if e.failOnChecksumMismatch {
					return "", nil, nil, nil, nil, fmt.Errorf("host: workflow %s: checksum verification failed: %w", e.workflowID, err)
				}
				e.log().WarnContext(ctx, "replay checksum verification failed but proceeding (failOnChecksumMismatch=false)", "workflow_id", e.workflowID, "tenant_id", e.tenantID)
			}
		}

		// (b) Version validation (always-on unless allowVersionMismatch).
		if e.versionValidateFn != nil && !e.allowVersionMismatch {
			if err := e.versionValidateFn(); err != nil {
				return "", nil, nil, nil, nil, fmt.Errorf("host: workflow %s: version validation failed: %w", e.workflowID, err)
			}
		}
	}

	result, err := e.rt.CallExport(execCtx, mod, entryPoint, input)
	if err != nil {
		if errors.Is(err, ErrSuspended) || session.suspendErr != nil {
			se := session.suspendErr
			if se == nil {
				se = &SuspendError{Reason: "workflow suspended"}
			}
			if se.Until.IsZero() {
				// Default: wake in 10 minutes. External events (child
				// completion via wakeParent, signal delivery) wake the
				// parent earlier. This fallback catches edge cases where
				// the wake mechanism fails and prevents infinite hangs.
				se.Until = time.Now().Add(10 * time.Minute)
			}

			// If ContinueAsNew was triggered and the engine has a handler,
			// call it now to atomically persist the transition inline.
			// This eliminates the race window between returning and the
			// worker calling store.ContinueAsNew separately.
			susResult := &SuspendResult{
				History:      session.history,
				SuspendUntil: se.Until,
				Reason:       se.Reason,
				NewInput:     se.NewInput,
				NewVersion:   se.NewVersion,
				Deferrals:    session.deferrals,
			}
			if se.Reason == "continue_as_new" && e.continueAsNewHandler != nil && !session.isReplay {
				// generation is 0 because the engine does not yet track generation
				// for continue-as-new; this code path is dormant (handler is never
				// wired in current deployments).
				newEvents := session.history[len(replayHistory):]
				priority := 0
				if e.state != nil {
					priority = e.state.Priority()
				}
				newRunID, cnErr := e.continueAsNewHandler(ctx, e.workflowID, e.workerID, int64(0), e.defName, e.defVersion, se.NewInput, newEvents, result, session.queryState, priority)
				if cnErr != nil {
					return "", stripCompactedEvents(session.history, compactedStep), nil, nil, nil, fmt.Errorf("host: workflow %s: continue_as_new handler failed: %w", e.workflowID, cnErr)
				}
				susResult.ContinueAsNewHandled = true
				susResult.NewRunID = newRunID
			}

			strippedHistory := stripCompactedEvents(session.history, compactedStep)
			return "", strippedHistory, susResult, session.deferrals, session.queryState, nil
		}
		// Workflow failed with a non-suspend error (trap, panic, timeout,
		// or cancellation). Try running defers on the still-live module
		// first, then fall back to fresh-module defers for the ones it
		// could not offer.
		//
		// The fall-back used to be unconditional, so every defer body ran
		// TWICE -- once on the live module and once on a fresh one. Measured
		// 2026-09-01 with a guest that registers one defer and traps: two
		// "defer execution failed" lines for the same defer_id, each carrying
		// the trap from the defer function itself, so both had reached the
		// body. The comment said "fall back" and there was no conditional.
		//
		// A defer is a destructor, so a doubled body is a doubled effect: a
		// compensating saga step applied twice, a lock released twice, a
		// notification sent twice.
		//
		// Not the worker: this is executeCompiled, reached only when no
		// backend is registered -- cleatctl replay|debug, cleat run,
		// cleat-bench, and the public testing packages cleat/wasmtest,
		// cleat/cleattest, cleat/embedded. That last group is the reason this
		// matters rather than the reason it does not: a user testing a
		// compensating defer saw it fire twice under the harness and once in
		// production, so the harness disagreed with the runtime in the
		// direction that makes a real double-compensation look like a test
		// artifact.
		//
		// Use context.Background() so defer functions execute even when the
		// execCtx has been cancelled or timed out (e.g., workflow timeout).
		if len(session.deferrals) > 0 {
			notInvoked := e.invokeDefersOnTrap(context.Background(), mod, session.deferrals)
			if len(notInvoked) > 0 {
				e.runDefers(context.Background(), wasmBytes, notInvoked)
			}
		}
		session.releaseHeldScopes(context.Background())
		// Attempt to resolve the WASM trap to a source location using
		// DWARF debug info. wazero v1.9.0 already embeds DWARF-resolved
		// file:line locations in trap errors; resolveWasmTrap ensures
		// consistent formatting and serves as a hook for future custom
		// DWARF parsing from the raw wasm binary.
		if enriched := resolveWasmTrap(wasmBytes, err.Error()); enriched != "" {
			// See the note at the other resolveWasmTrap site: %s dropped the
			// cause and broke errors.Is/errors.As for every trap.
			return "", stripCompactedEvents(session.history, compactedStep), nil, nil, nil, &wasmTrapError{
				cause: err,
				msg:   fmt.Sprintf("host: workflow %s: execution failed: %s", e.workflowID, enriched),
			}
		}
		return "", stripCompactedEvents(session.history, compactedStep), nil, nil, nil, fmt.Errorf("host: workflow %s: execution failed: %w", e.workflowID, err)
	}

	// Workflow completed successfully. Release any held scopes.
	session.releaseHeldScopes(ctx)
	return result, stripCompactedEvents(session.history, compactedStep), nil, session.deferrals, session.queryState, nil
}

// RunDefer invokes a defer cleanup function in the WASM module.
// This is called by the worker on workflow exit (after the main entry point
// returns) to run registered defer callbacks in LIFO order.
func (e *Engine) RunDefer(ctx context.Context, wasmBytes []byte, deferName string, input json.RawMessage) (string, error) {
	// Run the defer on the same backend the workflow itself runs on, so the
	// execution fence applies to it.
	//
	// This function used to reach straight for e.rt -- the wazero Runtime --
	// and, when that was nil (which is exactly the case when wasmtime is
	// handling execution), build a *fresh wazero Runtime* for the defer. So
	// every deferred callback in cleat ran on wazero whatever the routing table
	// said, and wazero cannot be bounded for a guest that never calls into the
	// host: not by WithCloseOnContextDone, which breaks all execution; not by
	// fuel, which only decrements on function entry; and not by closing the
	// module, which has no effect on a tight loop. All three measured, see
	// IMPROVEMENT-PLAN 3.32. A defer that loops held its worker slot until the
	// process died.
	//
	// The handler is whatever ctx carries, which today is nothing: no caller
	// puts one there, so a defer that makes a host call fails. It already
	// failed before this change -- by panicking on an unchecked type assertion
	// and being swallowed -- and now arrives as a recovered error that
	// runDefers logs.
	//
	// Read that as a description of the current implementation, NOT as a rule
	// about what a defer may do. It is neither: a defer is meant to be a
	// destructor, with the workflow context needed to actually clean up, and
	// the design for that is IMPROVEMENT-PLAN 3.35 -- run defers in a
	// *replayed* instance with a live session, so state is reconstructed the
	// way it is for any resumption. This change is only the fence, and is
	// deliberately silent on the semantics rather than fixing them here.
	//
	// PerExecution, not the backend itself: Execute stores the handler on the
	// backend struct, so calling it on the shared root would race a concurrent
	// workflow execution. executeWithBackend takes the same precaution.
	backend, err := e.resolveBackend(wasmBytes)
	if err != nil {
		return "", err
	}
	if backend != nil {
		res, err := backend.PerExecution().Execute(ctx, wasmBytes, deferName, input, handlerFromContextOrNil(ctx))
		if err != nil {
			return "", err
		}
		if res == nil {
			return "", nil
		}
		return res.Result, nil
	}

	// No backend AND no backends registered at all -- resolveBackend returned
	// (nil, nil), so this engine does no routing and the wazero Runtime it was
	// built with is the intended executor. That is cmd/cleatctl replay|debug,
	// cmd/cleat run_embedded, cmd/cleat-bench and cleat/wasmtest.
	//
	// This comment used to say "the CGO-less build, where wazero is the only
	// runtime there is. Unfenced, and unavoidably so." Both halves were wrong.
	// A CGO-less worker exits at startup (CLAUDE.md), so it never gets here;
	// what did get here was a CGO build handed a module whose cleat.metadata
	// named a language outside WasmtimeLanguages -- guest-supplied input
	// selecting an unfenceable runtime. resolveBackend now rejects that case
	// before this point, so what remains really is unavoidable: an engine with
	// no backends has nothing else to run on.
	rt := e.rt
	if rt == nil {
		var err error
		rt, err = NewRuntime(ctx, 0, 0)
		if err != nil {
			return "", fmt.Errorf("host: create runtime for defer: %w", err)
		}
		defer rt.Close(ctx)
	}

	compiled, err := rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		return "", fmt.Errorf("host: compile module for defer: %w", err)
	}
	defer compiled.Close(ctx)

	return e.runDeferCompiledWithRT(ctx, rt, compiled, deferName, input)
}

// runDeferCompiledWithRT is the shared defer execution path, accepting an
// explicit Runtime so callers can supply a temporary Runtime when needed.
func (e *Engine) runDeferCompiledWithRT(ctx context.Context, rt *Runtime, compiled wazero.CompiledModule, deferName string, input json.RawMessage) (string, error) {
	mod, err := rt.InstantiateModule(ctx, compiled)
	if err != nil {
		return "", fmt.Errorf("host: instantiate module for defer: %w", err)
	}
	defer mod.Close(ctx)

	if err := rt.InitModule(ctx, mod); err != nil {
		return "", fmt.Errorf("host: init module for defer: %w", err)
	}

	return rt.CallExport(ctx, mod, deferName, input)
}

// RunDeferCompiled is like RunDefer but takes a pre-compiled module.
func (e *Engine) RunDeferCompiled(ctx context.Context, compiled wazero.CompiledModule, deferName string, input json.RawMessage) (string, error) {
	rt := e.rt
	if rt == nil {
		var err error
		rt, err = NewRuntime(ctx, 0, 0)
		if err != nil {
			return "", fmt.Errorf("host: create runtime for defer: %w", err)
		}
		defer rt.Close(ctx)
	}
	return e.runDeferCompiledWithRT(ctx, rt, compiled, deferName, input)
}

// invokeDefersOnTrap attempts to invoke registered defer callbacks after a WASM trap.
// Each defer is called as a separate export. Failures are logged but not returned —
// the original trap error takes priority.
//
// It returns the deferrals it did NOT invoke, so the caller can fall back to a
// fresh module for those and only those. The caller used to run both passes
// unconditionally, which executed every defer body twice; see the call site for
// the measurement.
//
// There is exactly one caller. There were two until IMPROVEMENT-PLAN 3.65
// deleted the component decomposition path, and the note that used to be here
// -- "the other call site discards the return value deliberately" -- went with
// it. If a second caller is ever added, decide explicitly whether it falls
// back; discarding this value silently is how the doubling happened.
//
// "Invoked" means the export was found and called, whatever the outcome. A
// defer that ran and trapped must NOT be retried on a fresh instance: it is a
// destructor, so its effects are the point, and half-applying a compensating
// action and then applying it again is worse than not retrying. Only a defer
// the live module could not offer at all is handed on.
func (e *Engine) invokeDefersOnTrap(ctx context.Context, mod api.Module, deferrals map[string]string) (notInvoked map[string]string) {
	notInvoked = make(map[string]string)
	for deferID, description := range deferrals {
		exportName := "cleat_defer_" + deferID
		fn := mod.ExportedFunction(exportName)
		if fn == nil {
			e.log().WarnContext(ctx, "defer export not found", "defer_id", deferID, "description", description, "export_name", exportName)
			notInvoked[deferID] = description
			continue
		}
		_, _, err := e.rt.CallExportWithSuspend(ctx, mod, exportName, []byte("{}"))
		if err != nil {
			e.log().WarnContext(ctx, "defer execution failed", "defer_id", deferID, "description", description, "error", err)
		}
	}
	return notInvoked
}

// DispatchUpdate dispatches an update to a workflow by invoking its registered handler.
// The handler receives the update name and payload JSON, and returns the result JSON.
// Returns an error if no update handler is configured on the engine.
func (e *Engine) DispatchUpdate(ctx context.Context, name, payload string) (string, error) {
	if e.updateHandler == nil {
		return "", fmt.Errorf("host: no update handler configured for this engine. Call WithUpdateHandler before DispatchUpdate.")
	}
	return e.updateHandler(name, payload)
}
