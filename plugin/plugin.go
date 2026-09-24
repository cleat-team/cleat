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
	"errors"
	"fmt"
	"log/slog"
)

// ErrNotConfigured is returned by Init when a plugin received no
// configuration at all (env.Config is empty) and, absent one, cannot do
// anything useful -- so it disables itself rather than running degraded.
//
// IT IS NOT THE SAME AS A CONFIGURATION ERROR. A plugin given a config
// section that fails validation (bad JSON, a required field left blank
// inside an otherwise-present section) must return a plain error instead,
// so that mistake keeps failing loudly. Conflating the two turned "I forgot
// to set the SendGrid API key" and "I never touched this plugin" into the
// same ERROR-level log line on every stock worker start, cleat#2070.
//
// A caller distinguishes the two with errors.Is(err, ErrNotConfigured), not
// by matching error text -- wrap it, don't replace it:
//
//	if len(env.Config) == 0 {
//		return fmt.Errorf("myplugin: %w", plugin.ErrNotConfigured)
//	}
var ErrNotConfigured = errors.New("plugin not configured")

// ErrFatalMisconfiguration is returned by Init when the config a plugin
// received is not merely absent (see ErrNotConfigured) but actively
// contradicts itself in a way that must stop the WHOLE WORKER, not just
// disable the one plugin.
//
// The case that motivated it: cleat#1992 part 1 moved email-notify's
// SendGrid key out of --plugin-config into a deployment secret, gated behind
// a new "email_enabled" field. A pre-upgrade config still carrying
// "sendgrid_api_key" but not yet "email_enabled" is a deployment that was
// clearly sending email and, under the ordinary ErrNotConfigured path, would
// silently stop -- disabled, ERROR-logged at most, worker still starts.
// Found in cleat-review's #2202 re-check.
//
// A caller distinguishes this from every other Init error with
// errors.Is(err, ErrFatalMisconfiguration): cmd/cleat-worker's Init loop
// logs it and os.Exit(1)s immediately, the same severity as
// checkRequiredDeploymentSecrets' fail-closed boot check, rather than
// marking the plugin unhealthy and continuing.
var ErrFatalMisconfiguration = errors.New("plugin: fatal misconfiguration")

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

	// EventsLost is how a plugin that buffers events reports the ones it gave up on, so the
	// host can count them where an operator looks (cleat#2168). pluginName is the plugin's own
	// name, reason a short fixed word (audit-log uses buffer_full, insert_failed, shutdown, shutdown_inflight) and
	// n how many. It is called once per loss and must not block.
	//
	// NIL MEANS "NOBODY IS COUNTING", and a plugin must still log every loss: the worker sets
	// it, and cleattest, the embedded runner and a plugin's own unit tests do not.
	EventsLost func(pluginName, reason string, n int64)

	// HTTPTransport is the egress-guarded RoundTripper a plugin must use for
	// every outbound HTTP request. cleat#1565.
	//
	// NIL MEANS UNGUARDED, and that is a deliberate hole with a fence around
	// it rather than a default: cleattest, the embedded runner and a plugin's
	// own unit tests construct an Environment directly and have no worker to
	// build one. The worker always sets it, and
	// TestEveryPluginRoutesItsEgressThroughTheGuard fails if a plugin reaches
	// for http.DefaultTransport or builds a bare client instead of using this.
	//
	// What it enforces has two layers. The FLOOR -- loopback, link-local,
	// RFC1918 -- always applies, so a misconfigured or attacker-supplied
	// plugin endpoint cannot reach cloud instance metadata or the worker's own
	// admin port. Above it, a tenant's allowlist applies when a tenant is in
	// context (host-function calls), and the deployment-level
	// --egress-allowlist applies when one is not (background loops,
	// which are sweeps and have no tenant -- the same shape as cleat#1278).
	HTTPTransport EgressTransport

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

	// Secrets and Payloads give a plugin access to per-tenant encrypted
	// storage (cleat#1992) -- see secrets.go for the design and why neither
	// takes a tenantID parameter. May be nil where no master key is
	// configured, the same convention DB/HTTPTransport already use: a plugin
	// that dereferences a nil Environment field is a bug the type system
	// cannot catch here, same as it cannot for those either.
	Secrets  Secrets
	Payloads Payloads

	// DeploymentSecrets gives a plugin access to credentials that belong to
	// the whole deployment rather than to a tenant (cleat#1992 part 1) -- see
	// secrets.go's DeploymentSecrets doc comment for the fixed names and why
	// a plugin must call Get per use rather than cache it from Config. Nil
	// under the same convention as Secrets/Payloads.
	DeploymentSecrets DeploymentSecrets
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

	// EntryPoint names the workflow entry point to start, for a multi-entry
	// workflow. Empty means implicit resolution -- today that is
	// determineEntryPoint's single-entry-point shortcut or its
	// firstHandleExport fallback; once a workflow declares more than one
	// entry point, an empty EntryPoint here is exactly as ambiguous as an
	// empty entry_point on the public start API, and is resolved (or
	// refused) the same way. cleat#2114.
	//
	// Merge it into Input with MergeEntryPoint before calling StartWorkflow
	// -- callers must not roll their own merge. See MergeEntryPoint's doc
	// comment for why the shape (flat, not nested) matters.
	EntryPoint string
}

// MergeEntryPoint flat-merges an entry point name into a JSON object input,
// as a sibling field "__entry_point" -- the one shape determineEntryPoint
// (cmd/cleat-worker/setup.go) actually reads: "an explicit __entry_point
// field IN THE START INPUT". No guest -- Go's generated dispatcher included
// -- unwraps a nested "input" key or strips __entry_point, so wf.Input must
// reach the guest with __entry_point sitting flat alongside the entry's own
// fields, not wrapping them.
//
// cleat#2108 found the REST start API doing this wrong -- wrapping instead
// of merging, `{"input": originalInput, "__entry_point": ...}` -- which
// determineEntryPoint still resolved (it only reads the top-level key) but
// corrupted the entry's own input, so the guest failed to deserialize it.
// cleat#2114 is the same shape one seam over: event-triggered starts built
// plugin.StartRequest with no way to express an entry point at all. Both
// paths call this one helper so there is exactly one place that shape is
// implemented, instead of a second copy free to drift the way #2108's did.
//
// entryPoint == "" is a no-op: input is returned unchanged (todays implicit
// resolution). A non-empty entryPoint against a non-object input is
// refused -- there is no field to merge __entry_point into, and silently
// discarding the caller's input (as the pre-#2108 code did, producing
// {"input":null,"__entry_point":...}) is worse than an explicit error.
func MergeEntryPoint(input json.RawMessage, entryPoint string) (json.RawMessage, error) {
	if entryPoint == "" {
		return input, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(input, &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("entry_point requires the input to be a JSON object so __entry_point "+
			"can be merged into it (got %s): %w", string(input), err)
	}
	obj["__entry_point"] = entryPoint
	merged, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("encoding input with entry_point: %w", err)
	}
	return merged, nil
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
	Down    string // optional — SQL to roll back (PostgreSQL, and the default)

	// DownMySQL and DownMSSQL are the dialect-specific reversals, symmetric
	// with UpMySQL and UpMSSQL.
	//
	// They exist because a migration whose Up is dialect-specific could not be
	// reversed before cleat#1622. Every migration in the tree until then had a
	// non-empty Up as WELL as its dialect arms, so the asymmetry never
	// surfaced: the first MySQL-only migration -- converting a plugin's JSON
	// columns to LONGTEXT, which PostgreSQL and SQL Server do not need -- had
	// nowhere to put a reversal that is also MySQL-only. A single Down would
	// have been run verbatim against all three.
	// DialectSpecific explains why this migration deliberately has no arm for
	// one or more dialects, and must say which and why.
	//
	// It exists because "no arm for this dialect" has two causes that look
	// identical: an author forgot, or the dialect genuinely needs nothing.
	// cmd/cleat-worker's TestEveryLinkedPluginSupportsEveryDialectTheWorkerRuns
	// On refuses the first, correctly -- a skipped migration has its version
	// recorded as done, so a plugin can initialise against tables that were
	// never created (cleat#1157). That guard's own comment already says a
	// migration with no Up at all is "a different defect and not this test's
	// subject"; this field is how an author says so, rather than the guard
	// guessing.
	//
	// NARROW BY DESIGN, like TenantScoped and SweepTables: the exemption
	// requires this to be NON-EMPTY, so a migration missing an arm by accident
	// still trips the guard. A reason is required, not a boolean, because the
	// next reader has to be able to check the claim.
	//
	// cleat#1622 is the first use: converting MySQL's JSON columns to LONGTEXT
	// is work PostgreSQL's JSONB and SQL Server's NVARCHAR(MAX) do not need
	// and must not have.
	DialectSpecific string

	DownMySQL string // optional — MySQL reversal. Falls back to Down when empty.
	DownMSSQL string // optional — MSSQL reversal. Falls back to Down when empty.

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
	// POSTGRESQL AND SQL SERVER install a policy from this field. MySQL has
	// no row-level security, so a table named here is scoped by the plugin's
	// own WHERE clause there and by nothing else; the field is accepted and
	// does nothing. That is a real limit rather than a rounding error.
	//
	// THE TWO ARMS ARE NOT IDENTICAL, and the differences are measured rather
	// than assumed (applyTenantScopingMSSQL has the tables):
	//
	//   - a read with NO tenant set raises on PostgreSQL and returns an EMPTY
	//     RESULT on SQL Server, because a SQL Server filter predicate must be
	//     an inline table-valued function and cannot raise;
	//   - writes are refused on SQL Server by BLOCK predicates rather than by
	//     the filter, which does not affect writes at all;
	//   - dropping a tenant collects a table's rows on PostgreSQL only, via
	//     admin.plugin_tables, which SQL Server does not have.
	//
	// THE REASON GIVEN HERE FOR SQL SERVER WAS WRONG until cleat#1552: "SQL
	// Server binds a tenant to a whole connection pool
	// (tenantSessionConnector), which a per-request tenant does not fit".
	// Plugins never get that pool -- getPluginDB hands them the main or the
	// plugin pool -- and a per-request tenant fits fine, via
	// sp_set_session_context, which database/sql's connection recycle clears.
	// engine/plugindb_tenant.go now sets it. What is still missing on SQL
	// Server is the half this field controls: applyTenantScoping emits no
	// CREATE SECURITY POLICY there.
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

// HasFinalizeObserver: plugin wants to know when a workflow run it may have
// started (via Environment.StartWorkflow) reaches a terminal status.
//
// NOT transactional with the write that produces finalStatus. cleat#1715's
// design asked for a hook placed inside FinalizeWorkflowSegment's own
// transaction, so a write-back races nothing -- but FinalizeWorkflowSegment
// is called directly on a WorkflowStore (cmd/cleat-worker/setup.go), not
// through the Engine, and WorkflowStore has four implementations (Postgres,
// MySQL, MSSQL, Sharded). Threading a hook field through all four for a
// capability exactly one plugin uses is the twenty-edits shape this issue's
// own design note argues against for CompleteWorkflow; the same argument
// applies here to the store interface.
//
// The gap this leaves is a crash between FinalizeWorkflowSegment's commit and
// ObserveFinalize's own write -- a small, same-process window, not a network
// round trip. It is not silently accepted: cleat#1715 also asks for an
// abandonment sweep (a job whose run is no longer in flight and never
// received a terminal write-back), which is exactly the backstop this gap
// needs and would need to exist regardless of whether the hook were
// transactional -- a WORKER that dies between commit and write-back leaves
// the same gap a transactional hook cannot close on its own, because nothing
// guarantees the write-back's SIDE of a two-write transaction runs either
// once the process is gone. The sweep is the actual safety net either way.
type HasFinalizeObserver interface {
	Plugin
	// ObserveFinalize is called AFTER a workflow run reaches a terminal
	// status: "done", "failed", "dead_lettered", "terminated" or
	// "cancelled" -- never "ready", which is a suspend, not a terminal
	// status. Before cleat#1976, only "done" and "failed" ever reached this
	// call; the other three terminal outcomes reached it not at all, so an
	// observer's own bookkeeping (jobqueue's task_queue row, for the
	// currently-only implementer) could only be corrected later by an
	// abandonment sweep inferring from absence rather than being told.
	// Errors are logged and otherwise ignored: a plugin's own bookkeeping
	// must never be able to fail a workflow's finalize.
	ObserveFinalize(ctx context.Context, runID, finalStatus string) error
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

// HasRequiredDeploymentSecrets: plugin names the deployment secrets it
// cannot run without, given the same raw config bytes Init receives
// (cleat#1992 part 1).
//
// Checked by the worker AFTER Init succeeds, not during it: Init's own
// config-presence check is what decides whether the plugin is enabled at
// all (see Environment.Config's doc comment, and ErrNotConfigured), and a
// plugin with no config section is skipped before this is ever consulted.
// This interface answers a narrower, later question -- for a plugin that IS
// enabled, which of its deployment secrets must actually resolve before the
// worker is allowed to serve traffic.
//
// RequiredDeploymentSecrets returns the deployment-secret NAMES this
// instance needs (e.g. "email.sendgrid_api_key", or one
// "llm.providers.<provider>.api_key" per enabled provider) -- not whether
// they currently resolve. The worker checks that separately, against
// Environment.DeploymentSecrets, and refuses to start if any is missing or
// unopenable: a plugin enabled but unable to reach its own credential is
// worse than one not enabled at all, because every call it serves fails
// individually instead of the worker saying so once, at boot.
type HasRequiredDeploymentSecrets interface {
	Plugin
	RequiredDeploymentSecrets(config []byte) ([]string, error)
}

// HasDeploymentSecretPrefix: plugin declares the name prefix its own
// deployment secrets carry (e.g. "email.", "llm.providers."), so the worker
// can hand it a DeploymentSecrets that refuses to Get anything outside that
// prefix. Least privilege, cheap: every plugin currently shares ONE
// DeploymentSecretStore connection wrapped in one adapter, so without this a
// bug in any plugin using DeploymentSecrets (reachable through a workflow's
// own HostCall arguments, not just plugin-author error) could read a
// SIBLING plugin's credential -- llm reading email.sendgrid_api_key, say.
// Optional, unlike HasRequiredDeploymentSecrets, but NOT permissive: a
// plugin that does not implement this gets DeploymentSecrets == nil, not the
// unscoped adapter every plugin used to share regardless of whether it read
// deployment secrets at all. Default-deny, tightened in cleat-review's
// #2202 re-check after the first version of this left every non-declaring
// plugin able to read email's and llm's secrets through the one adapter
// they all received. Only email and llm read deployment secrets today, so
// this costs nothing; a plugin that starts needing one declares its prefix.
// Found in cleat-review's #2202 pass.
type HasDeploymentSecretPrefix interface {
	Plugin
	DeploymentSecretPrefix() string
}

// HasDeploymentSecretRemedyHint: plugin names an additional, non-secret way
// an operator can satisfy HasRequiredDeploymentSecrets -- e.g. blobstore's
// use_iam_credentials, a --plugin-config flag that opts out of the
// deployment secrets this interface's sibling would otherwise require.
// Optional: checkRequiredDeploymentSecrets (cmd/cleat-worker/setup.go)
// appends the hint to its boot-refusal error when a plugin implements this,
// so the operator sees every way to fix the refusal, not just the one
// RequiredDeploymentSecrets is named after.
type HasDeploymentSecretRemedyHint interface {
	Plugin
	DeploymentSecretRemedyHint() string
}
