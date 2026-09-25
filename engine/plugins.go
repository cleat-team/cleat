package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tetratelabs/wazero/api"

	"github.com/cleat-team/cleat/internal/telemetry"
	"github.com/cleat-team/cleat/plugin"
)

// ReplayPolicy is what a registration says about re-invoking a plugin function
// during replay. cleat#1318 split it from a single Idempotent bool, because
// "is re-running safe?" and "should this run at all?" are different questions
// and one boolean answered both.
//
// MayReInvokeOnReplay is the conjunction, and it is a method rather than a
// third field so the two inputs cannot drift out of agreement with the
// decision they feed.
type ReplayPolicy struct {
	// Idempotent: calling again has no additional effect.
	Idempotent bool
	// SameValueOnReplay: calling again returns what the original call returned.
	SameValueOnReplay bool
}

// MayReInvokeOnReplay reports whether replay may discard the recorded output
// and call the function live.
//
// BOTH are required. Idempotent alone permits a repeat that returns a
// different answer -- which is the thing replay exists to prevent.
// SameValueOnReplay alone permits repeating a side effect.
func (p ReplayPolicy) MayReInvokeOnReplay() bool {
	return p.Idempotent && p.SameValueOnReplay
}

// pluginFuncEntry stores a registered plugin function along with its replay
// policy and any declared secret-only fields (cleat#2043).
type pluginFuncEntry struct {
	fn               plugin.PluginFunc
	policy           ReplayPolicy
	secretOnlyFields []string
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
	return pr.RegisterWithPolicy(pluginName, funcName, fn, ReplayPolicy{}, nil)
}

// RegisterIdempotent registers a plugin function whose repeat invocation has no
// additional effect.
//
// SINCE cleat#1318 THIS NO LONGER LICENSES RE-INVOCATION ON REPLAY, and the
// change is deliberate rather than incidental. Idempotence says a repeat is
// harmless; it does not say the repeat returns what the first call returned,
// and only that second property justifies discarding recorded output. A
// function that needs both must say so via RegisterWithPolicy.
//
// Kept because it is exported and because "idempotent" remains a true and
// useful thing to record. The function is wrapped with panic recovery.
func (pr *PluginRegistry) RegisterIdempotent(pluginName, funcName string, fn plugin.PluginFunc) error {
	return pr.RegisterWithPolicy(pluginName, funcName, fn, ReplayPolicy{Idempotent: true}, nil)
}

// RegisterWithPolicy registers a plugin function with an explicit replay
// policy and, optionally, declared secret-only fields (cleat#2043). The
// function is wrapped with panic recovery.
//
// secretOnlyFields and a policy where MayReInvokeOnReplay() is true are
// refused together. Without the exclusion, a call the secret-only check
// refuses would stay refused forever once recorded -- including on replay of
// history recorded before the field was declared, where the same call may
// have genuinely succeeded with a literal. Re-invoking it live during replay
// would flip a run that finished "done" into one that fails on replay: a
// determinism break in the opposite direction from the leak this field
// closes. See plugin.FuncOptions.SecretOnlyFields's doc comment.
func (pr *PluginRegistry) RegisterWithPolicy(pluginName, funcName string, fn plugin.PluginFunc, policy ReplayPolicy, secretOnlyFields []string) error {
	key := lookupKey(pluginName, funcName)
	if len(secretOnlyFields) > 0 && policy.MayReInvokeOnReplay() {
		return fmt.Errorf("plugin function %q: SecretOnlyFields cannot be combined with a replay "+
			"policy that may re-invoke on replay (Idempotent && SameValueOnReplay) -- cleat#2043", key)
	}
	if _, exists := pr.funcs[key]; exists {
		return fmt.Errorf("plugin function %q already registered", key)
	}
	wrapped := plugin.RecoverPluginFunc(pluginName, pr.healthTracker, fn)
	pr.funcs[key] = pluginFuncEntry{fn: wrapped, policy: policy, secretOnlyFields: secretOnlyFields}
	return nil
}

// Has reports whether a plugin function is registered.
func (pr *PluginRegistry) Has(pluginName, funcName string) bool {
	_, ok := pr.funcs[lookupKey(pluginName, funcName)]
	return ok
}

// Lookup returns the function, its replay policy, and its declared
// secret-only fields (cleat#2043; nil if none were declared).
//
// The middle value is a ReplayPolicy rather than a bool so that a caller has to
// say WHICH property it means. It used to be `idempotent`, and the whole of
// cleat#1318 is what that ambiguity cost.
func (pr *PluginRegistry) Lookup(pluginName, funcName string) (plugin.PluginFunc, ReplayPolicy, []string, bool) {
	entry, ok := pr.funcs[lookupKey(pluginName, funcName)]
	return entry.fn, entry.policy, entry.secretOnlyFields, ok
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

// pluginStreamFuncEntry stores a registered streaming plugin function along
// with its declared secret-only fields (cleat#2043). Streaming has no
// re-invoke-on-replay mechanism -- replay always serves recorded chunks, never
// calls a streaming function live -- so there is no ReplayPolicy here and no
// exclusivity check to make against one.
type pluginStreamFuncEntry struct {
	fn               plugin.PluginStreamFunc
	secretOnlyFields []string
}

// PluginStreamRegistry maps plugin function names to streaming implementations.
type PluginStreamRegistry struct {
	funcs         map[string]pluginStreamFuncEntry
	healthTracker *plugin.PluginHealthTracker
}

func NewPluginStreamRegistry() *PluginStreamRegistry {
	return &PluginStreamRegistry{
		funcs:         make(map[string]pluginStreamFuncEntry),
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
	return psr.registerWithSecretOnlyFields(pluginName, funcName, fn, nil)
}

func (psr *PluginStreamRegistry) registerWithSecretOnlyFields(pluginName, funcName string, fn plugin.PluginStreamFunc, secretOnlyFields []string) error {
	key := lookupKey(pluginName, funcName)
	if _, exists := psr.funcs[key]; exists {
		return fmt.Errorf("plugin stream function %q already registered", key)
	}
	wrapped := plugin.RecoverPluginStreamFunc(pluginName, psr.healthTracker, fn)
	psr.funcs[key] = pluginStreamFuncEntry{fn: wrapped, secretOnlyFields: secretOnlyFields}
	return nil
}

// Lookup returns the function and its declared secret-only fields (cleat#2043;
// nil if none were declared).
func (psr *PluginStreamRegistry) Lookup(pluginName, funcName string) (plugin.PluginStreamFunc, []string, bool) {
	entry, ok := psr.funcs[lookupKey(pluginName, funcName)]
	return entry.fn, entry.secretOnlyFields, ok
}

// Has reports whether a streaming plugin function is registered.
func (psr *PluginStreamRegistry) Has(pluginName, funcName string) bool {
	_, ok := psr.funcs[lookupKey(pluginName, funcName)]
	return ok
}

// RegisterStream implements plugin.StreamFuncRegistry.
func (psr *PluginStreamRegistry) RegisterStream(pluginName string, opts plugin.FuncOptions, fn plugin.PluginStreamFunc) error {
	return psr.registerWithSecretOnlyFields(pluginName, opts.Name, fn, opts.SecretOnlyFields)
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

// secretOnlyFieldRef is the anchored form of secretRef (tenant_secrets.go):
// a secret-only field's raw value must be EXACTLY one reference, not merely
// contain one somewhere inside a larger string. Same character class, so a
// name this rejects is also a name ResolveSecretRefs would never have
// resolved.
var secretOnlyFieldRef = regexp.MustCompile(`^\$\{secret:[A-Za-z0-9_.-]{1,128}\}$`)

// secretOnlyFieldRedactionMarker replaces a declared secret-only field's
// value -- literal or reference alike -- wherever a call is refused and still
// has to be recorded (cleat#2043). It is applied to every declared field on a
// refused call, not just the one that violated, because once a call is being
// refused there is no reason for anything near a secret-only field to reach
// storage from it.
const secretOnlyFieldRedactionMarker = "[secret-only field, literal value refused]"

// checkSecretOnlyFields reports the first violation found in inputJSON
// against secretOnlyFields (top-level JSON field names, plugin.FuncOptions'
// declaration), or "" if there is none.
//
// A declared field absent from inputJSON is fine -- it falls back to
// whatever the plugin does when the field is unset. A field present exactly
// once, as a JSON string matching secretOnlyFieldRef, is fine. Everything
// else is a violation: a literal value, a non-string value, or more than one
// top-level key that case-fold-matches the declared name.
//
// The fold-match is deliberate, not a nicety: encoding/json's own struct
// decode matches field names case-insensitively as a fallback (including a
// few non-ASCII folds, e.g. the Kelvin sign folding to 'k'), so
// {"api_key":"${secret:x}","API_KEY":"sk-literal"} would pass an exact-key
// check here while still reaching the plugin's own decode as the literal.
// Rather than replicate encoding/json's exact/fold precedence to decide which
// one "would have won", any input with more than one fold-matching key for a
// declared field is refused outright -- the ambiguity itself is the problem.
//
// inputJSON that isn't a JSON object at all is ALSO a violation whenever
// secretOnlyFields is non-empty: a function that opted into this check gets
// no free pass for malformed input reaching its own decode unchecked.
func checkSecretOnlyFields(secretOnlyFields []string, inputJSON string) string {
	if len(secretOnlyFields) == 0 {
		return ""
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(inputJSON), &top); err != nil {
		return "input is not a JSON object, and this function declares secret-only fields"
	}
	for _, declared := range secretOnlyFields {
		var matches []string
		for k := range top {
			if strings.EqualFold(k, declared) {
				matches = append(matches, k)
			}
		}
		if len(matches) == 0 {
			continue
		}
		if len(matches) > 1 {
			sort.Strings(matches)
			return fmt.Sprintf("field %q is declared secret-only and matched more than one top-level "+
				"key (%s); refusing the ambiguity rather than guessing which one a decoder would use",
				declared, strings.Join(matches, ", "))
		}
		var val string
		if err := json.Unmarshal(top[matches[0]], &val); err != nil || !secretOnlyFieldRef.MatchString(val) {
			return fmt.Sprintf("field %q is declared secret-only and must be exactly a "+
				"${secret:NAME} reference; a literal value is refused", declared)
		}
	}
	return ""
}

// redactSecretOnlyFields returns inputJSON with every declared top-level
// secret-only field's value replaced by secretOnlyFieldRedactionMarker, for
// recording to event_history or embedding in an error message in place of a
// refused call's raw input.
//
// Called for every declared field on a refused call, whether or not that
// particular field was the one that violated -- see the marker's own doc
// comment. If inputJSON doesn't parse as a JSON object, there is nothing to
// redact field-by-field, so the whole payload becomes the marker.
func redactSecretOnlyFields(secretOnlyFields []string, inputJSON string) string {
	if len(secretOnlyFields) == 0 {
		return inputJSON
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(inputJSON), &top); err != nil {
		return secretOnlyFieldRedactionMarker
	}
	redacted, err := json.Marshal(secretOnlyFieldRedactionMarker)
	if err != nil {
		return secretOnlyFieldRedactionMarker
	}
	changed := false
	for k := range top {
		for _, declared := range secretOnlyFields {
			if strings.EqualFold(k, declared) {
				top[k] = redacted
				changed = true
				break
			}
		}
	}
	if !changed {
		return inputJSON
	}
	out, err := json.Marshal(top)
	if err != nil {
		return secretOnlyFieldRedactionMarker
	}
	return string(out)
}

func (s *execSession) PluginCall(ctx context.Context, m api.Module,
	pluginName, functionName, inputJSON string,
	responsePtr, responseMaxLen uint32) int64 {
	if s.isReplay {
		return s.replayPluginCall(ctx, m, pluginName, functionName, inputJSON, responsePtr, responseMaxLen)
	}
	if s.stopBeforeNewWork() {
		return callSuspendSentinel
	}
	return s.freshPluginCall(ctx, m, pluginName, functionName, inputJSON, responsePtr, responseMaxLen)
}

// redactedLiveInput redacts pluginName/functionName's declared secret-only
// fields out of a LIVE (not-yet-recorded) inputJSON, for embedding in a
// replay-divergence message. `rec.PluginInput` in the same message needs no
// such treatment: it is either from a successful call (a secret-only field
// there, if any, already holds only a resolved reference's text, never a
// literal -- the same structural guarantee ResolveSecretRefs relies on) or
// from a refused call recorded by freshPluginCallInternal, which already
// redacted it at record time.
//
// A lookup miss (pluginName/functionName unregistered) just means nothing is
// redacted, which is correct: an unregistered function declared nothing.
func (s *execSession) redactedLiveInput(pluginName, functionName, inputJSON string) string {
	if s.engine.pluginRegistry == nil {
		return inputJSON
	}
	_, _, secretOnlyFields, ok := s.engine.pluginRegistry.Lookup(pluginName, functionName)
	if !ok {
		return inputJSON
	}
	return redactSecretOnlyFields(secretOnlyFields, inputJSON)
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
			if s.engine.Metrics != nil {
				s.engine.Metrics.RecordReplayFailure(ctx)
			}
			errMsg := fmt.Sprintf("replay divergence at step %d: expected plugin_call event, got %s.\n  actual input: %s\n  expected (cached) input: %s\n  expected (cached) output: %s\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
				rec.Step, rec.EventType,
				truncateWithHash(s.redactedLiveInput(pluginName, functionName, inputJSON), maxPayloadLen),
				truncateWithHash(rec.PluginInput, maxPayloadLen),
				truncateWithHash(rec.PluginOutput, maxPayloadLen))
			// Not retryable: a divergence is a bug in the workflow code.
			written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
			return packDurableCallResult(int(written), callErrorUnknown, 1)
		}

		if rec.PluginName != pluginName || rec.PluginFunc != functionName {
			if s.engine.Metrics != nil {
				s.engine.Metrics.RecordReplayFailure(ctx)
			}
			errMsg := fmt.Sprintf("replay divergence at step %d: workflow called %s/%s but history has %s/%s.\n  actual input: %s\n  expected (cached) input: %s\n  expected (cached) output: %s\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
				rec.Step, pluginName, functionName, rec.PluginName, rec.PluginFunc,
				truncateWithHash(s.redactedLiveInput(pluginName, functionName, inputJSON), maxPayloadLen),
				truncateWithHash(rec.PluginInput, maxPayloadLen),
				truncateWithHash(rec.PluginOutput, maxPayloadLen))
			// Not retryable: a divergence is a bug in the workflow code.
			written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
			return packDurableCallResult(int(written), callErrorUnknown, 1)
		}

		// THE REGISTRY DECIDES, NOT THE RECORD, and rec.Idempotent is kept only
		// for the in-process case. event_history has dedicated plugin_* columns
		// and no idempotent one, so a record loaded from the database always
		// reads false here however it was registered. Flipping a registration
		// therefore changes the replay semantics of runs recorded before the
		// change; cleat#1318 leaves that alone deliberately, there being no old
		// data yet.
		//
		// Re-invocation requires BOTH properties (cleat#1318). It used to
		// require only "idempotent", which seven functions claimed -- including
		// reads of state an operator can change between the original run and
		// the replay.
		if rec.Idempotent {
			policy := ReplayPolicy{Idempotent: true, SameValueOnReplay: rec.SameValueOnReplay}
			if policy.MayReInvokeOnReplay() {
				// Do NOT append to newEvents (the event is already in history).
				return s.freshPluginCallWithHistory(ctx, m, pluginName, functionName, inputJSON, responsePtr, responseMaxLen)
			}
		}

		if s.engine.pluginRegistry != nil {
			_, policy, _, ok := s.engine.pluginRegistry.Lookup(pluginName, functionName)
			if ok && policy.MayReInvokeOnReplay() {
				return s.freshPluginCallWithHistory(ctx, m, pluginName, functionName, inputJSON, responsePtr, responseMaxLen)
			}
		}

		if rec.PluginError != "" {
			// Must match the fresh path below: the class was never persisted,
			// so the two have to use the same constant or the same step
			// changes retryability on replay.
			written, _ := s.writeResult(ctx, m, responsePtr, rec.PluginError, responseMaxLen)
			return packDurableCallResult(int(written), callFailureCode, 1)
		}

		written, writtenEC := s.writeOut(ctx, m, responsePtr, rec.PluginOutput, responseMaxLen)
		return packDurableCallResult(int(written), truncClass(writtenEC), writtenEC)
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
		// Not retryable, and nothing is recorded on this path: the worker has
		// no plugin registry, which no amount of retrying changes.
		written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
		return packDurableCallResult(int(written), callErrorUnknown, 1)
	}
	fn, policy, secretOnlyFields, ok := s.engine.pluginRegistry.Lookup(pluginName, functionName)

	var outputJSON string
	var fnErr error
	// recordedInput is what goes into event_history, distinct from inputJSON
	// (what fn is called with) only on the secret-only-field violation path
	// below, where fn is never called at all.
	recordedInput := inputJSON
	if !ok {
		fnErr = fmt.Errorf("plugin function %s/%s not registered. Check that the plugin is deployed and its version satisfies the workflow's plugin_deps.", pluginName, functionName)
	} else {
		// Check plugin call guard (enforces call_plugin capability for WASM plugins).
		if s.engine.pluginCallGuard != nil && s.callerPluginName != "" {
			if err := s.engine.pluginCallGuard.Check(s.callerPluginName, pluginName); err != nil {
				fnErr = err
			}
		}
		// Secret-only field check (cleat#2043), before fn is ever invoked and
		// before anything is recorded: a declared field holding a literal, an
		// ambiguous case-variant duplicate, or malformed input on a function
		// that declared fields is refused here. fn does not run. The refusal
		// IS recorded below like any other failed call -- skipping the record
		// entirely would leave a hole at this step that a later recorded call
		// (should the guest catch this error and continue) would misalign
		// replay against; see checkSecretOnlyFields and
		// secretOnlyFieldRedactionMarker's doc comments.
		if fnErr == nil {
			if violation := checkSecretOnlyFields(secretOnlyFields, inputJSON); violation != "" {
				fnErr = fmt.Errorf("plugin function %s/%s: %s", pluginName, functionName, violation)
				recordedInput = redactSecretOnlyFields(secretOnlyFields, inputJSON)
			}
		}
		if fnErr == nil {
			callCtx := s.pluginCallContext(ctx)

			// Actually call the plugin.
			step := s.stepCount
			callCtx, eventSpan := telemetry.EventSpan(callCtx, step, "plugin_call", pluginName, functionName)
			t0 := time.Now()
			outputJSON, fnErr = fn(callCtx, inputJSON)
			if s.engine.Metrics != nil {
				s.engine.Metrics.RecordPluginCallDuration(ctx, time.Since(t0), pluginName, functionName)
			}
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
			Step:              s.stepCount,
			EventType:         EventTypePluginCall,
			PluginName:        pluginName,
			PluginFunc:        functionName,
			PluginInput:       recordedInput,
			PluginOutput:      outputJSON,
			PluginError:       errStr,
			Idempotent:        policy.Idempotent,
			SameValueOnReplay: policy.SameValueOnReplay,
		}
		s.recordEvent(rec)

		// Flush immediately so plugin results survive worker crashes.
		if s.engine.db != nil {
			if flushErr := s.engine.flushEvent(context.Background(), s.workflowID, rec, s.lastChecksum); flushErr != nil {
				if errors.Is(flushErr, ErrFenceLost) {
					s.engine.log().DebugContext(ctx, "PluginCall flushEvent: fence lost, workflow reassigned to another worker", "workflow_id", s.workflowID, "step", rec.Step)
				} else {
					s.engine.log().ErrorContext(ctx, "PluginCall flushEvent failed", "workflow_id", s.workflowID, "step", rec.Step, "error", flushErr)
				}
			}
		}
	}

	if fnErr != nil {
		written, _ := s.writeResult(ctx, m, responsePtr, errStr, responseMaxLen)
		return packDurableCallResult(int(written), callFailureCode, 1)
	}

	written, writtenEC := s.writeOut(ctx, m, responsePtr, outputJSON, responseMaxLen)
	return packDurableCallResult(int(written), truncClass(writtenEC), writtenEC)
}

func (s *execSession) PluginCallStreaming(ctx context.Context, m api.Module,
	pluginName, functionName, inputJSON string,
	responsePtr, responseMaxLen uint32) int64 {
	if s.isReplay {
		return s.replayPluginCallStreaming(ctx, m, pluginName, functionName, inputJSON, responsePtr, responseMaxLen)
	}
	if s.stopBeforeNewWork() {
		return callSuspendSentinel
	}
	return s.freshPluginCallStreaming(ctx, m, pluginName, functionName, inputJSON, responsePtr, responseMaxLen)
}

// recordStreamError records a synthetic stream chunk event representing a
// stream-level error (e.g. registry not found, call guard rejection). This
// ensures replayPluginCallStreaming can reproduce the same error result.
//
// code is the call error code the guest is about to be told, stored so replay
// can report the same one instead of deriving it a second time. Prefer
// streamFailure below, which records and returns together; this stays
// separate only because the tests that check what gets recorded call it
// directly.
func (s *execSession) recordStreamError(pluginName, functionName, inputJSON, errMsg string, code byte) {
	rec := EventRecord{
		Step:             s.stepCount,
		EventType:        EventTypePluginCallStreamChunk,
		PluginName:       pluginName,
		PluginFunc:       functionName,
		PluginInput:      inputJSON,
		PluginOutput:     errMsg,
		StreamChunkIndex: 0,
		StreamFinish:     true,
		StreamErrCode:    int(code),
	}
	s.recordEvent(rec)
}

// streamFailure records a stream-level failure and returns the packed result
// the guest sees, deriving both from one `code` argument.
//
// One function for both halves on purpose. The failure this prevents is the
// one recordedFailureCode's comment describes for the non-streaming path: a
// fresh run and the replay of it classifying the same step differently. Four
// call sites each writing a record and then packing a constant is four chances
// for those to drift apart, and the drift would not show up as a broken test
// -- it would show up as a workflow that retried on the first run and gave up
// on the replay.
func (s *execSession) streamFailure(ctx context.Context, m api.Module,
	pluginName, functionName, inputJSON, errMsg string, code byte,
	responsePtr, responseMaxLen uint32) int64 {

	s.recordStreamError(pluginName, functionName, inputJSON, errMsg, code)
	written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
	return packDurableCallResult(int(written), code, 1)
}

func (s *execSession) freshPluginCallStreaming(ctx context.Context, m api.Module,
	pluginName, functionName, inputJSON string,
	responsePtr, responseMaxLen uint32) int64 {

	// Look up the streaming plugin function.
	if s.engine.pluginStreamRegistry == nil {
		errMsg := "plugin_call_streaming: no plugin stream registry configured"
		// callErrorUnknown, matching the non-streaming path's own answer for
		// the same condition (freshPluginCallInternal): a worker with no
		// registry is not a service that might succeed next time.
		return s.streamFailure(ctx, m, pluginName, functionName, inputJSON, errMsg,
			callErrorUnknown, responsePtr, responseMaxLen)
	}

	fn, secretOnlyFields, ok := s.engine.pluginStreamRegistry.Lookup(pluginName, functionName)
	if !ok {
		errMsg := fmt.Sprintf("plugin stream function %s/%s not registered. Check that the plugin is deployed and its version satisfies the workflow's plugin_deps.", pluginName, functionName)
		// callFailureCode, which is what freshPluginCallInternal reports for an
		// unregistered function: the lookup failure becomes fnErr there and
		// fnErr packs callFailureCode. This site used to report
		// callErrorUnknown, so the same deployment gap was retryable to a
		// workflow calling the plugin and non-retryable to one streaming from
		// it. IMPROVEMENT-PLAN 2.35.
		return s.streamFailure(ctx, m, pluginName, functionName, inputJSON, errMsg,
			callFailureCode, responsePtr, responseMaxLen)
	}

	// Check plugin call guard for streaming calls too.
	if s.engine.pluginCallGuard != nil && s.callerPluginName != "" {
		if err := s.engine.pluginCallGuard.Check(s.callerPluginName, pluginName); err != nil {
			errMsg := err.Error()
			// callFailureCode, as above: the guard rejection becomes fnErr on
			// the non-streaming path and packs callFailureCode there.
			return s.streamFailure(ctx, m, pluginName, functionName, inputJSON, errMsg,
				callFailureCode, responsePtr, responseMaxLen)
		}
	}

	// Secret-only field check (cleat#2043) -- see freshPluginCallInternal's
	// identical check for the reasoning. streamFailure records via
	// recordStreamError, which takes inputJSON as a plain argument, so
	// passing the REDACTED string through it is enough: no new no-record path
	// needed here, unlike what an earlier draft of this fix assumed.
	if violation := checkSecretOnlyFields(secretOnlyFields, inputJSON); violation != "" {
		errMsg := fmt.Sprintf("plugin stream function %s/%s: %s", pluginName, functionName, violation)
		redactedInput := redactSecretOnlyFields(secretOnlyFields, inputJSON)
		return s.streamFailure(ctx, m, pluginName, functionName, redactedInput, errMsg,
			callFailureCode, responsePtr, responseMaxLen)
	}

	callCtx := s.pluginCallContext(ctx)

	// Call the streaming plugin function and collect chunks.
	chunkCh, err := fn(callCtx, inputJSON)
	if err != nil {
		errMsg := fmt.Sprintf("plugin_call_streaming %s/%s: %v", pluginName, functionName, err)
		// The stream function itself failed. This is the direct analogue of a
		// non-streaming plugin call returning an error, which packs
		// callFailureCode -- so the identical failure was retryable through
		// PluginCall and non-retryable through PluginCallStreaming.
		return s.streamFailure(ctx, m, pluginName, functionName, inputJSON, errMsg,
			callFailureCode, responsePtr, responseMaxLen)
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
			persistence := s.recordEvent(rec)

			// A CHUNK THE DATABASE REFUSED IS NOT SHOWN TO ANYBODY. cleat#1572.
			//
			// recordEvent returns nil-and-silent on a failed write -- the
			// commonest cause being ErrFenceLost, where another worker has
			// taken this run, so nothing this worker records is going to be in
			// the run's history. Publishing anyway would put tokens on a user's
			// screen that the system does not believe happened, which is the
			// precise failure the preview decision on this issue set out to
			// prevent. It is also the case a reader cannot recover from: there
			// is no row to reconnect to.
			//
			// eventNotAttempted is DIFFERENT and is published. No database, or
			// --no-per-step-flush, is a configuration rather than a refusal;
			// the chunk is carried with Durable false so a reader that cares
			// can tell, rather than being silently dropped.
			if persistence == eventFlushFailed {
				index++
				continue
			}

			// AFTER recordEvent, never before -- so a reader that sees a chunk
			// live can always find it in event_history afterwards, and never
			// the other way round.
			// EVERY FIELD COMES OFF rec, not off the locals it was built
			// from. They are equal today -- rec is assigned from them three
			// lines up -- and the point is that they stay equal: the whole
			// contract with a reader is that a chunk seen live can be found
			// again in event_history under the SAME (step, index). Reading
			// the published cursor from anywhere but the recorded row leaves
			// that agreement to be maintained by hand.
			s.engine.streamHub.Publish(s.workflowID, LiveChunk{
				Step:    rec.Step,
				Index:   rec.StreamChunkIndex,
				Content: rec.PluginOutput,
				Finish:  rec.StreamFinish,
				Durable: persistence == eventPersisted,
			})
			index++
		}
	}
done:
	// Return collected chunks as JSON.
	outJSON, err := json.Marshal(collected)
	if err != nil {
		errMsg := fmt.Sprintf("plugin_call_streaming %s/%s: marshal chunks: %v", pluginName, functionName, err)
		written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
		return packDurableCallResult(int(written), callErrorUnknown, 1)
	}

	written, writtenEC := s.writeOut(ctx, m, responsePtr, string(outJSON), responseMaxLen)
	return packDurableCallResult(int(written), truncClass(writtenEC), writtenEC)
}

func (s *execSession) replayPluginCallStreaming(ctx context.Context, m api.Module,
	pluginName, functionName, inputJSON string,
	responsePtr, responseMaxLen uint32) int64 {

	// Past recorded history -- switch to fresh execution.
	//
	// Without this the loop below reads nothing, `collected` stays nil, and
	// the function returns SUCCESS with a marshalled `null` for a stream the
	// plugin was never asked to produce. Every streaming call after a
	// workflow's first suspension took that path.
	//
	// replayPluginCall, in this file, is the shape this should have had: it
	// ends with exactly this pair of lines under exactly this comment. The
	// streaming twin was written without them.
	if s.stepCount >= len(s.history) {
		s.exitReplay()
		return s.freshPluginCallStreaming(ctx, m, pluginName, functionName, inputJSON, responsePtr, responseMaxLen)
	}

	var collected []plugin.StreamEvent
	index := 0
	// The code recorded alongside the chunk, used only in the single-finished-
	// chunk case below where there is exactly one record and so no ambiguity
	// about which one it came from.
	var recordedErrCode byte

	// Read consecutive stream chunk events from history.
	for s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		if rec.EventType != EventTypePluginCallStreamChunk {
			break
		}
		if !s.advanceReplayStep(ctx, &rec) {
			return 0
		}
		recordedErrCode = byte(rec.StreamErrCode)

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
	//
	// The code comes off the record rather than from a constant here, which is
	// what lets fresh and replay agree while the fresh classification changes
	// underneath them. This site used to report callErrorUnknown for all four
	// causes, because none of them was distinguishable once they arrived as
	// one synthetic chunk -- and three of the four should have matched the
	// non-streaming path's callFailureCode. IMPROVEMENT-PLAN 2.35.
	//
	// StreamErrCode is absent on every chunk written before that change and so
	// reads back as 0 == callErrorUnknown, which is exactly what those
	// failures reported when they were fresh: an event recorded then still
	// replays the way it always did, and only events recorded from now on
	// carry the corrected code. That is the property 2.35 asks for --
	// determinism is per recorded step, not across eras.
	if len(collected) == 1 && collected[0].Finish {
		written, _ := s.writeResult(ctx, m, responsePtr, collected[0].Content, responseMaxLen)
		return packDurableCallResult(int(written), recordedErrCode, 1)
	}

	// Return collected chunks as JSON.
	outJSON, err := json.Marshal(collected)
	if err != nil {
		errMsg := fmt.Sprintf("plugin_call_streaming %s/%s: marshal chunks: %v", pluginName, functionName, err)
		written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
		return packDurableCallResult(int(written), callErrorUnknown, 1)
	}

	written, writtenEC := s.writeOut(ctx, m, responsePtr, string(outJSON), responseMaxLen)
	return packDurableCallResult(int(written), truncClass(writtenEC), writtenEC)
}
