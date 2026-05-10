// Package plugin provides a minimal plugin system for cleat.
//
// Plugins are Go packages compiled into the worker binary. They register
// themselves via init() and the central registry. The worker discovers
// them at startup, calls Init(), and optionally calls lifecycle methods
// based on which optional interfaces the plugin implements.
//
// Design principle: give plugins access to infrastructure through
// purpose-built interfaces. The Environment struct provides a
// PluginDB interface (not *sql.DB directly), along with standard
// library types (*http.ServeMux, *slog.Logger).
//
// Crash recovery boundaries:
//
// Go-compiled plugins share the worker process, so a panic in a plugin
// host function can crash the entire worker. The RecoverPluginFunc and
// RecoverPluginStreamFunc wrappers (in recovery.go) add defer/recover
// boundaries around each host function call. When a panic is caught, the
// plugin is marked unhealthy and subsequent invocations are rejected
// without calling into the plugin.
//
// Long-term migration: compile plugins to WASM modules instead of
// linking them into the worker binary. WASM provides process-level
// isolation so a plugin crash cannot affect the worker or other plugins.
// The recovery wrappers exist for Go-compiled plugins only.
package plugin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
)

// PluginInfo describes a plugin for discovery and documentation.
type PluginInfo struct {
	Name           string         `json:"name"`
	Version        string         `json:"version"`
	Description    string         `json:"description"`
	Author         string         `json:"author,omitempty"`
	Requires       []string       `json:"requires,omitempty"`
	DatabaseAccess DatabaseAccess `json:"database_access,omitempty"`
}

// RowScanner abstracts a single row result for single-row queries.
type RowScanner interface {
	Scan(dest ...interface{}) error
}

// Rows is the result of a multi-row query.
type Rows interface {
	RowScanner
	Next() bool
	Close() error
	Err() error
}

// PluginDB is the database handle available to plugins.
// It intentionally does not mirror *sql.DB — plugins get a scoped
// interface appropriate to their declared DatabaseAccess level.
type PluginDB interface {
	Begin(ctx context.Context) (PluginTx, error)
	Exec(ctx context.Context, query string, args ...interface{}) (int64, error)
	Query(ctx context.Context, query string, args ...interface{}) (Rows, error)
	QueryRow(ctx context.Context, query string, args ...interface{}) RowScanner
	Ping(ctx context.Context) error
}

// PluginTx is a transaction scoped to a plugin operation.
type PluginTx interface {
	Exec(ctx context.Context, query string, args ...interface{}) (int64, error)
	Query(ctx context.Context, query string, args ...interface{}) (Rows, error)
	QueryRow(ctx context.Context, query string, args ...interface{}) RowScanner
	Commit() error
	Rollback() error
}

// Plugin is the only required interface. Every plugin must implement this.
type Plugin interface {
	Info() PluginInfo
	Init(ctx context.Context, env *Environment) error
}

// Environment provides plugins with access to cleat infrastructure.
type Environment struct {
	DB       PluginDB
	Mux      *http.ServeMux
	Config   []byte
	Logger   *slog.Logger
	TenantID uuid.UUID
	Done     <-chan struct{}
	Dialect  Dialect

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
//
// Up is the default SQL (PostgreSQL) and is required.
// UpMySQL and UpMSSQL are optional dialect-specific overrides.
// If the active dialect is MySQL or MSSQL and the corresponding
// field is empty, the migration is skipped with a warning.
type Migration struct {
	Version   int
	Up        string // required — SQL for PostgreSQL (the default)
	UpMySQL   string // optional — MySQL DDL. Empty means PG-only for this version.
	UpMSSQL   string // optional — MSSQL DDL. Empty means PG-only for this version.
	Down      string // optional — SQL to roll back
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

