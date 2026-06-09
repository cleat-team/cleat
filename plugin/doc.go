// Package plugin provides a minimal plugin system for cleat.
//
// Plugins are Go packages compiled into the worker binary. They register
// via init() using Register(), and the worker discovers them at startup.
// The registry, lifecycle management, host function helpers, and crash
// recovery wrappers live here.
//
// Key types:
//   - Plugin — interface all plugins implement
//   - PluginInfo — metadata for discovery and documentation
//   - Environment — infrastructure access (DB, HTTP mux, logger)
//   - Registry — central plugin function registry
//   - Manifest — plugin manifest for code generation
package plugin
