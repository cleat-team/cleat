package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
//
// MATCHED AGAINST THE RAW JSON BYTES (json.RawMessage), quotes included --
// NOT against a json.Unmarshal'd Go string. cleat#2336: ResolveSecretRefs
// (tenant_secrets.go) finds a reference by scanning the raw, undecoded JSON
// text for the literal substring "${secret:", because that is the only text
// it ever sees -- it runs before anything decodes the document. A value
// written with JSON escaping that DECODES to a reference, e.g. the string
// literal "\u0024{secret:name}" (a \u0024 escape for the $),
// used to pass this check (which decoded first, matched the decoded form)
// but could never be found by ResolveSecretRefs's raw scan -- it reached
// the plugin call with the placeholder text "${secret:name}" sitting in
// the field, unresolved and indistinguishable from a successfully-resolved
// secret that happened to contain that text. Matching the raw bytes makes
// this function reject exactly the inputs the resolver could never have
// resolved, instead of validating a promise the next stage cannot keep.
var secretOnlyFieldRef = regexp.MustCompile(`^"\$\{secret:[A-Za-z0-9_.-]{1,128}\}"$`)

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
// top-level key occurrence that case-fold-matches the declared name.
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
// This walks inputJSON's top-level keys with a json.Decoder's own token
// stream rather than unmarshaling into a map[string]json.RawMessage. That
// used to be the whole check, and it had a hole a case-fold check alone does
// not: encoding/json's map decode silently keeps only the LAST value for an
// EXACT duplicate key, so {"api_key":"LIT","api_key":"${secret:x}"} collapsed
// to a one-entry map before this function ever ran, and the surviving
// (well-formed) value was the only thing checked -- the literal was still in
// the raw inputJSON that went on to be recorded. Reported in review of
// cleat#2043 (cleat#2329, commit 7a94d970), verified on all three dialects.
// Reading tokens one pair at a time sees every occurrence, spelled identically
// or not, before anything collapses them.
//
// inputJSON that isn't a JSON object at all, or that carries trailing data
// after the closing brace, is ALSO a violation whenever secretOnlyFields is
// non-empty: a function that opted into this check gets no free pass for
// malformed input reaching its own decode unchecked.
func checkSecretOnlyFields(secretOnlyFields []string, inputJSON string) string {
	if len(secretOnlyFields) == 0 {
		return ""
	}
	const malformed = "input is not a JSON object, and this function declares secret-only fields"

	dec := json.NewDecoder(strings.NewReader(inputJSON))
	tok, err := dec.Token()
	if err != nil {
		return malformed
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return malformed
	}

	// occurrences[declared] holds every top-level key spelling that
	// case-folds to that declared field, in encounter order; rawValues holds
	// the matching JSON value for each occurrence, same order, same index.
	occurrences := map[string][]string{}
	rawValues := map[string][]json.RawMessage{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return malformed
		}
		key, ok := keyTok.(string)
		if !ok {
			return malformed
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return malformed
		}
		for _, declared := range secretOnlyFields {
			if strings.EqualFold(key, declared) {
				occurrences[declared] = append(occurrences[declared], key)
				rawValues[declared] = append(rawValues[declared], raw)
			}
		}
	}
	if _, err := dec.Token(); err != nil { // the closing '}'
		return malformed
	}
	if _, err := dec.Token(); err != io.EOF { // trailing data after the object
		return malformed
	}

	for _, declared := range secretOnlyFields {
		ks := occurrences[declared]
		if len(ks) == 0 {
			continue
		}
		if len(ks) > 1 {
			sorted := append([]string(nil), ks...)
			sort.Strings(sorted)
			return fmt.Sprintf("field %q is declared secret-only and matched more than one top-level "+
				"key occurrence (%s); refusing the ambiguity rather than guessing which one a decoder would use",
				declared, strings.Join(sorted, ", "))
		}
		if !secretOnlyFieldRef.Match(rawValues[declared][0]) {
			return fmt.Sprintf("field %q is declared secret-only and must be exactly a "+
				"${secret:NAME} reference, written with no JSON escaping inside it -- a literal "+
				"value, a non-string value, or an escaped reference ResolveSecretRefs could never "+
				"have found is refused", declared)
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
// comment. If inputJSON doesn't parse as a JSON object (or carries trailing
// data after one), there is nothing to redact field-by-field, so the whole
// payload becomes the marker.
//
// Like checkSecretOnlyFields, this walks inputJSON's top-level keys with a
// json.Decoder's token stream instead of unmarshaling into a
// map[string]json.RawMessage and re-marshaling it -- a map collapses an exact
// duplicate key to its last value before this function would ever see the
// other one, which would silently drop a redaction target rather than merely
// lose a display fidelity. Rebuilding the object key-by-key redacts every
// occurrence, duplicate spellings included, and never collapses anything.
func redactSecretOnlyFields(secretOnlyFields []string, inputJSON string) string {
	if len(secretOnlyFields) == 0 {
		return inputJSON
	}
	redactedVal, err := json.Marshal(secretOnlyFieldRedactionMarker)
	if err != nil {
		return secretOnlyFieldRedactionMarker
	}

	dec := json.NewDecoder(strings.NewReader(inputJSON))
	tok, err := dec.Token()
	if err != nil {
		return secretOnlyFieldRedactionMarker
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return secretOnlyFieldRedactionMarker
	}

	var b strings.Builder
	b.WriteByte('{')
	changed := false
	first := true
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return secretOnlyFieldRedactionMarker
		}
		key, ok := keyTok.(string)
		if !ok {
			return secretOnlyFieldRedactionMarker
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return secretOnlyFieldRedactionMarker
		}
		keyJSON, err := json.Marshal(key)
		if err != nil {
			return secretOnlyFieldRedactionMarker
		}
		value := raw
		for _, declared := range secretOnlyFields {
			if strings.EqualFold(key, declared) {
				value = redactedVal
				changed = true
				break
			}
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.Write(keyJSON)
		b.WriteByte(':')
		b.Write(value)
	}
	if _, err := dec.Token(); err != nil { // the closing '}'
		return secretOnlyFieldRedactionMarker
	}
	if _, err := dec.Token(); err != io.EOF { // trailing data after the object
		return secretOnlyFieldRedactionMarker
	}
	b.WriteByte('}')

	if !changed {
		return inputJSON
	}
	return b.String()
}

// pluginCallRecordsNothingNoRegistry names the one path on which plugin_call answers WITHOUT recording its event: the worker has no plugin registry. Every other
// refusal (an unregistered function, a call-guard rejection, a secret-only violation) IS recorded, because a hole at that step would misalign a later replay.
const pluginCallRecordsNothingNoRegistry = "the worker has no plugin registry, which no amount of retrying changes: nothing is recorded and the guest is told so"

// pluginCallRecordsNothingReinvoked names the other: a replayed call whose function may be re-invoked runs it again, live, and its event is already in history.
const pluginCallRecordsNothingReinvoked = "a replayed call whose function may be re-invoked on replay runs it again: its event is already in history, so nothing is recorded"

// pluginCallStep is plugin_call as a dispatcher step (cleat#4152, G3): one plugin_call event, in the durable-call result layout. The refusals a stop or a
// suspension asks for, the replay-to-fresh hand-off at the frontier, the divergence report and the consumption of the record are the dispatcher's. What stays in
// Decode is the call's SECOND replay behaviour: a record whose function may be re-invoked on replay (cleat#1318) is answered by running the function live, which
// records nothing, so it is not a Fresh. What stays in Fresh is the invocation and its record.
var pluginCallStep = stepSpec{
	Name:    "plugin_call",
	Effect:  effectEffect,
	Records: recordsOne,
	Want:    EventTypePluginCall,
	// The guest decodes result<string, call-failure>: a divergence is answered in the durable-call layout, as every durable call's is.
	Result: layoutDurableCall,
	Identity: func(a stepArgs, rec *EventRecord) string {
		if rec.PluginName != a.s1 || rec.PluginFunc != a.s2 {
			return fmt.Sprintf("plugin %s/%s, recorded %s/%s", a.s1, a.s2, rec.PluginName, rec.PluginFunc)
		}
		return ""
	},
	RecordsNothingOn: []string{pluginCallRecordsNothingNoRegistry, pluginCallRecordsNothingReinvoked},
	Reinvoke:         func(c *stepCtx, rec *EventRecord) bool { return c.s.mayReinvokePluginCall(c.args.s1, c.args.s2, rec) },
	// What the old divergence messages printed: the live input (a.s4 is it with the function's secret-only fields redacted, computed by PluginCall only when
	// the session is replaying, the only time a divergence can arise), and what the record cached.
	DivergenceDetail: func(a stepArgs, rec *EventRecord) string {
		return fmt.Sprintf("\n  actual input: %s\n  expected (cached) input: %s\n  expected (cached) output: %s",
			redactedBody(a.s4), redactedBody(rec.PluginInput), redactedBody(rec.PluginOutput))
	},
	Decode: func(c *stepCtx, rec *EventRecord) int64 { return c.s.answerRecordedPluginCall(c, rec) },
	Fresh:  func(c *stepCtx) int64 { return c.s.freshPluginCall(c) },
}

func init() { registerStepSpec(&pluginCallStep) }

func (s *execSession) PluginCall(ctx context.Context, m api.Module,
	pluginName, functionName, inputJSON string,
	responsePtr, responseMaxLen uint32) int64 {
	args := stepArgs{s1: pluginName, s2: functionName, s3: inputJSON, outPtr: responsePtr, outMaxLen: responseMaxLen}
	if s.isReplay {
		// Only a replay can report a divergence, and a divergence names the live input by its size and a hash (redactedBody, cleat#4321), taken over the input with the
		// function's secret-only fields redacted (see redactedLiveInput), so that it compares with the recorded input, which was recorded redacted the same way.
		// s3 stays the input as the guest passed it: a call that reaches the frontier runs fresh with it.
		args.s4 = s.redactedLiveInput(pluginName, functionName, inputJSON)
	}
	return s.step(ctx, m, &pluginCallStep, args)
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

// mayReinvokePluginCall says whether a replayed plugin_call runs its function AGAIN, live, instead of serving the recorded answer (cleat#1318). It is the spec's
// Reinvoke: a decision from the record and the registry, and no live work. The call itself runs in Fresh, behind the dispatcher's refusals.
func (s *execSession) mayReinvokePluginCall(pluginName, functionName string, rec *EventRecord) bool {
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
	//
	// A function whose CURRENT registration declares secret-only fields
	// (cleat#2043) never re-invokes on replay, full stop -- regardless of
	// what rec itself says. RegisterWithPolicy already refuses to combine
	// SecretOnlyFields with MayReInvokeOnReplay()==true for a NEW
	// registration, but rec.Idempotent below reads the OLD record's own
	// flags without consulting the registry at all, so history written
	// under a permissive pre-#2043 registration -- one that genuinely
	// succeeded with a literal, because the check did not exist yet --
	// would still take this branch today and call freshPluginCallWithHistory,
	// which runs checkSecretOnlyFields against the CURRENT declaration and
	// refuses it live: a run that finished successfully turns into a
	// replay failure. Reported in review of cleat#2043 (cleat#2329): the
	// correct behaviour for a declared function is to fall through and
	// serve rec's own recorded output/error below, exactly as if neither
	// branch applied -- never re-invoke, and never flip a done run to
	// failing either. Look the registry up once, ahead of both branches,
	// so it decides before rec's flags are consulted, not after.
	var secretOnlyFields []string
	var regPolicy ReplayPolicy
	var regOK bool
	if s.engine.pluginRegistry != nil {
		_, regPolicy, secretOnlyFields, regOK = s.engine.pluginRegistry.Lookup(pluginName, functionName)
	}
	if len(secretOnlyFields) != 0 {
		return false
	}
	if rec.Idempotent {
		policy := ReplayPolicy{Idempotent: true, SameValueOnReplay: rec.SameValueOnReplay}
		if policy.MayReInvokeOnReplay() {
			return true
		}
	}
	return regOK && regPolicy.MayReInvokeOnReplay()
}

// answerRecordedPluginCall answers a plugin_call from its own record, which the dispatcher has already consumed: the recorded output or error. It touches nothing
// outside the session (a call whose function may be re-invoked never gets here: its Reinvoke said so and the step went to Fresh).
func (s *execSession) answerRecordedPluginCall(c *stepCtx, rec *EventRecord) int64 {
	ctx, m := c.ctx, c.m
	responsePtr, responseMaxLen := c.args.outPtr, c.args.outMaxLen

	if rec.PluginError != "" {
		// Must match the fresh path below: the class was never persisted,
		// so the two have to use the same constant or the same step
		// changes retryability on replay.
		written, _ := s.writeResult(ctx, m, responsePtr, rec.PluginError, responseMaxLen)
		return packDurableCallResult(int(written), callFailureCode, 1)
	}

	written, writtenEC := s.writeOut(ctx, m, responsePtr, rec.PluginOutput, responseMaxLen)
	return packDurableCallResult(int(written), writeErrorClass(writtenEC), writtenEC)
}

// freshPluginCall is plugin_call's Fresh: invoke the function and record the event. The stop and suspension refusals are the dispatcher's, for both ways in
// (a plain fresh call and a replay that reached the frontier), where they used to be repeated here for exactly that reason (cleat#4143).
func (s *execSession) freshPluginCall(c *stepCtx) int64 {
	ctx, m := c.ctx, c.m
	pluginName, functionName, inputJSON := c.args.s1, c.args.s2, c.args.s3
	responsePtr, responseMaxLen := c.args.outPtr, c.args.outMaxLen

	inv := s.invokePlugin(ctx, pluginName, functionName, inputJSON)
	if inv.noRegistry != "" {
		c.RecordsNothing(pluginCallRecordsNothingNoRegistry)
		return s.writePluginResult(ctx, m, inv, responsePtr, responseMaxLen)
	}
	if c.Reinvoking() {
		// A replayed call whose function may be re-invoked: the event is already in history and already consumed.
		c.RecordsNothing(pluginCallRecordsNothingReinvoked)
		return s.writePluginResult(ctx, m, inv, responsePtr, responseMaxLen)
	}

	// Record in event history BEFORE checking for errors, so that all
	// plugin calls are captured (even failed lookups). This ensures
	// replay determinism — the history must include every call attempt.
	rec := EventRecord{
		Step:              s.stepCount,
		EventType:         EventTypePluginCall,
		PluginName:        pluginName,
		PluginFunc:        functionName,
		PluginInput:       inv.recordedInput,
		PluginOutput:      inv.output,
		PluginError:       inv.errStr,
		Idempotent:        inv.policy.Idempotent,
		SameValueOnReplay: inv.policy.SameValueOnReplay,
	}
	c.Record(rec)

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
	return s.writePluginResult(ctx, m, inv, responsePtr, responseMaxLen)
}

// pluginInvocation is what running a plugin function (or refusing to) produced, before anything is recorded or written.
type pluginInvocation struct {
	// noRegistry is the message when the worker has no plugin registry: nothing is recorded, and the answer is not retryable.
	noRegistry string
	output     string
	errStr     string
	// failed is whether the function (or a refusal) produced an error, which is not the same as errStr being non-empty: an error may have an empty message.
	failed bool
	// recordedInput is what goes into event_history, which differs from the input the function was called with on a failed call for a function that declares
	// secret-only fields.
	recordedInput string
	policy        ReplayPolicy
}

// invokePlugin looks the function up, applies the call guard and the secret-only field check, and runs it. It records nothing: the caller decides whether the
// event is new (freshPluginCall) or already in history (reinvokePluginCall).
func (s *execSession) invokePlugin(ctx context.Context, pluginName, functionName, inputJSON string) pluginInvocation {
	// Look up the plugin function.
	if s.engine.pluginRegistry == nil {
		msg := fmt.Sprintf("plugin function %s/%s not available: no plugin registry configured. Check that the plugin is deployed and its version satisfies the workflow's plugin_deps.", pluginName, functionName)
		// Not retryable, and nothing is recorded on this path: the worker has no plugin registry, which no amount of retrying changes.
		return pluginInvocation{noRegistry: msg}
	}
	fn, policy, secretOnlyFields, ok := s.engine.pluginRegistry.Lookup(pluginName, functionName)

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
			}
		}
		if fnErr == nil {
			// Actually call the plugin.
			step := s.stepCount
			callCtx := s.pluginCallContext(ctx, step)
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

	// recordedInput is what goes into event_history, distinct from inputJSON
	// (what fn is called with) on ANY failed call for a function that
	// declares secret-only fields -- not only a checkSecretOnlyFields
	// violation. A call-guard rejection (s.engine.pluginCallGuard.Check,
	// above) sets fnErr and short-circuits BEFORE checkSecretOnlyFields ever
	// runs, so inputJSON at that point has not been validated and may still
	// hold a literal; recording it unredacted defeats the whole point of
	// declaring the field. Reported in review of cleat#2043 (cleat#2329):
	// every recorded failure for a declared function is redacted, regardless
	// of which check produced it, and only a SUCCESS -- which is only
	// reachable once checkSecretOnlyFields has already passed -- records
	// inputJSON as-is.
	recordedInput := inputJSON
	if fnErr != nil {
		recordedInput = redactSecretOnlyFields(secretOnlyFields, inputJSON)
	}
	return pluginInvocation{output: outputJSON, errStr: errStr, failed: fnErr != nil, recordedInput: recordedInput, policy: policy}
}

// writePluginResult hands the guest what an invocation produced, in the durable-call layout.
func (s *execSession) writePluginResult(ctx context.Context, m api.Module, inv pluginInvocation, responsePtr, responseMaxLen uint32) int64 {
	if inv.noRegistry != "" {
		written, _ := s.writeResult(ctx, m, responsePtr, inv.noRegistry, responseMaxLen)
		return packDurableCallResult(int(written), callErrorUnknown, 1)
	}
	if inv.failed {
		written, _ := s.writeResult(ctx, m, responsePtr, inv.errStr, responseMaxLen)
		return packDurableCallResult(int(written), callFailureCode, 1)
	}
	written, writtenEC := s.writeOut(ctx, m, responsePtr, inv.output, responseMaxLen)
	return packDurableCallResult(int(written), writeErrorClass(writtenEC), writtenEC)
}

// pluginCallStreamingStep is plugin_call_streaming (cleat#4152, G3). A streaming call is a MULTI-RECORD step: one plugin_call_stream_chunk record per chunk,
// each at its own step, the last of them the finished chunk. A stream-level failure (no registry, an unregistered function, a call-guard refusal, a secret-only
// violation, a function that fails to start) is ONE synthetic finished chunk, marked as not plugin output, carrying the error code the guest was told.
//
// WHAT THE DISPATCHER ADDS, each a change to the old replay that cleat#4339's table pinned as it was:
//
//   - Identity is the plugin and the function, on EVERY record of the run. The old replay named neither, so a record for another function or plugin was served
//     as this call's answer; it is a divergence now (the table's two KNOWN_DEFECT rows).
//   - A run ends at its finished chunk, so two adjacent streams are two answers and not one, and the second no longer reaches the frontier and runs live again
//     (the table's adjacent-streams rows). EndsWithoutFinal keeps the old reading of a stream that never finished: prefix chunks followed by another call's record
//     are a complete stream with no final, answered from what was recorded.
//   - A first record that is not a chunk is a divergence that consumes nothing. The old replay answered SUCCESS with a `null` stream and did not advance.
//   - The stop and suspension refusals are the dispatcher's, on both ways into Fresh, so a stopping or suspended session refuses a replayed call's frontier as it
//     refuses any call, and the frontier hand-off is the dispatcher's too.
//
// A history that ENDS inside a stream (prefix chunks, then the end of history) is answered from those chunks and not re-run: that is what the old replay did for
// a stream that never finished, and it cannot tell that from a stream whose worker was lost mid-way, so a lost-mid-stream run is still served as the partial
// stream it recorded. (It now leaves replay on that call, through the dispatcher, where it stayed in replay before.)
//
// An EMPTY stream records a marker (IsEmptyStreamMarker, cleat#4342), so a replay can tell it from a call that is not there. A history written BEFORE the marker has
// nothing for an empty stream, and two adjacent streams of the SAME function in such a history, the first empty, are still read as one. And an UNFINISHED stream
// directly followed by an empty one cannot be told apart either: the marker ends the unfinished stream's run, so the first call is answered that stream's chunks and
// the empty call reaches the frontier and runs live (a pinned row).
var pluginCallStreamingStep = stepSpec{
	Name: "plugin_call_streaming",
	NoDivergenceDetail: "its old replay never reported a divergence: it read whatever chunk records were there and answered, so there was no message to carry detail; " +
		divergenceSweepNote,
	Effect:           effectEffect,
	Records:          recordsMany,
	Want:             EventTypePluginCallStreamChunk,
	Result:           layoutDurableCall,
	EndsWithoutFinal: true,
	Identity: func(a stepArgs, rec *EventRecord) string {
		if rec.PluginName != a.s1 || rec.PluginFunc != a.s2 {
			return fmt.Sprintf("plugin stream function %s/%s, recorded %s/%s", a.s1, a.s2, rec.PluginName, rec.PluginFunc)
		}
		return ""
	},
	Role: func(_ stepArgs, rec *EventRecord) recordRole {
		switch {
		case rec.EventType != EventTypePluginCallStreamChunk:
			return roleForeign
		case IsEmptyStreamMarker(*rec), rec.StreamFinish:
			// The marker is a whole call by itself: an empty stream, so the run ends at it.
			return roleFinal
		}
		return rolePrefix
	},
	DecodeRun: func(c *stepCtx, run []EventRecord) int64 { return c.s.decodePluginCallStreamingRun(c, run) },
	Fresh:     func(c *stepCtx) int64 { return c.s.freshPluginCallStreaming(c) },
}

func init() { registerStepSpec(&pluginCallStreamingStep) }

// PluginCallStreaming is plugin_call_streaming.
func (s *execSession) PluginCallStreaming(ctx context.Context, m api.Module,
	pluginName, functionName, inputJSON string,
	responsePtr, responseMaxLen uint32) int64 {
	return s.step(ctx, m, &pluginCallStreamingStep, stepArgs{s1: pluginName, s2: functionName, s3: inputJSON, outPtr: responsePtr, outMaxLen: responseMaxLen})
}

// emptyStreamMarkerCode is the error code the marker below carries. Any non-zero value works (the marker is told by the combination, not by the value); this is the
// call-failure code only so that the record reads as odd to a human who looks at it.
const emptyStreamMarkerCode = callFailureCode

// IsEmptyStreamMarker says that a stream chunk record is the marker a streaming call writes when its stream produced NO chunk at all (cleat#4342).
//
// Without it an empty stream recorded nothing, so a replay could not tell the call from one that was not there, and [empty A][stream B] of the same function replayed
// B's chunks to A and ran B live a second time. The marker is an EXISTING record shape, not a new event type: a plugin_call_stream_chunk with StreamData true and a
// NON-ZERO StreamErrCode. No other writer produces that combination: a chunk the plugin produced sets StreamData and never an error code (freshPluginCallStreaming), and
// the synthetic failure chunk sets an error code and never StreamData (streamErrorRecord). A history written before 0.6.0 has no StreamData at all, so it cannot hold one.
// Readers of these records that serve them to a client (the SSE stream) must skip it, as it is not a chunk the plugin produced.
func IsEmptyStreamMarker(rec EventRecord) bool {
	return rec.EventType == EventTypePluginCallStreamChunk && rec.StreamData && rec.StreamErrCode != 0
}

// emptyStreamMarkerRecord is the record of a streaming call whose stream produced no chunk. It is not finished (StreamFinish false) on purpose: a worker that still reads
// the record as a plain chunk (0.5.x, which does not know the data flag) serves it as a one-element stream and not as a FAILURE, which a lone finished chunk would be.
func emptyStreamMarkerRecord(step int, pluginName, functionName, inputJSON string) EventRecord {
	return EventRecord{
		Step:          step,
		EventType:     EventTypePluginCallStreamChunk,
		PluginName:    pluginName,
		PluginFunc:    functionName,
		PluginInput:   inputJSON,
		StreamData:    true,
		StreamErrCode: int(emptyStreamMarkerCode),
	}
}

// streamErrorRecord is the synthetic stream chunk that represents a stream-level error (e.g. registry not found, call guard rejection), so that a replay can
// reproduce the same error result.
//
// code is the call error code the guest is about to be told, stored so replay can report the same one instead of deriving it a second time.
func streamErrorRecord(step int, pluginName, functionName, inputJSON, errMsg string, code byte) EventRecord {
	return EventRecord{
		Step:             step,
		EventType:        EventTypePluginCallStreamChunk,
		PluginName:       pluginName,
		PluginFunc:       functionName,
		PluginInput:      inputJSON,
		PluginOutput:     errMsg,
		StreamChunkIndex: 0,
		StreamFinish:     true,
		StreamErrCode:    int(code),
	}
}

// freshPluginCallStreaming is the spec's Fresh. The refusals (a stopping session, a defer segment, a session that has already suspended) are the dispatcher's,
// applied before this runs on both ways in, including the replay frontier (cleat#4143).
//
// A history that ended INSIDE a stream arrives with c.Prefix, the chunks it recorded; they are the answer (see the spec's comment), and nothing runs.
func (s *execSession) freshPluginCallStreaming(c *stepCtx) int64 {
	ctx := c.ctx
	pluginName, functionName, inputJSON := c.args.s1, c.args.s2, c.args.s3
	if len(c.Prefix) > 0 {
		return s.decodePluginCallStreamingRun(c, c.Prefix)
	}

	// fail records a stream-level failure and returns the packed result the guest sees, deriving both from the ONE `code` argument, and records the input it is given,
	// which is inputJSON as called or its redacted form.
	//
	// One closure for both halves on purpose. The failure this prevents is the one recordedFailureCode's comment describes for the non-streaming path: a fresh run and
	// the replay of it classifying the same step differently. Several call sites each writing a record and then packing a constant is several chances for those to drift
	// apart, and the drift would not show up as a broken test -- it would show up as a workflow that retried on the first run and gave up on the replay. It is a closure
	// in the Fresh function, and not a method, so that every Record call stays inside what the dispatcher has already refused.
	fail := func(recordedInput, errMsg string, code byte) int64 {
		c.Record(streamErrorRecord(s.stepCount, pluginName, functionName, recordedInput, errMsg, code))
		written, _ := s.writeResult(ctx, c.m, c.args.outPtr, errMsg, c.args.outMaxLen)
		return packDurableCallResult(int(written), code, 1)
	}

	// Look up the streaming plugin function.
	if s.engine.pluginStreamRegistry == nil {
		errMsg := "plugin_call_streaming: no plugin stream registry configured"
		// callErrorUnknown, matching the non-streaming path's own answer for the same condition: a worker with no registry is not a service that might succeed next
		// time.
		return fail(inputJSON, errMsg, callErrorUnknown)
	}

	fn, secretOnlyFields, ok := s.engine.pluginStreamRegistry.Lookup(pluginName, functionName)
	if !ok {
		errMsg := fmt.Sprintf("plugin stream function %s/%s not registered. Check that the plugin is deployed and its version satisfies the workflow's plugin_deps.", pluginName, functionName)
		// callFailureCode, which is what the non-streaming path reports for an unregistered function: the same deployment gap was once retryable to a workflow calling
		// the plugin and non-retryable to one streaming from it (IMPROVEMENT-PLAN 2.35).
		return fail(inputJSON, errMsg, callFailureCode)
	}

	// Check plugin call guard for streaming calls too.
	//
	// Redact with the ALREADY-LOOKED-UP secretOnlyFields before recording, not raw inputJSON: this rejection short-circuits BEFORE checkSecretOnlyFields runs below,
	// so inputJSON has not been validated yet and may still hold a literal in a declared field. Recording it unredacted here was reported in review of cleat#2043
	// (cleat#2329): redact on every recorded failure for a function that declares secret-only fields, not only on the secret-only check's own failure.
	if s.engine.pluginCallGuard != nil && s.callerPluginName != "" {
		if err := s.engine.pluginCallGuard.Check(s.callerPluginName, pluginName); err != nil {
			// callFailureCode, as above: the guard rejection becomes fnErr on the non-streaming path and packs callFailureCode there.
			return fail(redactSecretOnlyFields(secretOnlyFields, inputJSON), err.Error(), callFailureCode)
		}
	}

	// Secret-only field check (cleat#2043) -- see the non-streaming path's identical check for the reasoning.
	if violation := checkSecretOnlyFields(secretOnlyFields, inputJSON); violation != "" {
		errMsg := fmt.Sprintf("plugin stream function %s/%s: %s", pluginName, functionName, violation)
		return fail(redactSecretOnlyFields(secretOnlyFields, inputJSON), errMsg, callFailureCode)
	}

	callCtx := s.pluginCallContext(ctx, s.stepCount)

	// Call the streaming plugin function and collect chunks.
	chunkCh, err := fn(callCtx, inputJSON)
	if err != nil {
		errMsg := fmt.Sprintf("plugin_call_streaming %s/%s: %v", pluginName, functionName, err)
		// The stream function itself failed: the direct analogue of a non-streaming plugin call returning an error, which packs callFailureCode. inputJSON has already
		// passed checkSecretOnlyFields at this point, but redact anyway, for the same belt-and-braces reason the non-streaming path does on its fn()-error path.
		return fail(redactSecretOnlyFields(secretOnlyFields, inputJSON), errMsg, callFailureCode)
	}

	var collected []plugin.StreamEvent
	index := 0
	// Drain the channel on exit to prevent goroutine leak when the context is cancelled mid-stream. The producer blocks on send until the receiver reads; draining
	// ensures it can exit.
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
				StreamData:       true, // plugin output, never the synthetic failure chunk (cleat#4117)
			}
			persistence := c.Record(rec)

			// A CHUNK THE DATABASE REFUSED IS NOT SHOWN TO ANYBODY. cleat#1572.
			//
			// recordEvent returns nil-and-silent on a failed write -- the commonest cause being ErrFenceLost, where another worker has taken this run, so nothing this
			// worker records is going to be in the run's history. Publishing anyway would put tokens on a user's screen that the system does not believe happened, which
			// is the precise failure the preview decision on this issue set out to prevent. It is also the case a reader cannot recover from: there is no row to
			// reconnect to.
			//
			// eventNotAttempted is DIFFERENT and is published. No database, or --no-per-step-flush, is a configuration rather than a refusal; the chunk is carried
			// with Durable false so a reader that cares can tell, rather than being silently dropped.
			if persistence == eventFlushFailed {
				index++
				continue
			}

			// AFTER the record, never before -- so a reader that sees a chunk live can always find it in event_history afterwards, and never the other way round.
			// EVERY FIELD COMES OFF rec, not off the locals it was built from: the whole contract with a reader is that a chunk seen live can be found again in
			// event_history under the SAME (step, index).
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
	if c.recorded == 0 {
		// A stream that produced no chunk records the marker (cleat#4342), so that a replay can tell this call from one that is not there. The guest is answered null, as it
		// always was. Nothing is published to a live reader: nothing was produced.
		c.Record(emptyStreamMarkerRecord(s.stepCount, pluginName, functionName, inputJSON))
	}
	// Return collected chunks as JSON.
	outJSON, err := json.Marshal(collected)
	if err != nil {
		errMsg := fmt.Sprintf("plugin_call_streaming %s/%s: marshal chunks: %v", pluginName, functionName, err)
		written, _ := s.writeResult(ctx, c.m, c.args.outPtr, errMsg, c.args.outMaxLen)
		return packDurableCallResult(int(written), callErrorUnknown, 1)
	}

	written, writtenEC := s.writeOut(ctx, c.m, c.args.outPtr, string(outJSON), c.args.outMaxLen)
	return packDurableCallResult(int(written), writeErrorClass(writtenEC), writtenEC)
}

// decodePluginCallStreamingRun answers a replayed streaming call from the run of chunk records the dispatcher read (or, for a history that ended inside the
// stream, from the chunks it had recorded). It touches nothing in the world.
func (s *execSession) decodePluginCallStreamingRun(c *stepCtx, run []EventRecord) int64 {
	ctx, m := c.ctx, c.m
	pluginName, functionName := c.args.s1, c.args.s2
	responsePtr, responseMaxLen := c.args.outPtr, c.args.outMaxLen
	var collected []plugin.StreamEvent
	index := 0
	for _, rec := range run {
		if IsEmptyStreamMarker(rec) {
			continue // an empty stream's marker is the call's whole record and no chunk: the answer is null
		}
		chunk := plugin.StreamEvent{Index: rec.StreamChunkIndex, Content: rec.PluginOutput, Finish: rec.StreamFinish}
		if !(rec.StreamChunkIndex > 0 || (rec.StreamChunkIndex == 0 && rec.StreamFinish)) {
			chunk.Index = index
		}
		collected = append(collected, chunk)
		index++
	}
	last := run[len(run)-1]

	// A single finished chunk with no real chunk content is a stream-level error recorded by streamErrorRecord. Return it with error status to match what Fresh
	// produced on the error path.
	//
	// The code comes off the record rather than from a constant here, which is what lets fresh and replay agree while the fresh classification changes underneath
	// them. StreamErrCode is absent on every chunk written before IMPROVEMENT-PLAN 2.35 and so reads back as 0 == callErrorUnknown, which is exactly what those
	// failures reported when they were fresh: determinism is per recorded step, not across eras.
	//
	// BUT only when the chunk is not marked as plugin output. A stream function that sends exactly one final chunk records the same row a failure does, and reading
	// it back as a failure turned a successful stream into an error on every resume, with the chunk's own text as the message (cleat#4117). StreamData is what
	// tells them apart; a record without it replays as it always did.
	if len(collected) == 1 && collected[0].Finish && !last.StreamData {
		written, _ := s.writeResult(ctx, m, responsePtr, collected[0].Content, responseMaxLen)
		return packDurableCallResult(int(written), byte(last.StreamErrCode), 1)
	}

	// Return collected chunks as JSON.
	outJSON, err := json.Marshal(collected)
	if err != nil {
		errMsg := fmt.Sprintf("plugin_call_streaming %s/%s: marshal chunks: %v", pluginName, functionName, err)
		written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
		return packDurableCallResult(int(written), callErrorUnknown, 1)
	}

	written, writtenEC := s.writeOut(ctx, m, responsePtr, string(outJSON), responseMaxLen)
	return packDurableCallResult(int(written), writeErrorClass(writtenEC), writtenEC)
}
