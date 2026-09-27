package plugin

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"time"
)

// PanicError is returned when a plugin host function panics.
// It captures the panic value and the full goroutine stack trace
// so operators can diagnose the root cause.
//
// Long-term, plugins should be compiled to WASM modules for true
// process-level isolation. See design docs at docs/wasm-migration.md.
type PanicError struct {
	Plugin string `json:"plugin"`
	Value  any    `json:"value"`
	Stack  string `json:"stack"`
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("plugin %q panicked: %v", e.Plugin, e.Value)
}

// Unwrap returns nil — PanicError is a terminal error, not a wrapper.
func (e *PanicError) Unwrap() error { return nil }

// PluginHealthTracker tracks the runtime health of Go-compiled plugins.
// When a plugin's host function panics, it is marked unhealthy and all
// subsequent invocations are blocked without calling into the plugin.
//
// Migration note: WASM-compiled plugins provide process-level isolation
// and do not need this tracker because a WASM crash cannot take down
// the worker. The tracker exists for Go-compiled plugins only.
type PluginHealthTracker struct {
	mu        sync.RWMutex
	unhealthy map[string]*pluginFailure

	// cooldown is how long a panicked plugin is refused before one call is
	// allowed through to see whether it still panics.
	//
	// NOT PERMANENT, AND THAT IS THE WHOLE DESIGN. This tracker's own comment
	// said future invocations "will return this error without executing", and
	// nothing enforced it -- RecoverPluginFunc called the function regardless
	// and IsHealthy had no caller outside its own tests. Wiring the check in
	// as written would have made the promise true and created a worse problem:
	//
	//   - the commonest panic is INPUT-TRIGGERED, so one malformed request
	//     would disable the plugin for every tenant;
	//   - nothing calls MarkHealthy, so there is no way back;
	//   - PluginHealthStatus is not surfaced over HTTP, so there is no way to
	//     see it either.
	//
	// Permanent plus unrecoverable plus invisible is a denial of service a
	// caller can trigger on purpose. A cooldown keeps the protection that
	// matters -- a plugin whose state is genuinely broken is not hammered --
	// while an input-specific panic costs one window rather than an outage.
	cooldown time.Duration
}

// pluginFailure is why a plugin was refused and when, so the cooldown has
// something to measure from.
type pluginFailure struct {
	err error
	at  time.Time
}

// DefaultPluginCooldown is how long a panicked plugin is refused by default.
//
// Long enough that a plugin panicking on every call is called seldom rather
// than continuously, short enough that an input-specific panic does not read
// as an outage to the tenants that sent well-formed input.
const DefaultPluginCooldown = 30 * time.Second

// NewPluginHealthTracker creates a new PluginHealthTracker.
func NewPluginHealthTracker() *PluginHealthTracker {
	return NewPluginHealthTrackerWithCooldown(DefaultPluginCooldown)
}

// NewPluginHealthTrackerWithCooldown is NewPluginHealthTracker with an explicit
// cooldown. Tests use it to avoid sleeping; production has no reason to.
func NewPluginHealthTrackerWithCooldown(cooldown time.Duration) *PluginHealthTracker {
	return &PluginHealthTracker{
		unhealthy: make(map[string]*pluginFailure),
		cooldown:  cooldown,
	}
}

// MarkHealthy clears any previous unhealthy status for the given plugin.
func (t *PluginHealthTracker) MarkHealthy(pluginName string) {
	t.mu.Lock()
	delete(t.unhealthy, pluginName)
	t.mu.Unlock()
}

// MarkUnhealthy marks a plugin as unhealthy with the given fatal error.
// Once marked, all future invocations of the plugin's host functions
// will return this error without executing the function.
func (t *PluginHealthTracker) MarkUnhealthy(pluginName string, err error) {
	t.mu.Lock()
	t.unhealthy[pluginName] = &pluginFailure{err: err, at: time.Now()}
	t.mu.Unlock()
}

// refusalFor reports the error a call should be refused with, or nil to let it
// through. An entry whose cooldown has elapsed is CLEARED and the call allowed:
// that call is the probe, and if it panics again RecoverPluginFunc marks the
// plugin afresh.
//
// Clearing rather than keeping a half-open flag is deliberate -- the state a
// reader has to hold is then just "refused until this instant", and a probe
// that succeeds needs no separate transition to become healthy again.
func (t *PluginHealthTracker) refusalFor(pluginName string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	f, ok := t.unhealthy[pluginName]
	if !ok {
		return nil
	}
	if time.Since(f.at) >= t.cooldown {
		delete(t.unhealthy, pluginName)
		return nil
	}
	return f.err
}

// IsHealthy reports whether the plugin is healthy.
func (t *PluginHealthTracker) IsHealthy(pluginName string) bool {
	t.mu.RLock()
	f, ok := t.unhealthy[pluginName]
	t.mu.RUnlock()
	if !ok {
		return true
	}
	// A plugin past its cooldown reports healthy: the next call will be let
	// through, so saying otherwise would describe a refusal that is not going
	// to happen. This is a read-only view and does not clear the entry --
	// refusalFor does that, on the call path, under a write lock.
	return time.Since(f.at) >= t.cooldown
}

// UnhealthyError returns the error that caused the plugin to be marked
// unhealthy, or nil if the plugin is healthy.
func (t *PluginHealthTracker) UnhealthyError(pluginName string) error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	f, ok := t.unhealthy[pluginName]
	if !ok {
		return nil
	}
	return f.err
}

// HealthStatus describes the runtime health of a single plugin.
type HealthStatus struct {
	Name    string `json:"name"`
	Healthy bool   `json:"healthy"`
	Error   string `json:"error,omitempty"`
}

// UnhealthyStatus returns the current health status of all plugins that have
// been marked unhealthy. Healthy plugins are not included because the tracker
// only records failures — it does not maintain a registry of all plugin names.
func (t *PluginHealthTracker) UnhealthyStatus() []HealthStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()

	statuses := make([]HealthStatus, 0, len(t.unhealthy))
	for name, f := range t.unhealthy {
		s := HealthStatus{Name: name, Healthy: time.Since(f.at) >= t.cooldown}
		if f.err != nil {
			s.Error = f.err.Error()
		}
		statuses = append(statuses, s)
	}
	return statuses
}

// RecoverPluginFunc wraps a PluginFunc with panic recovery AND the refusal that
// recovery is for.
//
// If the wrapped function panics, the panic is caught, the plugin is marked
// unhealthy, and a PanicError is returned with the stack logged. Calls during
// the cooldown that follows are refused WITHOUT reaching the plugin.
//
// THE REFUSAL IS THE PART THAT WAS MISSING. This function's doc comment, and
// the tracker's, both said future invocations would "return this error without
// executing" -- and the closure ended `return fn(ctx, inputJSON)`
// unconditionally. IsHealthy and UnhealthyError had no callers outside their
// own tests. A plugin that panicked on every call was re-entered on every call,
// forever, with a stack trace logged each time.
//
// Checked HERE rather than at the call site in engine/plugins.go, because this
// wrapper is what owns the promise: every path to a plugin function goes
// through it, and a check at one call site is a check the next call site can
// forget.
func RecoverPluginFunc(pluginName string, tracker *PluginHealthTracker, fn PluginFunc) PluginFunc {
	return func(ctx context.Context, inputJSON string) (outputJSON string, err error) {
		if refusal := tracker.refusalFor(pluginName); refusal != nil {
			return "", refusal
		}
		defer func() {
			if r := recover(); r != nil {
				stack := string(debug.Stack())
				panicErr := &PanicError{
					Plugin: pluginName,
					Value:  r,
					Stack:  stack,
				}
				err = panicErr
				tracker.MarkUnhealthy(pluginName, panicErr)
				log.Printf("[plugin] %s panicked: %v\n%s", pluginName, r, stack)
			}
		}()
		return fn(ctx, inputJSON)
	}
}

// RecoverPluginStreamFunc wraps a PluginStreamFunc with panic recovery and the
// same refusal RecoverPluginFunc applies.
// If the wrapped function panics during setup (before returning the channel),
// the panic is caught and handled like RecoverPluginFunc. Panics during
// channel consumption are not caught here — the consumer must handle them.
func RecoverPluginStreamFunc(pluginName string, tracker *PluginHealthTracker, fn PluginStreamFunc) PluginStreamFunc {
	return func(ctx context.Context, inputJSON string) (ch <-chan StreamEvent, err error) {
		if refusal := tracker.refusalFor(pluginName); refusal != nil {
			return nil, refusal
		}
		defer func() {
			if r := recover(); r != nil {
				stack := string(debug.Stack())
				panicErr := &PanicError{
					Plugin: pluginName,
					Value:  r,
					Stack:  stack,
				}
				err = panicErr
				tracker.MarkUnhealthy(pluginName, panicErr)
				log.Printf("[plugin] %s panicked (stream setup): %v\n%s", pluginName, r, stack)
			}
		}()
		return fn(ctx, inputJSON)
	}
}

// RecoverGoroutine runs fn and converts a panic into a logged PanicError
// instead of a process exit. It is for goroutines a plugin STARTS, which the
// wrappers above cannot reach.
//
// # Why this exists separately from RecoverPluginStreamFunc
//
// RecoverPluginStreamFunc says so in its own doc comment: it catches a panic
// during stream SETUP, "panics during channel consumption are not caught here".
// A streaming provider returns its channel and then produces into it from a
// goroutine, so every SSE-scan and JSON-decode panic lands after the wrapper has
// already returned. Four such goroutines existed when this was written
// (plugins/llm/host_functions.go and the openai, anthropic and ollama
// providers), and a panic in any of them killed the worker process rather than
// the request.
//
// # What a caller can and cannot observe, stated because it is a real limit
//
// The caller's channel is closed by the producer's own `defer close(ch)`, which
// still runs: this recovery is deferred LAST, so it runs FIRST and the close
// follows it. So the consumer is never left hanging.
//
// But it cannot tell a panic from a stream that simply ended. Neither
// StreamChunk (plugins/llm/providers/types.go) nor plugin.StreamEvent carries
// an error field, so a truncated stream and a completed one are the same three
// values on the wire. Giving the consumer that signal means adding a field to a
// public type, which is a larger change than this one and is deliberately not
// made here. What the OPERATOR gets is complete: the panic, its stack, and the
// plugin marked unhealthy so the next call is refused rather than attempted.
//
// tracker may be nil, for a goroutine that belongs to no plugin health record.
func RecoverGoroutine(pluginName string, tracker *PluginHealthTracker, fn func()) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		stack := string(debug.Stack())
		panicErr := &PanicError{Plugin: pluginName, Value: r, Stack: stack}
		if tracker != nil {
			tracker.MarkUnhealthy(pluginName, panicErr)
		}
		log.Printf("[plugin] %s panicked in a background goroutine: %v\n%s", pluginName, r, stack)
	}()
	fn()
}
