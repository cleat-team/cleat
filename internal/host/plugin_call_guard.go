package host

import "fmt"

// PluginCallGuard enforces call_plugin capability restrictions.
// It restricts which WASM plugins can call which other plugins' functions.
type PluginCallGuard struct {
	// allowedCalls maps caller plugin name -> set of target plugin names it can call.
	// An empty map = no restrictions (for Go compile-time plugins and workflow code).
	allowedCalls map[string]map[string]bool
}

// NewPluginCallGuard creates a new PluginCallGuard with no restrictions.
func NewPluginCallGuard() *PluginCallGuard {
	return &PluginCallGuard{
		allowedCalls: make(map[string]map[string]bool),
	}
}

// Allow sets which plugins the given caller can call.
// targets of ["*"] means allow all.
func (g *PluginCallGuard) Allow(callerName string, targets []string) {
	allowed := make(map[string]bool)
	for _, t := range targets {
		allowed[t] = true
	}
	g.allowedCalls[callerName] = allowed
}

// Check verifies that caller is allowed to call target.
// Returns nil if allowed, or an error if denied.
// If caller is not in the guard (e.g., workflow code, Go compile-time plugins),
// the call is always allowed.
func (g *PluginCallGuard) Check(callerName, targetName string) error {
	targets, ok := g.allowedCalls[callerName]
	if !ok {
		// caller not restricted -- allow (workflow code, Go plugins)
		return nil
	}
	if targets["*"] {
		return nil
	}
	if targets[targetName] {
		return nil
	}
	return fmt.Errorf("plugin %q is not allowed to call plugin %q. Add %q to the caller's call_plugin capability in its manifest.", callerName, targetName, targetName, callerName, targetName)
}
