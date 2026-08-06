package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"

	"github.com/cleat-team/cleat/internal/telemetry"
	"github.com/cleat-team/cleat/wasm"
)

// backendForWasm looks up a WasmBackend for the given WASM binary by
// detecting its language and checking the registered backends map.
// Returns nil if no backend is registered for the detected language.
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
	// itself (wazero, for example, honors ctx cancellation directly and
	// should not need this either, but the fallback is cheap insurance).
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
		if enriched := resolveWasmTrap(wasmBytes, callErr.Error()); enriched != "" {
			// wasmTrapError, not fmt.Errorf("%s"): resolveWasmTrap returns an
			// enriched *string*, and formatting it with %s dropped callErr out
			// of the chain, so errors.Is/errors.As stopped working for exactly
			// the errors that carry the most information -- traps. That is the
			// opposite of what wasmTrapError.Unwrap was written for. Keeping
			// the enriched text as the message and callErr as the cause gives
			// both.
			return "", stripCompactedEvents(session.history, compactedStep), nil, nil, nil, &wasmTrapError{
				cause: callErr,
				msg:   fmt.Sprintf("host: workflow %s: execution failed: %s", e.workflowID, enriched),
			}
		}
		return "", stripCompactedEvents(session.history, compactedStep), nil, nil, nil, fmt.Errorf("host: workflow %s: execution failed: %w", e.workflowID, callErr)
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
		// first, then fall back to fresh-module defers.
		// Use context.Background() so defer functions execute even when the
		// execCtx has been cancelled or timed out (e.g., workflow timeout).
		if len(session.deferrals) > 0 {
			e.invokeDefersOnTrap(context.Background(), mod, session.deferrals)
			e.runDefers(context.Background(), wasmBytes, session.deferrals)
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

// executeComponent runs a fresh execution using a WASM Component Model binary
// that has been decomposed into a ComponentBundle. It instantiates all core
// modules following the component's instance DAG, wires cross-module imports
// using wazero's experimental ImportResolver, and calls the component's entry
// point export.
//
// The implementation follows the same patterns as executeCompiled: it sets up
//
// NOTE: Fresh-execution only (isReplay: false, history: nil). stepCallback and
// stepCancel are intentionally NOT wired because there is no replay path.
// an execSession for host function routing, uses the standard CallExport
// calling convention, and handles suspension and event history the same way.
func (e *Engine) executeComponent(ctx context.Context, bundle *wasm.ComponentBundle,
	entryPoint string, input json.RawMessage) (string, []EventRecord, *SuspendResult, map[string]string, map[string]string, error) {

	if e.rt == nil {
		return "", nil, nil, nil, nil, fmt.Errorf("host: no runtime available for component execution")
	}

	// ---- Step 1: Compile all core modules ----
	const componentAdapterModule = "__component_adapter__"

	// Per-execution stdout/stderr buffers to avoid racing on Runtime's
	// shared buffers when multiple component-model workflows execute
	// concurrently on the same Runtime.
	var execStdout, execStderr bytes.Buffer

	compiled := make([]wazero.CompiledModule, len(bundle.Modules))
	for i, w := range bundle.Modules {
		w = wasm.PatchEmptyImportModuleName(w, componentAdapterModule)
		var err error
		compiled[i], err = e.rt.CompileModule(ctx, w)
		if err != nil {
			return "", nil, nil, nil, nil, fmt.Errorf("host: workflow %s: compile core module %d: %w", e.workflowID, i, err)
		}
		defer compiled[i].Close(ctx)
	}

	// ---- Step 2: Set up execution session ----
	now := nowMs.Load()
	session := &execSession{
		engine:     e,
		history:    nil, // fresh execution, no history
		isReplay:   false,
		nowMs:      now,
		deferrals:  make(map[string]string),
		workflowID: e.workflowID,
		defName:    e.defName,
		execRunID:  e.workflowID,
		tenantID:   e.tenantID,
	}
	execCtx := withHandler(ctx, session)

	execCtx, workflowSpan := telemetry.WorkflowSpan(execCtx,
		e.workflowID, e.defName, e.defVersion, e.tenantID, e.traceID)
	defer workflowSpan.End()

	if e.defaultWorkflowTimeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(execCtx, e.defaultWorkflowTimeout)
		defer cancel()
	}
	if e.wasmInstanceTimeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(execCtx, e.wasmInstanceTimeout)
		defer cancel()
	}

	// ---- Step 3: Walk instance DAG and instantiate core modules ----
	// resolvedInstances[i] is the wazero api.Module for instance i (nil for
	// FromExports-only instances).
	resolvedInstances := make([]api.Module, len(bundle.Instances))

	// resolveModuleForInstance returns the real module for an instance,
	// or nil for FromExports instances that have no module yet.
	resolveModuleForInstance := func(instIdx int) api.Module {
		if instIdx < 0 || instIdx >= len(resolvedInstances) {
			return nil
		}
		if m := resolvedInstances[instIdx]; m != nil {
			return m
		}
		return nil
	}

	// Keep track of all instantiated modules for cleanup.
	var cleanupMods []api.Module
	defer func() {
		for _, m := range cleanupMods {
			m.Close(ctx)
		}
	}()

	for i, inst := range bundle.Instances {
		if inst.ModuleIndex < 0 {
			// FromExports-only: no actual module instantiation.
			// The export aliases are resolved in Step 4.
			continue
		}

		cm := compiled[inst.ModuleIndex]

		// Build a map from import module name (as used by this module's
		// WASM import section) to the source instance index that provides it.
		importNameToInstance := make(map[string]int, len(inst.Args))
		for _, arg := range inst.Args {
			importNameToInstance[arg.Name] = arg.InstanceIndex
			// Also map the synthetic name used to replace empty module
			// names to the same source instance.
			if arg.Name == "" {
				importNameToInstance[componentAdapterModule] = arg.InstanceIndex
			}
		}

		// Use the experimental ImportResolver to redirect cross-module imports.
		// Host modules ("env", "wasi_snapshot_preview1", "teavm") are already
		// registered in wazero's store by NewRuntime and resolve via store
		// fallback (when the resolver returns nil).
		instantiateCtx := experimental.WithImportResolver(execCtx, func(name string) api.Module {
			// Host WASI and teavm modules are always resolved from the store.
			if name == "wasi_snapshot_preview1" || name == "teavm" {
				return nil
			}
			// DAG-mapped imports take priority (handles "env" routing to
			// component instances and cross-module references).
			if srcIdx, ok := importNameToInstance[name]; ok {
				if m := resolveModuleForInstance(srcIdx); m != nil {
					return m
				}
			}
			// "env" fallback to host store (when no DAG mapping exists).
			if name == "env" {
				return nil
			}
			return nil
		})

		execStdout.Reset()
		execStderr.Reset()
		mod, err := e.rt.instantiateModuleNamedWithWriters(instantiateCtx, cm, fmt.Sprintf("__core_%d__", i), &execStdout, &execStderr)
		if err != nil {
			return "", nil, nil, nil, nil, fmt.Errorf("host: workflow %s: instantiate instance %d (module %d): %w", e.workflowID, i, inst.ModuleIndex, err)
		}
		resolvedInstances[i] = mod
		cleanupMods = append(cleanupMods, mod)
	}

	// ---- Step 4: Build resolved exports map per instance ----
	// resolvedExports[i] maps export name -> (actual export name, source module)
	// for each instance. This resolves FromExports chains.
	type resolvedExp struct {
		exportName string // actual export name on the source module
		mod        api.Module
	}
	resolvedExports := make([]map[string]resolvedExp, len(bundle.Instances))

	for i, inst := range bundle.Instances {
		resolvedExports[i] = make(map[string]resolvedExp)

		if inst.ModuleIndex >= 0 {
			// Collect all exports from the instantiated module.
			mod := resolvedInstances[i]
			if mod == nil {
				continue
			}

			// Function exports.
			for _, fd := range mod.ExportedFunctionDefinitions() {
				for _, en := range fd.ExportNames() {
					resolvedExports[i][en] = resolvedExp{exportName: en, mod: mod}
				}
			}

			// Memory exports.
			for memName := range mod.ExportedMemoryDefinitions() {
				resolvedExports[i][memName] = resolvedExp{exportName: memName, mod: mod}
			}
		}

		// Apply FromExports aliases (copies export references from source).
		for _, fe := range inst.FromExports {
			if fe.SourceInstance >= 0 && fe.SourceInstance < len(resolvedExports) {
				if exp, ok2 := resolvedExports[fe.SourceInstance][fe.SourceName]; ok2 {
					resolvedExports[i][fe.Name] = resolvedExp{
						exportName: exp.exportName,
						mod:        exp.mod,
					}
				}
			}
		}
	}

	// ---- Step 5: Find the entry point and initialize the module ----
	exp, ok := bundle.Exports[entryPoint]
	if !ok {
		return "", nil, nil, nil, nil, fmt.Errorf("host: component export %q not found", entryPoint)
	}

	// Resolve the entry point through the export chain (handles FromExports).
	var entryMod api.Module
	var entryExportName string

	if re, ok2 := resolvedExports[exp.InstanceIndex][exp.Name]; ok2 && re.mod != nil {
		entryMod = re.mod
		entryExportName = re.exportName
	} else if exp.InstanceIndex < len(resolvedInstances) && resolvedInstances[exp.InstanceIndex] != nil {
		// Fallback: try direct lookup on the module.
		entryMod = resolvedInstances[exp.InstanceIndex]
		entryExportName = exp.Name
		if fn := entryMod.ExportedFunction(entryExportName); fn == nil {
			return "", nil, nil, nil, nil, fmt.Errorf("host: export %q not found on instance %d", entryExportName, exp.InstanceIndex)
		}
	} else {
		return "", nil, nil, nil, nil, fmt.Errorf("host: cannot resolve component export %q (instance %d)", entryPoint, exp.InstanceIndex)
	}

	// Initialize the module (calls _start if present, e.g. for Go wasip1
	// runtime initialization; no-op for modules without _start).
	if err := e.rt.InitModule(execCtx, entryMod); err != nil {
		return "", nil, nil, nil, nil, fmt.Errorf("host: init component entry module: %w", err)
	}

	// ---- Step 6: Call the entry point ----
	result, err := e.rt.CallExport(execCtx, entryMod, entryExportName, input)
	if err != nil {
		if errors.Is(err, ErrSuspended) || session.suspendErr != nil {
			se := session.suspendErr
			if se == nil {
				se = &SuspendError{Reason: "workflow suspended"}
				if se.Until.IsZero() {
					se.Until = time.Now().Add(30 * time.Second)
				}
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
				// generation is 0 because the engine does not yet track generation
				// for continue-as-new; this code path is dormant (handler is never
				// wired in current deployments).
				newEvents := session.history
				priority := 0
				if e.state != nil {
					priority = e.state.Priority()
				}
				newRunID, cnErr := e.continueAsNewHandler(ctx, e.workflowID, e.workerID, int64(0), e.defName, e.defVersion, se.NewInput, newEvents, result, session.queryState, priority)
				if cnErr != nil {
					return "", session.history, nil, nil, nil, fmt.Errorf("host: workflow %s: continue_as_new handler failed: %w", e.workflowID, cnErr)
				}
				susResult.ContinueAsNewHandled = true
				susResult.NewRunID = newRunID
			}

			return "", session.history, susResult, session.deferrals, session.queryState, nil
		}
		// Workflow failed with non-suspend error.
		if len(session.deferrals) > 0 {
			e.invokeDefersOnTrap(ctx, entryMod, session.deferrals)
		}
		session.releaseHeldScopes(ctx)
		return "", session.history, nil, nil, nil, err
	}

	// Workflow completed successfully.
	session.releaseHeldScopes(ctx)
	return result, session.history, nil, session.deferrals, session.queryState, nil
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
	if backend := e.backendForWasm(wasmBytes); backend != nil {
		res, err := backend.PerExecution().Execute(ctx, wasmBytes, deferName, input, handlerFromContextOrNil(ctx))
		if err != nil {
			return "", err
		}
		if res == nil {
			return "", nil
		}
		return res.Result, nil
	}

	// No backend for this guest: the CGO-less build, where wazero is the only
	// runtime there is. Unfenced, and unavoidably so.
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
func (e *Engine) invokeDefersOnTrap(ctx context.Context, mod api.Module, deferrals map[string]string) {
	for deferID, description := range deferrals {
		exportName := "cleat_defer_" + deferID
		fn := mod.ExportedFunction(exportName)
		if fn == nil {
			e.log().WarnContext(ctx, "defer export not found", "defer_id", deferID, "description", description, "export_name", exportName)
			continue
		}
		_, _, err := e.rt.CallExportWithSuspend(ctx, mod, exportName, []byte("{}"))
		if err != nil {
			e.log().WarnContext(ctx, "defer execution failed", "defer_id", deferID, "description", description, "error", err)
		}
	}
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
