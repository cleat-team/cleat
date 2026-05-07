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
	Config   []byte
	Logger   *slog.Logger
	TenantID uuid.UUID
	Done     <-chan struct{}

	// StartWorkflow starts a new workflow instance using the latest deployed version.
	// Plugins use this to trigger workflow executions (e.g., from cron schedules
	// or job queues). Returns the run ID of the new workflow instance.
	StartWorkflow func(ctx context.Context, defName string, input json.RawMessage) (runID string, err error)

	// SignalWorkflow delivers a signal to a running workflow instance.
	// The signal name and JSON payload are recorded deterministically
	// in the workflow_signals table.
	SignalWorkflow func(ctx context.Context, workflowID, signalName, payload string) error
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

// HasHostFunctions: plugin adds functions callable from workflows.
// These functions are automatically recorded in event history and
// replayed deterministically -- plugin authors don't need to handle replay.
type HasHostFunctions interface {
	Plugin
	RegisterHostFunctions(scope FuncRegistry) error
}

// FuncOptions configures a registered host function.
type FuncOptions struct {
	Name       string // function name (required)
	Idempotent bool   // if true, safe to re-invoke during replay
}

// FuncRegistry lets plugins register workflow-callable functions.
// The plugin name is implicit -- each plugin gets its own scoped registry.
type FuncRegistry interface {
	// Register adds a host function. The engine handles WASM I/O,
	// event history recording, and deterministic replay.
	Register(opts FuncOptions, fn PluginFunc) error
}

// PluginFunc is a plugin host function implementation.
// Takes JSON input, returns JSON output.
type PluginFunc func(ctx context.Context, inputJSON string) (outputJSON string, err error)

// StreamEvent represents a single chunk of a streaming response.
type StreamEvent struct {
	Index   int    `json:"i"`
	Content string `json:"c"`
	Finish  bool   `json:"f"`
}

// PluginStreamFunc is a plugin host function that returns a stream of events.
// Takes JSON input and returns a channel that receives stream events.
type PluginStreamFunc func(ctx context.Context, inputJSON string) (<-chan StreamEvent, error)

// StreamFuncRegistry lets plugins register streaming host functions.
type StreamFuncRegistry interface {
	RegisterStream(opts FuncOptions, fn PluginStreamFunc) error
}

// HasHealth: plugin reports its health status.
type HasHealth interface {
	Plugin
	Health() error // nil = healthy
}
