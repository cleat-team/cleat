package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/tetratelabs/wazero/api"

	"github.com/cleat-team/cleat/internal/telemetry"
	"github.com/cleat-team/cleat/plugin"
)


// pluginFuncEntry stores a registered plugin function along with its
// idempotent flag. Idempotent functions are safe to re-invoke during replay.
type pluginFuncEntry struct {
	fn         plugin.PluginFunc
	idempotent bool
}

// PluginRegistry maps plugin function names to implementations.
// It also tracks plugin health: if a plugin function panics, the
// entire plugin is marked unhealthy and all its functions return
// an error without being invoked.
type PluginRegistry struct {
	funcs         map[string]pluginFuncEntry
	healthTracker *plugin.PluginHealthTracker
}

func NewPluginRegistry() *PluginRegistry {
	return &PluginRegistry{
		funcs:         make(map[string]pluginFuncEntry),
		healthTracker: plugin.NewPluginHealthTracker(),
	}
}

// SetHealthTracker replaces the default health tracker with a shared one.
// Used to share a single tracker between PluginRegistry and
// PluginStreamRegistry so a panic in any function marks the plugin
// unhealthy across both registries.
func (pr *PluginRegistry) SetHealthTracker(t *plugin.PluginHealthTracker) {
	pr.healthTracker = t
}

// Register adds a plugin function. Returns an error if the function name
// is already registered for this plugin. The function is wrapped with
// panic recovery so a plugin crash does not take down the worker.
func (pr *PluginRegistry) Register(pluginName, funcName string, fn plugin.PluginFunc) error {
	key := lookupKey(pluginName, funcName)
	if _, exists := pr.funcs[key]; exists {
		return fmt.Errorf("plugin function %q already registered", key)
	}
	wrapped := plugin.RecoverPluginFunc(pluginName, pr.healthTracker, fn)
	pr.funcs[key] = pluginFuncEntry{fn: wrapped, idempotent: false}
	return nil
}

// RegisterIdempotent registers a plugin function that is safe to re-invoke
// during replay (e.g., read-only S3 GET operations). The function is wrapped
// with panic recovery.
func (pr *PluginRegistry) RegisterIdempotent(pluginName, funcName string, fn plugin.PluginFunc) error {
	key := lookupKey(pluginName, funcName)
	if _, exists := pr.funcs[key]; exists {
		return fmt.Errorf("plugin function %q already registered", key)
	}
	wrapped := plugin.RecoverPluginFunc(pluginName, pr.healthTracker, fn)
	pr.funcs[key] = pluginFuncEntry{fn: wrapped, idempotent: true}
	return nil
}

// Has reports whether a plugin function is registered.
func (pr *PluginRegistry) Has(pluginName, funcName string) bool {
	_, ok := pr.funcs[lookupKey(pluginName, funcName)]
	return ok
}

func (pr *PluginRegistry) Lookup(pluginName, funcName string) (plugin.PluginFunc, bool, bool) {
	entry, ok := pr.funcs[lookupKey(pluginName, funcName)]
	return entry.fn, entry.idempotent, ok
}

// IsPluginHealthy reports whether the given plugin has not panicked.
func (pr *PluginRegistry) IsPluginHealthy(pluginName string) bool {
	return pr.healthTracker.IsHealthy(pluginName)
}

// MarkPluginUnhealthy marks a plugin as unhealthy with the given error.
// All future invocations of the plugin's host functions are blocked.
func (pr *PluginRegistry) MarkPluginUnhealthy(pluginName string, err error) {
	pr.healthTracker.MarkUnhealthy(pluginName, err)
}

// PluginHealthStatus returns the current health status of all plugins
// that have been marked unhealthy. Healthy plugins are not included.
func (pr *PluginRegistry) PluginHealthStatus() []plugin.HealthStatus {
	return pr.healthTracker.UnhealthyStatus()
}

// UnhealthyError returns the error that caused the plugin to be marked
// unhealthy, or nil if the plugin is healthy.
func (pr *PluginRegistry) UnhealthyError(pluginName string) error {
	return pr.healthTracker.UnhealthyError(pluginName)
}

// PluginStreamRegistry maps plugin function names to streaming implementations.
type PluginStreamRegistry struct {
	funcs         map[string]plugin.PluginStreamFunc
	healthTracker *plugin.PluginHealthTracker
}

func NewPluginStreamRegistry() *PluginStreamRegistry {
	return &PluginStreamRegistry{
		funcs:         make(map[string]plugin.PluginStreamFunc),
		healthTracker: plugin.NewPluginHealthTracker(),
	}
}

// SetHealthTracker replaces the default health tracker with a shared one.
// Used to share a single tracker between PluginRegistry and
// PluginStreamRegistry.
func (psr *PluginStreamRegistry) SetHealthTracker(t *plugin.PluginHealthTracker) {
	psr.healthTracker = t
}

func (psr *PluginStreamRegistry) Register(pluginName, funcName string, fn plugin.PluginStreamFunc) error {
	key := lookupKey(pluginName, funcName)
	if _, exists := psr.funcs[key]; exists {
		return fmt.Errorf("plugin stream function %q already registered", key)
	}
	wrapped := plugin.RecoverPluginStreamFunc(pluginName, psr.healthTracker, fn)
	psr.funcs[key] = wrapped
	return nil
}

func (psr *PluginStreamRegistry) Lookup(pluginName, funcName string) (plugin.PluginStreamFunc, bool) {
	fn, ok := psr.funcs[lookupKey(pluginName, funcName)]
	return fn, ok
}

// Has reports whether a streaming plugin function is registered.
func (psr *PluginStreamRegistry) Has(pluginName, funcName string) bool {
	_, ok := psr.funcs[lookupKey(pluginName, funcName)]
	return ok
}

// RegisterStream implements plugin.StreamFuncRegistry.
func (psr *PluginStreamRegistry) RegisterStream(pluginName string, opts plugin.FuncOptions, fn plugin.PluginStreamFunc) error {
	return psr.Register(pluginName, opts.Name, fn)
}

// IsPluginHealthy reports whether the given streaming plugin has not panicked.
func (psr *PluginStreamRegistry) IsPluginHealthy(pluginName string) bool {
	return psr.healthTracker.IsHealthy(pluginName)
}

// MarkPluginUnhealthy marks a streaming plugin as unhealthy with the given error.
func (psr *PluginStreamRegistry) MarkPluginUnhealthy(pluginName string, err error) {
	psr.healthTracker.MarkUnhealthy(pluginName, err)
}

// PluginHealthStatus returns the current health status of all streaming plugins
// that have been marked unhealthy. Healthy plugins are not included.
func (psr *PluginStreamRegistry) PluginHealthStatus() []plugin.HealthStatus {
	return psr.healthTracker.UnhealthyStatus()
}

// UnhealthyError returns the error that caused the streaming plugin to be
// marked unhealthy, or nil if the plugin is healthy.
func (psr *PluginStreamRegistry) UnhealthyError(pluginName string) error {
	return psr.healthTracker.UnhealthyError(pluginName)
}


func (s *execSession) PluginCall(ctx context.Context, m api.Module,
	pluginName, functionName, inputJSON string,
	responsePtr, responseMaxLen uint32) int64 {
	if s.isReplay {
		return s.replayPluginCall(ctx, m, pluginName, functionName, inputJSON, responsePtr, responseMaxLen)
	}
	return s.freshPluginCall(ctx, m, pluginName, functionName, inputJSON, responsePtr, responseMaxLen)
}

func (s *execSession) replayPluginCall(ctx context.Context, m api.Module,
	pluginName, functionName, inputJSON string,
	responsePtr, responseMaxLen uint32) int64 {

	if s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		if !s.advanceReplayStep(ctx, &rec) {
			return 0
		}

		if rec.EventType != EventTypePluginCall {
			replayFailuresTotal.Inc()
			errMsg := fmt.Sprintf("replay divergence at step %d: expected plugin_call event, got %s.\n  actual input: %s\n  expected (cached) input: %s\n  expected (cached) output: %s\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
				rec.Step, rec.EventType,
				truncateWithHash(inputJSON, maxPayloadLen),
				truncateWithHash(rec.PluginInput, maxPayloadLen),
				truncateWithHash(rec.PluginOutput, maxPayloadLen))
			written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}

		if rec.PluginName != pluginName || rec.PluginFunc != functionName {
			replayFailuresTotal.Inc()
			errMsg := fmt.Sprintf("replay divergence at step %d: workflow called %s/%s but history has %s/%s.\n  actual input: %s\n  expected (cached) input: %s\n  expected (cached) output: %s\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
				rec.Step, pluginName, functionName, rec.PluginName, rec.PluginFunc,
				truncateWithHash(inputJSON, maxPayloadLen),
				truncateWithHash(rec.PluginInput, maxPayloadLen),
				truncateWithHash(rec.PluginOutput, maxPayloadLen))
			written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}

		if rec.Idempotent {
			// Safe to re-invoke during replay -- read-only operation (S3 GET).
			// Look up the function and call it, returning fresh output.
			// Do NOT append to newEvents (the event is already in history).
			return s.freshPluginCallWithHistory(ctx, m, pluginName, functionName, inputJSON, responsePtr, responseMaxLen)
		}

		// Idempotent flag may not be persisted in DB (no event_history column).
		// Fall back to registry lookup: if the function is currently registered
		// as idempotent, re-invoke instead of returning cached output.
		if s.engine.pluginRegistry != nil {
			_, idempotent, ok := s.engine.pluginRegistry.Lookup(pluginName, functionName)
			if ok && idempotent {
				return s.freshPluginCallWithHistory(ctx, m, pluginName, functionName, inputJSON, responsePtr, responseMaxLen)
			}
		}

		if rec.PluginError != "" {
			written, _ := s.writeResult(ctx, m, responsePtr, rec.PluginError, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}

		written, _ := s.writeResult(ctx, m, responsePtr, rec.PluginOutput, responseMaxLen)
		return packDurableCallResult(int(written), 0, 0)
	}

	// Past recorded history -- switch to fresh execution.
	s.exitReplay()
	return s.freshPluginCall(ctx, m, pluginName, functionName, inputJSON, responsePtr, responseMaxLen)
}

func (s *execSession) freshPluginCall(ctx context.Context, m api.Module,
	pluginName, functionName, inputJSON string,
	responsePtr, responseMaxLen uint32) int64 {
	return s.freshPluginCallInternal(ctx, m, pluginName, functionName, inputJSON, responsePtr, responseMaxLen, true)
}

// freshPluginCallWithHistory is like freshPluginCall but does not record the
// event in history or advance the step counter. Used for replay re-invocation
// of idempotent functions where the event is already in history.

func (s *execSession) freshPluginCallWithHistory(ctx context.Context, m api.Module,
	pluginName, functionName, inputJSON string,
	responsePtr, responseMaxLen uint32) int64 {
	return s.freshPluginCallInternal(ctx, m, pluginName, functionName, inputJSON, responsePtr, responseMaxLen, false)
}

func (s *execSession) freshPluginCallInternal(ctx context.Context, m api.Module,
	pluginName, functionName, inputJSON string,
	responsePtr, responseMaxLen uint32, recordEvent bool) int64 {

	// Look up the plugin function.
	if s.engine.pluginRegistry == nil {
		errMsg := fmt.Sprintf("plugin function %s/%s not available: no plugin registry configured. Check that the plugin is deployed and its version satisfies the workflow's plugin_deps.", pluginName, functionName)
		written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
		return packDurableCallResult(int(written), 1, 1)
	}
	fn, idempotent, ok := s.engine.pluginRegistry.Lookup(pluginName, functionName)

	var outputJSON string
	var fnErr error
	if !ok {
		fnErr = fmt.Errorf("plugin function %s/%s not registered. Check that the plugin is deployed and its version satisfies the workflow's plugin_deps.", pluginName, functionName)
	} else {
		// Check plugin call guard (enforces call_plugin capability for WASM plugins).
		if s.engine.pluginCallGuard != nil && s.callerPluginName != "" {
			if err := s.engine.pluginCallGuard.Check(s.callerPluginName, pluginName); err != nil {
				fnErr = err
			}
		}
		if fnErr == nil {
			// Inject call context (tenant ID + workflow ID) for plugin functions.
			callCtx := ctx
			cc := &plugin.CallContext{}
			if s.tenantID != "" {
				cc.TenantID = s.tenantID
			}
			if s.workflowID != "" {
				cc.WorkflowID = s.workflowID
			}
			if s.engine.db != nil {
				cc.DB = s.engine.db
			}
			callCtx = plugin.WithCallContext(callCtx, cc)

			// Actually call the plugin.
			step := s.stepCount
			callCtx, eventSpan := telemetry.EventSpan(callCtx, step, "plugin_call", pluginName, functionName)
			t0 := time.Now()
			outputJSON, fnErr = fn(callCtx, inputJSON)
			pluginCallDuration.WithLabelValues(pluginName, functionName).Observe(time.Since(t0).Seconds())
			eventSpan.End()
		}
	}

	var errStr string
	if fnErr != nil {
		errStr = fnErr.Error()
	}

	// Record in event history BEFORE checking for errors, so that all
	// plugin calls are captured (even failed lookups). This ensures
	// replay determinism — the history must include every call attempt.
	if recordEvent {
		rec := EventRecord{
			Step:         s.stepCount,
			EventType:    EventTypePluginCall,
			PluginName:   pluginName,
			PluginFunc:   functionName,
			PluginInput:  inputJSON,
			PluginOutput: outputJSON,
			PluginError:  errStr,
			Idempotent:   idempotent,
		}
		s.recordEvent(rec)

		// Flush immediately so plugin results survive worker crashes.
		if s.engine.db != nil {
			if flushErr := s.engine.flushEvent(context.Background(), s.workflowID, rec); flushErr != nil {
				s.engine.log().ErrorContext(ctx, "PluginCall flushEvent failed", "workflow_id", s.workflowID, "step", rec.Step, "error", flushErr)
			}
		}
	}

	if fnErr != nil {
		written, _ := s.writeResult(ctx, m, responsePtr, errStr, responseMaxLen)
		return packDurableCallResult(int(written), 1, 1)
	}

	written, _ := s.writeResult(ctx, m, responsePtr, outputJSON, responseMaxLen)
	return packDurableCallResult(int(written), 0, 0)
}

func (s *execSession) PluginCallStreaming(ctx context.Context, m api.Module,
	pluginName, functionName, inputJSON string,
	responsePtr, responseMaxLen uint32) int64 {
	if s.isReplay {
		return s.replayPluginCallStreaming(ctx, m, pluginName, functionName, inputJSON, responsePtr, responseMaxLen)
	}
	return s.freshPluginCallStreaming(ctx, m, pluginName, functionName, inputJSON, responsePtr, responseMaxLen)
}

// recordStreamError records a synthetic stream chunk event representing a
// stream-level error (e.g. registry not found, call guard rejection). This
// ensures replayPluginCallStreaming can reproduce the same error result.

func (s *execSession) recordStreamError(pluginName, functionName, inputJSON, errMsg string) {
	rec := EventRecord{
		Step:             s.stepCount,
		EventType:        EventTypePluginCallStreamChunk,
		PluginName:       pluginName,
		PluginFunc:       functionName,
		PluginInput:      inputJSON,
		PluginOutput:     errMsg,
		StreamChunkIndex: 0,
		StreamFinish:     true,
	}
	s.recordEvent(rec)
}

func (s *execSession) freshPluginCallStreaming(ctx context.Context, m api.Module,
	pluginName, functionName, inputJSON string,
	responsePtr, responseMaxLen uint32) int64 {

	// Look up the streaming plugin function.
	if s.engine.pluginStreamRegistry == nil {
		errMsg := "plugin_call_streaming: no plugin stream registry configured"
		s.recordStreamError(pluginName, functionName, inputJSON, errMsg)
		written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
		return packDurableCallResult(int(written), 0, 1)
	}

	fn, ok := s.engine.pluginStreamRegistry.Lookup(pluginName, functionName)
	if !ok {
		errMsg := fmt.Sprintf("plugin stream function %s/%s not registered. Check that the plugin is deployed and its version satisfies the workflow's plugin_deps.", pluginName, functionName)
		s.recordStreamError(pluginName, functionName, inputJSON, errMsg)
		written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
		return packDurableCallResult(int(written), 0, 1)
	}

	// Check plugin call guard for streaming calls too.
	if s.engine.pluginCallGuard != nil && s.callerPluginName != "" {
		if err := s.engine.pluginCallGuard.Check(s.callerPluginName, pluginName); err != nil {
			errMsg := err.Error()
			s.recordStreamError(pluginName, functionName, inputJSON, errMsg)
			written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
			return packDurableCallResult(int(written), 0, 1)
		}
	}

	// Inject call context.
	callCtx := ctx
	cc := &plugin.CallContext{}
	if s.tenantID != "" {
		cc.TenantID = s.tenantID
	}
	if s.workflowID != "" {
		cc.WorkflowID = s.workflowID
	}
	if s.engine.db != nil {
		cc.DB = s.engine.db
	}
	callCtx = plugin.WithCallContext(callCtx, cc)

	// Call the streaming plugin function and collect chunks.
	chunkCh, err := fn(callCtx, inputJSON)
	if err != nil {
		errMsg := fmt.Sprintf("plugin_call_streaming %s/%s: %v", pluginName, functionName, err)
		s.recordStreamError(pluginName, functionName, inputJSON, errMsg)
		written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
		return packDurableCallResult(int(written), 0, 1)
	}

	var collected []plugin.StreamEvent
	index := 0
	// Drain the channel on exit to prevent goroutine leak when the context
	// is cancelled mid-stream. The producer blocks on send until the receiver
	// reads; draining ensures it can exit.
	defer func() {
		for range chunkCh {
		}
	}()
	for {
		select {
		case <-callCtx.Done():
			// Context cancelled — return partial results.
			goto done
		case chunk, ok := <-chunkCh:
			if !ok {
				goto done
			}
			collected = append(collected, chunk)

			// Record each chunk as an event.
			rec := EventRecord{
				Step:             s.stepCount,
				EventType:        EventTypePluginCallStreamChunk,
				PluginName:       pluginName,
				PluginFunc:       functionName,
				PluginInput:      inputJSON,
				PluginOutput:     chunk.Content,
				StreamChunkIndex: index,
				StreamFinish:     chunk.Finish,
			}
			s.recordEvent(rec)
			index++
		}
	}
done:
	// Return collected chunks as JSON.
	outJSON, err := json.Marshal(collected)
	if err != nil {
		errMsg := fmt.Sprintf("plugin_call_streaming %s/%s: marshal chunks: %v", pluginName, functionName, err)
		written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
		return packDurableCallResult(int(written), 0, 1)
	}

	written, _ := s.writeResult(ctx, m, responsePtr, string(outJSON), responseMaxLen)
	return packDurableCallResult(int(written), 0, 0)
}

func (s *execSession) replayPluginCallStreaming(ctx context.Context, m api.Module,
	pluginName, functionName, inputJSON string,
	responsePtr, responseMaxLen uint32) int64 {

	var collected []plugin.StreamEvent
	index := 0

	// Read consecutive stream chunk events from history.
	for s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		if rec.EventType != EventTypePluginCallStreamChunk {
			break
		}
		if !s.advanceReplayStep(ctx, &rec) {
			return 0
		}

		chunk := plugin.StreamEvent{
			Index:   rec.StreamChunkIndex,
			Content: rec.PluginOutput,
			Finish:  rec.StreamFinish,
		}
		if rec.StreamChunkIndex > 0 || (rec.StreamChunkIndex == 0 && rec.StreamFinish) {
			chunk.Index = rec.StreamChunkIndex
		} else {
			chunk.Index = index
		}
		collected = append(collected, chunk)
		index++
	}

	// A single finished chunk with no real chunk content is a stream-level
	// error recorded by recordStreamError. Return it with error status to
	// match what freshPluginCallStreaming produced on the error path.
	if len(collected) == 1 && collected[0].Finish {
		written, _ := s.writeResult(ctx, m, responsePtr, collected[0].Content, responseMaxLen)
		return packDurableCallResult(int(written), 0, 1)
	}

	// Return collected chunks as JSON.
	outJSON, err := json.Marshal(collected)
	if err != nil {
		errMsg := fmt.Sprintf("plugin_call_streaming %s/%s: marshal chunks: %v", pluginName, functionName, err)
		written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
		return packDurableCallResult(int(written), 0, 1)
	}

	written, _ := s.writeResult(ctx, m, responsePtr, string(outJSON), responseMaxLen)
	return packDurableCallResult(int(written), 0, 0)
}
