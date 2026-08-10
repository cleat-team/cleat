package plugin

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"sync"
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
	unhealthy map[string]error // plugin name -> fatal error
}

// NewPluginHealthTracker creates a new PluginHealthTracker.
func NewPluginHealthTracker() *PluginHealthTracker {
	return &PluginHealthTracker{
		unhealthy: make(map[string]error),
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
	t.unhealthy[pluginName] = err
	t.mu.Unlock()
}

// IsHealthy reports whether the plugin is healthy.
func (t *PluginHealthTracker) IsHealthy(pluginName string) bool {
	t.mu.RLock()
	_, ok := t.unhealthy[pluginName]
	t.mu.RUnlock()
	return !ok
}

// UnhealthyError returns the error that caused the plugin to be marked
// unhealthy, or nil if the plugin is healthy.
func (t *PluginHealthTracker) UnhealthyError(pluginName string) error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.unhealthy[pluginName]
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
	for name, err := range t.unhealthy {
		s := HealthStatus{Name: name, Healthy: false}
		if err != nil {
			s.Error = err.Error()
		}
		statuses = append(statuses, s)
	}
	return statuses
}

// RecoverPluginFunc wraps a PluginFunc with panic recovery.
// If the wrapped function panics, the panic is caught, the plugin is
// marked unhealthy via the provided tracker, and a PanicError is returned.
// The full stack trace is logged using the standard log package.
func RecoverPluginFunc(pluginName string, tracker *PluginHealthTracker, fn PluginFunc) PluginFunc {
	return func(ctx context.Context, inputJSON string) (outputJSON string, err error) {
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

// RecoverPluginStreamFunc wraps a PluginStreamFunc with panic recovery.
// If the wrapped function panics during setup (before returning the channel),
// the panic is caught and handled like RecoverPluginFunc. Panics during
// channel consumption are not caught here — the consumer must handle them.
func RecoverPluginStreamFunc(pluginName string, tracker *PluginHealthTracker, fn PluginStreamFunc) PluginStreamFunc {
	return func(ctx context.Context, inputJSON string) (ch <-chan StreamEvent, err error) {
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
