// Package plugin provides a minimal plugin system for cleat.
//
// Plugins are Go packages compiled into the worker binary. They register
// themselves via init() and the central registry. The worker discovers
// them at startup, calls Init(), and optionally calls lifecycle methods
// based on which optional interfaces the plugin implements.
//
// Design principle: give plugins raw access to infrastructure, not
// abstractions over it. The Environment struct uses standard library
// types (*sql.DB, *http.ServeMux, *slog.Logger) so plugin authors
// don't need to learn a new API.
package plugin

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
)

// PluginInfo describes a plugin for discovery and documentation.
type PluginInfo struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Author      string   `json:"author,omitempty"`
	Requires    []string `json:"requires,omitempty"`
}

// Plugin is the only required interface. Every plugin must implement this.
type Plugin interface {
	Info() PluginInfo
	Init(ctx context.Context, env *Environment) error
}

// Environment provides plugins with raw access to cleat infrastructure.
// No wrappers, no abstractions -- standard library types.
type Environment struct {
	DB       *sql.DB
	Mux      *http.ServeMux
	Config   json.RawMessage
	Logger   *slog.Logger
	TenantID uuid.UUID
	Done     <-chan struct{}
}

// --- Optional interfaces (discovered by loader via type assertion) ---

// Stoppable: plugin needs cleanup on shutdown.
type Stoppable interface {
	Plugin
	Stop(ctx context.Context) error
}

// HasMigrations: plugin needs database tables.
type HasMigrations interface {
	Plugin
	Migrations() []Migration
}

// Migration describes a single database migration.
type Migration struct {
	Version int
	Up      string // SQL to run
	Down    string // SQL to roll back (optional)
}

// HasRoutes: plugin exposes HTTP endpoints.
type HasRoutes interface {
	Plugin
	RegisterRoutes(mux *http.ServeMux) error
}

// HasMiddleware: plugin wraps the HTTP handler chain.
type HasMiddleware interface {
	Plugin
	Middleware(next http.Handler) http.Handler
}

// HasCommands: plugin adds CLI subcommands.
type HasCommands interface {
	Plugin
	RegisterCommands() []Command
}

// Command describes a CLI subcommand exposed by a plugin.
type Command struct {
	Name        string
	Description string
	Run         func(args []string) error
}

// HasBackground: plugin runs a background goroutine.
type HasBackground interface {
	Plugin
	Run(ctx context.Context) error
}

// HasHostFunctions: plugin adds WASM imports callable from workflows.
type HasHostFunctions interface {
	Plugin
	RegisterHostFunctions(builder HostModuleBuilder) error
}

// HostModuleBuilder is the interface for registering WASM host functions.
// This mirrors wazero's HostModuleBuilder pattern so plugins can add
// custom imports callable from workflow WASM modules.
type HostModuleBuilder interface {
	// Register adds a host function. fn must be a function with a signature
	// compatible with wazero's WithFunc (e.g., func(ctx context.Context, m api.Module, params...) uint64).
	Register(name string, fn interface{}) error
}

// HasHealth: plugin reports its health status.
type HasHealth interface {
	Plugin
	Health() error // nil = healthy
}
