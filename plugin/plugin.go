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
	Scan(dest ...any) error
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
	Exec(ctx context.Context, query string, args ...any) (int64, error)
	Query(ctx context.Context, query string, args ...any) (Rows, error)
	QueryRow(ctx context.Context, query string, args ...any) RowScanner
	Ping(ctx context.Context) error
}

// PluginTx is a transaction scoped to a plugin operation.
type PluginTx interface {
	Exec(ctx context.Context, query string, args ...any) (int64, error)
	Query(ctx context.Context, query string, args ...any) (Rows, error)
	QueryRow(ctx context.Context, query string, args ...any) RowScanner
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
	Mux      ServeMux // *http.ServeMux on host, interface{} on TinyGo
	Config   []byte
	Logger   *slog.Logger
	TenantID string
	Done     <-chan struct{}
	Dialect  Dialect

	// StartWorkflow starts a new workflow instance using the latest deployed
	// version. Plugins use this to trigger workflow executions (e.g. from cron
	// schedules or job queues). Returns the run ID of the new instance.
	//
	// TAKES A STRUCT, AND BOTH KEY AND TENANT ARE REQUIRED. It used to be
	// (ctx, defName, input) and the implementation supplied `""` for the
	// idempotency key and the all-zeros default for the tenant, so no plugin
	// could start a workflow that was either retry-safe or correctly
	// attributed. cleat#1555 and cleat#1580.
	//
	// A STRUCT RATHER THAN TWO MORE STRINGS, because the key and the tenant are
	// both strings and adjacent. Passing them the wrong way round would compile,
	// run, and produce a workflow owned by a tenant named after an idempotency
	// key -- a failure with no symptom at the call site.
	StartWorkflow func(ctx context.Context, req StartRequest) (runID string, err error)

	// SignalWorkflow delivers a signal to a running workflow instance.
	// The signal name and JSON payload are recorded deterministically
	// in the workflow_signals table.
	SignalWorkflow func(ctx context.Context, workflowID, signalName, payload string) error

	// Audit provides access to the plugin audit log. Plugins can record
	// deployment, deprecation, capability changes, and invocation events.
	// May be nil if the audit log is not configured.
	Audit *AuditLogger
}

// StartRequest is everything a plugin must supply to start a workflow.
//
// EVERY FIELD IS REQUIRED and the implementation rejects an empty one rather
// than defaulting it. Both of the added fields exist because the values were
// previously hardcoded at the seam:
//
//   - IdempotencyKey was `""`, so a plugin that retried a start -- or crashed
//     between starting and recording that it had -- created a second run. Each
//     caller is a sweep over a durable table and already holds a natural key.
//     cleat#1555.
//   - TenantID was engine.DefaultTenantUUID, so a run triggered for tenant X
//     was stored as the default tenant's. Every caller knows the real tenant;
//     the seam discarded it. cleat#1580.
//
// Defaulting either would restore precisely the silent failure the requirement
// exists to remove, so an empty value is an error rather than a fallback.
type StartRequest struct {
	// DefName is the workflow definition to start.
	DefName string

	// Input is the workflow's input payload.
	Input json.RawMessage

	// IdempotencyKey deduplicates retries of the same logical start.
	//
	// IT MUST BE STABLE ACROSS THOSE RETRIES: derive it from the durable row
	// being acted on, never from the wall clock at dispatch. A key built from
	// "now" differs on every attempt, so it deduplicates nothing -- and the
	// failure has no symptom, because duplicate runs look exactly as they do
	// without a key at all.
	IdempotencyKey string

	// TenantID is the tenant the started run belongs to, as a UUID string.
	TenantID string
}

// AuditLogger is the interface for recording plugin lifecycle events.
// The concrete implementation writes to the plugin_audit_log table.
type AuditLogger interface {
	// Deploy records a plugin deployment event.
	Deploy(ctx context.Context, pluginName, pluginVersion string) error

	// Deprecate records a plugin deprecation event.
	Deprecate(ctx context.Context, pluginName, pluginVersion string) error

	// CapabilityChange records a change in plugin capabilities.
	CapabilityChange(ctx context.Context, pluginName, pluginVersion, details string) error

	// Invocation records a plugin function invocation. The dropRate
	// (0.0-1.0) controls sampling: 0.995 means ~1 in 200 are logged.
	Invocation(ctx context.Context, pluginName, functionName string, dropRate float64) error

	// EnforceRetention deletes audit log entries that exceed the policy.
	EnforceRetention(ctx context.Context, policy any) (int64, error)
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
	Version int
	Up      string // required — SQL for PostgreSQL (the default)
	UpMySQL string // optional — MySQL DDL. Empty means PG-only for this version.
	UpMSSQL string // optional — MSSQL DDL. Empty means PG-only for this version.
	Down    string // optional — SQL to roll back

	// TenantScoped names tables this migration creates whose rows belong to
	// one tenant, identified by a tenant_id column. The runtime enables
	// row-level security on each and installs a policy filtering on the
	// cleat.tenant_id set by SQLDBAdapter for the statement. cleat#1277.
	//
	// Declare it rather than writing the policy into Up, so that twenty-odd
	// hand-written schemas do not each get it slightly wrong -- and so that
	// the set of tenant-scoped plugin tables is a value the runtime can
	// read rather than a pattern someone greps for.
	//
	// POSTGRESQL ONLY, and this is a real limit rather than a rounding
	// error. MySQL has no row-level security, so a table named here is
	// scoped by the plugin's own WHERE clause there and by nothing else.
	// SQL Server binds a tenant to a whole connection pool
	// (tenantSessionConnector), which a per-request tenant does not fit.
	// On both, this field is accepted and does nothing.
	//
	// ONLY FOR TABLES WHOSE EVERY READER HAS A TENANT. A policy fails
	// closed, so a plugin that also sweeps across tenants from a background
	// loop -- where no tenant is in context -- will find those sweeps
	// returning nothing. kvstore qualifies because all of its access is
	// request-scoped; most plugins do not yet.
	TenantScoped []string

	// SweepTables names tables a cross-tenant sweep in this plugin READS OR
	// WRITES but which carry no tenant column, so they get no policy and are
	// not listed in TenantScoped.
	//
	// WHY THIS EXISTS AT ALL, because "grant the sweep everything" is the
	// obvious alternative. A cross-tenant sweep runs under SET LOCAL ROLE
	// cleat_sweep (engine/plugindb_tenant.go, cleat#1490), and switching role
	// changes the privilege set for EVERY table the transaction touches, not
	// only the ones carrying a policy. cleat_sweep therefore needs privileges
	// on these by name. Granting it blanket privileges instead would hand any
	// plugin that names itself cross-tenant the engine's own tables, which
	// migrations 023, 024 and 073 each declined to do -- 073 in as many words:
	// "it would let ANY plugin that names itself cross-tenant read the
	// engine's own table. That trades a bounded question for an open
	// capability, on behalf of two callers."
	//
	// A MISSING ENTRY FAILS LOUDLY, which is the point of naming them. The
	// sweep gets `permission denied for table X (42501)` in the plugin's own
	// tests. That is why this is a declaration and not a heuristic: the
	// alternative shapes all fail by quietly widening what a sweep can reach.
	//
	// NOT A SUBSTITUTE FOR TenantScoped. A table listed here gets a GRANT and
	// no policy -- it is asserted to have no tenant column. A table with a
	// tenant column belongs in TenantScoped, which gives it both.
	//
	// PostgreSQL only, for the same reason TenantScoped is: the other two
	// dialects install no policy and do not switch role.
	SweepTables []string
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
//
// THE TWO REPLAY PROPERTIES ARE SEPARATE BECAUSE THEY ANSWER DIFFERENT
// QUESTIONS. cleat#1318. Idempotent asks "is re-running this safe?"; replay
// asks "should this run at all?". One boolean carried both until seven
// functions were registered against the weaker reading, and they come apart
// exactly where the wording is most inviting -- an idempotent WRITE reads as a
// yes and is still a live write issued during a reconstruction of a past
// execution.
//
// Replay re-invokes only when BOTH are true. Either alone is not enough:
// re-invoking something unsafe repeats a side effect, and re-invoking something
// safe-but-unstable hands the workflow a value it never branched on.
type FuncOptions struct {
	Name string // function name (required)

	// Idempotent reports that calling this function again has no additional
	// effect -- no new side effects, nothing created twice. It says nothing
	// about what the second call RETURNS.
	//
	// Nothing in the engine reads this for retry today; it is declarative.
	Idempotent bool

	// SameValueOnReplay reports that re-invoking during a replay yields what
	// the original call yielded. This is the property that licenses discarding
	// recorded output, and it is a statement about the WORLD, not about the
	// function: a perfectly deterministic function fails it if its inputs can
	// change between the original run and the replay. A feature flag an
	// operator can toggle, a vector index anything can insert into, and a
	// provider's model list all fail it.
	//
	// Note what it is not: not purity, and not determinism. The question is
	// agreement with history.
	SameValueOnReplay bool
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
