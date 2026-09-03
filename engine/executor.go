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

	now := e.seedNowMs(replayHistory)

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
		// originalInput and eventCount were set only in executeCompiled, the
		// wazero path that CLI tooling uses. This is the backend path -- the
		// one a worker runs -- and without them the event cap did two wrong
		// things there and only there. eventCount restarted at 0 each segment,
		// so a cap meant to bound a whole workflow bounded one segment of it
		// and a workflow that suspends often never reached it. originalInput
		// was "", so the ContinueAsNew the cap records carried empty input and
		// the continued run started with none.
		originalInput: string(input),
		eventCount:    e.initialEventCount,
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

	// Apply the wall-clock ceiling if configured.
	//
	// NOT wasmInstanceTimeout, which is the epoch fence and bounds guest
	// EXECUTION (IMPROVEMENT-PLAN 3.90). Applying that here as well is what
	// made the epoch fence's exclusion of host wait unobservable: both bounded
	// wall clock, so a workflow waiting on slow services died here instead,
	// with "execution timed out" rather than "execution time limit exceeded"
	// and at the same moment.
	if ceiling := e.wallClockCeiling(execCtx); ceiling > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(execCtx, ceiling)
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

	// A defer segment replays a workflow whose outcome is already decided,
	// purely to run its outstanding cleanup (IMPROVEMENT-PLAN 3.35 phase 5).
	// The backend needs to know, because the drain happens on the suspension
	// the replay ends in, inside the store the backend owns and destroys
	// before Execute returns.
	//
	// Asserted rather than added to WasmBackend: see setDeferPhase. A backend
	// that does not implement it simply cannot run a defer segment, which is
	// the honest outcome rather than a silently skipped drain -- the engine
	// refuses one it cannot honour, below.
	if e.deferPhase {
		dp, ok := execBackend.(interface{ setDeferPhase(bool) })
		if !ok {
			return "", nil, nil, nil, nil, fmt.Errorf(
				"host: workflow %s: backend %q cannot run a defer segment; "+
					"its outstanding defers would be silently skipped",
				e.workflowID, execBackend.Name())
		}
		// And the guest has to be able to hear the stop. An SDK that does not
		// decode callSuspendSentinel reads it as an empty successful response
		// and runs on -- doing the new work the segment exists to prevent,
		// with nothing to see. Fail closed rather than silently.
		if lang := wasm.DetectLanguage(wasmBytes); !deferSegmentLanguages[lang] {
			return "", nil, nil, nil, nil, fmt.Errorf(
				"host: workflow %s: guest language %q has no defer-segment support; "+
					"its SDK does not decode the suspend sentinel, so the segment "+
					"would run the workflow body instead of only its defers",
				e.workflowID, lang)
		}
		dp.setDeferPhase(true)
	}

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
		//
		// A guest that returned an error is not a trap. resolveWasmTrap
		// prefixes "wasm trap: " onto any non-empty message, so before this
		// check an operator whose workflow simply returned an error read
		// "execution failed: wasm trap: host: export ... failed: <their
		// error>" -- a claim of a memory fault over their own error text.
		// The guest stopped cleanly and said it had failed. See 3.23.
		var guestErr *GuestReturnedError
		guestCompleted := errors.As(callErr, &guestErr)

		// Run defers only for a guest that never got to run its own.
		//
		// Reaching cleat_complete -- which is what GuestReturnedError marks --
		// means the guest came out through its entry point wrapper, and that
		// wrapper runs the registered defer bodies on the error path as well
		// as the success path (wasm/exports.go, IMPROVEMENT-PLAN 3.70). The
		// pass below would then look for an export named "cleat_defer_<id>"
		// that no guest in any language has ever had, and log the miss as
		// "defer execution failed" for a defer that had just run. An operator
		// reading that concludes their cleanup did not happen.
		//
		// A trap, a fence kill or a timeout is the other case: the guest was
		// stopped before its wrapper returned, so nothing ran its defers and
		// this pass is the only chance they have.
		if len(session.deferrals) > 0 && !guestCompleted {
			e.runDefers(context.Background(), wasmBytes, session.deferrals)
		}
		session.releaseHeldScopes(context.Background())
		if guestCompleted {
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

	// res may be nil here. Every error return in the wasmtime backend is
	// `return nil, err`, and this line is reached with callErr != nil whenever
	// session.suspendErr is also set -- the check above deliberately lets a
	// suspension win over the error that accompanied it. `res.Suspended` then
	// dereferences nil, in no recover, killing the worker process rather than
	// failing the workflow. The event cap was one way in (see freshCall); this
	// guard closes the shape rather than that one caller.
	if (res != nil && res.Suspended) || session.suspendErr != nil {
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
			// Same nil res as above: a suspension can arrive alongside a
			// backend error, and there is no result to carry when it does.
			result := ""
			if res != nil {
				result = res.Result
			}
			newRunID, cnErr := e.continueAsNewHandler(ctx, e.workflowID, e.workerID, int64(0), e.defName, e.defVersion, se.NewInput, newEvents, result, session.queryState, priority)
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

	now := e.seedNowMs(replayHistory)

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

	// Apply the wall-clock ceiling if configured.
	//
	// NOT wasmInstanceTimeout, which is the epoch fence and bounds guest
	// EXECUTION (IMPROVEMENT-PLAN 3.90). Applying that here as well is what
	// made the epoch fence's exclusion of host wait unobservable: both bounded
	// wall clock, so a workflow waiting on slow services died here instead,
	// with "execution timed out" rather than "execution time limit exceeded"
	// and at the same moment.
	if ceiling := e.wallClockCeiling(execCtx); ceiling > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(execCtx, ceiling)
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
			// withHandler, not a bare Background: IMPROVEMENT-PLAN §3.35 phase 2.
			//
			// The bare context is deliberate and stays -- defers have to run
			// when execCtx is already cancelled or timed out, which is exactly
			// when they matter most. What it was missing is the session, so
			// handlerFromContext's unchecked assertion panicked inside any host
			// call the defer body made:
			//
			//   interface conversion: interface is nil, not engine.HostHandler
			//     engine.handlerFromContext  engine/imports.go:20
			//
			// That was invisible until the line above started finding a defer
			// runner to call. While the drain looked for an export no guest
			// had, no body ever ran, so no body ever reached a host call, so
			// this panic had nothing to fire on. Two defects in series, and
			// fixing the outer one is what exposed the inner one.
			//
			// A defer that cannot call the host cannot release the lock it
			// took, which is most of what a defer is for.
			deferCtx := withHandler(context.Background(), session)
			notInvoked := e.invokeDefersOnTrap(deferCtx, mod, session.deferrals)
			if len(notInvoked) > 0 {
				e.runDefers(deferCtx, wasmBytes, notInvoked)
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
	// The handler is whatever ctx carries. That USED to be nothing -- no caller
	// put one there, so a defer that made a host call panicked on an unchecked
	// type assertion -- and IMPROVEMENT-PLAN 3.35 phase 2 fixed it: the
	// executor now passes withHandler(context.Background(), session), which
	// keeps the immunity to a cancelled execCtx and adds the session. A defer
	// body reached through here can call the host.
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

	// One export for the whole table, not one per defer.
	//
	// This asked the module for `cleat_defer_<id>`, once per registered defer,
	// until 2026-09-02. **No guest in any language has ever produced an export
	// by that name** -- `grep -rn "cleat_defer_"` finds consumers and no
	// producers -- so every defer took the not-found branch, every one was
	// handed to the fresh-module fallback, and the fallback looked for the
	// same name and failed the same way. Measured on an AssemblyScript guest
	// that traps with one defer outstanding: two warnings, zero cleanup.
	//
	// The convention that does exist is deferRunnerExport, emitted by every
	// SDK's codegen, and it drains the whole table in LIFO order in one call
	// because the bodies live in the guest (§3.73). That also makes the
	// per-defer bookkeeping below all-or-nothing, which is the honest shape:
	// the drain either happened or the guest could not offer it.
	fn := mod.ExportedFunction(deferRunnerExport)
	if fn == nil {
		// No runner: fall back to the per-defer convention below.
		//
		// Every SDK emits the runner, so in practice this is a guest built
		// before it existed. It is kept rather than deleted because a
		// hand-written guest can still export one function per defer, and the
		// partial-failure semantics that path carries are load-bearing --
		// see the loop's own comments and TestDeferBodyRunsOnceAfterATrap.
		return e.invokePerDeferExports(ctx, mod, deferrals)
	}

	// Called with no arguments and returning one i64 -- how many bodies ran --
	// so this does not go through CallExportWithSuspend, which marshals the
	// four-argument entry-point ABI.
	res, err := fn.Call(ctx)
	if err != nil {
		// Logged and not propagated: the original trap is the caller's error
		// and takes priority. NOT handed to the fallback either -- a defer
		// that ran and then failed must not be retried, because it is a
		// destructor and half-applying a compensating action and then applying
		// it again is worse than not retrying.
		e.log().WarnContext(ctx, "defer execution failed", "export_name", deferRunnerExport, "error", err)
		return notInvoked
	}
	var ran int64
	if len(res) > 0 {
		ran = int64(res[0])
	}
	e.log().InfoContext(ctx, "ran the defers of a trapped workflow",
		"defers_run", ran, "registered", len(deferrals))
	return notInvoked
}

// invokePerDeferExports is the legacy shape: one export per registered defer,
// named "cleat_defer_<id>".
//
// No SDK has ever emitted these -- `grep -rn "cleat_defer_"` finds consumers,
// and the only producers in the tree are hand-written WAT test fixtures. It is
// reached only by a guest with no deferRunnerExport, and it is kept for the
// guest that hand-rolls its exports rather than using an SDK.
//
// "Invoked" means the export was found and called, whatever the outcome. A
// defer that ran and trapped must NOT be retried on a fresh instance: it is a
// destructor, so its effects are the point, and half-applying a compensating
// action and then applying it again is worse than not retrying. Only a defer
// the live module could not offer at all is handed on.
func (e *Engine) invokePerDeferExports(ctx context.Context, mod api.Module, deferrals map[string]string) (notInvoked map[string]string) {
	notInvoked = make(map[string]string)
	for deferID, description := range deferrals {
		exportName := "cleat_defer_" + deferID
		fn := mod.ExportedFunction(exportName)
		if fn == nil {
			// Debug, not Warn: the legacy per-defer convention is one no SDK
			// emits, so its absence is the normal case rather than a fault.
			// See the note in flush.go's runDefers.
			e.log().DebugContext(ctx, "no per-defer export for this defer", "defer_id", deferID, "description", description, "export_name", exportName)
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
