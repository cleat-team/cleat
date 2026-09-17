package main

import (
	"flag"
	"os"
	"strings"
	"time"

	"github.com/cleat-team/cleat/engine"

	"github.com/cleat-team/cleat/migration"
)

// Config holds structured configuration for the cleat worker.
type Config struct {
	DBURL               string
	Concurrency         int
	HeartbeatInterval   time.Duration
	PollInterval        time.Duration
	APIAddr             string
	TaskQueues          []string
	CompactionThreshold int
	CompactionInterval  time.Duration
	ShardsFile          string
	PluginConfigFile    string
	RequireAuth         bool
	RequireSignalAuth   bool
	MaxBodySize         int64
	MaxAttempts         int
	WASMCacheMaxEntries int
	WASMCacheMaxBytes   int64
	RedactPatternsFile  string
	DBMaxOpenConns      int
	DBMaxIdleConns      int
	LogLevel            string
	LogFormat           string
	OTelEndpoint        string
	OTelDisabled        bool
	MigrationsDir       string
}

// Package-level flag variables used across the worker.
var (
	dbURL = flag.String("db", "", "Database connection URL (required). For Postgres: postgres://... For MySQL: user:pass@tcp(host:port)/dbname?parseTime=true For MSSQL: sqlserver://user:pass@host:port?database=dbname")

	// migrateDBURL exists so that --db can be an unprivileged role.
	//
	// The role cleat should run as (migrations/postgres/005_app_role.sql) owns
	// nothing and has no DDL rights -- that is what makes it subject to
	// row-level security, and RLS is the only tenant isolation
	// GetWorkflowByID and ListWorkflows have. But migrations obviously do need
	// DDL rights, and workers run them at boot. Two DSNs is the way out:
	// privileged for the schema, unprivileged for everything after.
	//
	// Defaults to --db, so a deployment that has not been split keeps working
	// exactly as before.
	migrateDBURL = flag.String("migrate-db", "",
		"Database URL used only for schema migrations, which need DDL rights (default: --db). "+
			"Set this when --db is an unprivileged role such as cleat_app.")

	// rlsCheck decides what happens when the runtime connection turns out not
	// to be subject to row-level security.
	// "when --require-auth is set" was the old wording here too, and it reads
	// as "if you pass the flag" -- which made the warning arm look like the
	// default. --require-auth defaults TRUE, so the refusing arm is what a
	// worker takes unless someone opts out. See the note on requireAuth below.
	rlsCheck = flag.String("rls-check", "auto",
		"Row-level security enforcement check on startup. \"auto\" (the default) REFUSES "+
			"to start on a connection that bypasses RLS, because --require-auth defaults "+
			"true; it only warns if --require-auth=false. \"require\" always refuses, "+
			"\"off\" skips the check. PostgreSQL only.")
	driver      = flag.String("driver", "postgres", "Database driver: postgres, mysql, or mssql")
	concurrency = flag.Int("concurrency", 10, "Max concurrent workflow executions")
	// 200 is twenty workers' worth at the default --concurrency of 10, per
	// tick, and the reaper ticks at most every 10s -- so an ordinary failure
	// (one worker, or several) is reclaimed in a single tick and never
	// notices this. It binds only when the stale set is far larger than any
	// plausible number of simultaneous worker deaths, which is the signature
	// of a stall rather than of workers dying. cleat#1320.
	maxReclaimPerTick = flag.Int("max-reclaim-per-tick", 200,
		"Max stale instances the reaper reclaims per tick (0 = unbounded; see --help for why bounding matters)")
	maxQueued           = flag.Int("max-queued", 0, "Max queued (ready) workflows before rejecting new starts (0 = unlimited)")
	heartbeatInterval   = flag.Duration("heartbeat", 5*time.Second, "How often a worker proves it is alive. This ALSO sets two expiry windows unless --reclaim-timeout overrides the first: a run is reclaimable at max(2x this, 10s), and a worker's membership lease expires on the same shape. Raising it to survive a longer database outage therefore also delays how quickly a genuinely dead worker's runs are picked up -- use --reclaim-timeout to separate those.")
	reclaimTimeout      = flag.Duration("reclaim-timeout", 0, "How long a run may go without a heartbeat before another worker may claim it. 0 (the default) derives it as max(2x --heartbeat, 10s), which is the historical behaviour and changes nothing. Set it to decouple the two: the heartbeat is how often a live worker checks in, this is how long a DEAD one's work stays stranded, and a database outage stops the heartbeat without the worker being dead. Sizing it to a failover window (tens of seconds for streaming replication, longer for managed Multi-AZ) buys outage tolerance at the cost of that much extra delay before a crashed worker's runs are recovered. Refused if below 2x --heartbeat, which would reap runs whose workers are heartbeating normally. Does NOT move the worker-membership lease, which is a different question -- which workers exist, for shard distribution -- and still follows --heartbeat.")
	pollInterval        = flag.Duration("poll", 500*time.Millisecond, "Poll interval when no work")
	notifyChannel       = flag.String("notify-channel", "cleat_dispatch", "PostgreSQL NOTIFY channel for dispatch wake-up (empty disables)")
	apiAddr             = flag.String("api-addr", "", "HTTP API listen address (e.g., :8080)")
	pprofAddr           = flag.String("pprof-addr", "", "Go pprof HTTP listen address (e.g., :6060)")
	taskQueuesStr       = flag.String("task-queue", "default", "Comma-separated task queues to poll (e.g. \"default,gpu,high-memory\")")
	compactionThreshold = flag.Int("compaction-threshold", 100, "Number of events before history compaction triggers")
	compactionInterval  = flag.Duration("compaction-interval", 5*time.Minute, "Interval between compaction checks")
	retentionInterval   = flag.Duration("retention-interval", 24*time.Hour, "Interval between retention sweeps")
	// SEPARATE FROM THE REAPER'S STALE TIMEOUT ON PURPOSE, though they start
	// from the same idea. The reaper asks "has this run missed enough
	// heartbeats that I should take it back", and answers in seconds because
	// reclaiming early is cheap. This asks "has a run been wedged long enough
	// that a human should look", and a human paged every ten seconds stops
	// reading the page. cleat_workflows_stuck has always been described as
	// "stalled beyond the configured stall threshold" and nothing configured
	// it; this is that threshold. cleat#1317.
	stallThreshold = flag.Duration("stall-threshold", 5*time.Minute,
		"How long a running workflow may go without progress before it counts toward cleat_workflows_stuck. "+
			"Independent of the reaper's reclaim timeout, which is --reclaim-timeout or, unset, a multiple of --heartbeat: this one "+
			"is an alerting threshold, not a recovery one.")
	// The window for cleat_concurrency_keys_expiring_soon. Deliberately its own
	// flag rather than a multiple of the sweep interval: it answers "how much
	// does the key sweep still owe", and tying it to how often we LOOK would
	// make the number move when the observer changed rather than when the
	// system did. cleat#1317.
	keyExpiryWindow = flag.Duration("concurrency-key-expiry-window", 5*time.Minute,
		"How far ahead cleat_concurrency_keys_expiring_soon looks. A key inside this window "+
			"is one the sweep owes shortly; a rising count is the leading indicator of a sweep "+
			"falling behind.")
	metricsSweepInterval = flag.Duration("metrics-sweep-interval", 60*time.Second,
		"Interval between database sweeps that publish the gauges nothing else feeds: stuck workflows, "+
			"event-history size and row count, and active concurrency keys. 0 disables the sweep.")
	shardsFile            = flag.String("shards-file", "", "Path to shards JSON config for multi-shard operation")
	pluginConfigFile      = flag.String("plugin-config", "", "path to plugin config JSON file")
	memorySoftLimit       = flag.Float64("memory-soft-limit", 0.80, "Memory soft limit fraction 0.0-1.0 (stop claiming new work)")
	memoryHardLimit       = flag.Float64("memory-hard-limit", 0.95, "Memory hard limit fraction 0.0-1.0 (reject API workflows)")
	memoryCheckInterval   = flag.Duration("memory-check-interval", 2*time.Second, "Interval between memory readings")
	memorySampleRetention = flag.Int("memory-sample-retention", 1000, "Max samples per workflow definition")
	// The help text used to read "(default: true when --api-addr is set)",
	// which describes when auth is APPLIED and reads as a condition on the
	// DEFAULT. It is not one: the value is true whether or not an API is
	// served, and main.go reads that value -- with no --api-addr condition --
	// to decide what -rls-check=auto does. So a worker on a connection that
	// bypasses row-level security REFUSES TO START by default, and the old
	// wording made that look like a warning. It took two sessions and three
	// documents down the wrong path before anyone ran `cleat-worker -h`.
	// cleat#1180, cleat#1204.
	requireAuth = flag.Bool("require-auth", true,
		"Require API key authentication on --api-addr. Also selects the refusing arm "+
			"of -rls-check=auto, whether or not --api-addr is set")
	// Defaults to false. Originally because nothing in the product could write
	// workflow_instances.allowed_signals, so defaulting it on denied every
	// cross-workflow, plugin and external signal on a deployment that had never
	// opted into anything, with no supported way to permit one.
	//
	// That half is fixed -- WorkflowStore.SetAllowedSignalCallers and
	// PUT /api/workflows/:id/allowed-signals exist as of 2026-09-02 -- and the
	// default still stays off, for a reason that outlives it: a workflow starts
	// with an empty list and nothing sets one at start time, so turning this on
	// denies every signal until an operator makes a second call per workflow.
	// It is a per-deployment decision, not yet a safe default.
	// IMPROVEMENT-PLAN 3.15.
	// OFF BY DEFAULT, and it has to be: every existing deployment has zero rows
	// in tenant_domains, so defaulting this on would refuse every
	// authenticated request everywhere. But an opt-in security control is
	// cleat#1581's problem again -- an unconfigured default is
	// indistinguishable from a missing feature -- so when it IS on and no
	// domain is configured, the worker refuses to start rather than refusing
	// every request at runtime. See checkHostBindingConfigured.
	requireHostMatch = flag.Bool("require-host-match", false,
		"Refuse a request whose Host header does not belong to the authenticated tenant, "+
			"per the tenant_domains table (cleat#1568). Per-tenant URLs are not safe without "+
			"it: otherwise one tenant's API key works against another tenant's hostname. "+
			"The worker REFUSES TO START with this set and no domains configured, because "+
			"that configuration refuses every authenticated request.")

	requireSignalAuth = flag.Bool("require-signal-auth", false, "Require signal authorization: checks caller identity against target's allowed_signals (set it with PUT /api/workflows/{id}/allowed-signals). Off by default: workflows start with an empty list, so enabling this denies every signal until callers are granted")
	generateAPIKeyFor = flag.String("generate-api-key", "", "Generate a new API key for the given tenant UUID and exit")
	// --create-tenant is the other half of --generate-api-key, which mints a key
	// for a tenant UUID it does not create. admin.tenant_api_keys.tenant_id
	// REFERENCES admin.tenants(tenant_id), so minting against an id that does not
	// exist fails on the foreign key -- and until this flag there was no command
	// that made one. A test harness wanting a second tenant had to INSERT INTO
	// admin.tenants directly, which puts raw SQL underneath the assertions that
	// exist to test tenant isolation (cleat#1114).
	//
	// PostgreSQL only, and loudly: auth.CreateTenant refuses on MySQL and SQL
	// Server rather than emitting PostgreSQL SQL at them, because RETURNING has
	// no MySQL equivalent and SQL Server spells it OUTPUT. That refusal is the
	// correct behaviour and this flag surfaces it verbatim.
	createTenantNamed = flag.String("create-tenant", "", "Create a tenant with the given name, print its UUID, and exit (PostgreSQL only)")

	// Migration.Down finally has a caller. cleat#1290.
	//
	// A one-shot administrative flag, like --create-tenant above and
	// --generate-api-key below, and for the same reason: cleat-worker is the
	// only binary that links the plugin registry, so it is the only one that
	// can read a plugin's Down SQL. `cleat plugin uninstall` deprecates a
	// version and never touches tables, which its own help text says.
	//
	// --dry-run FIRST in the usage text on purpose: this executes SQL that
	// plugin authors wrote to drop their own tables, and nothing in cleat has
	// ever run it before.
	uninstallPlugin = flag.String("uninstall-plugin", "",
		"Reverse a plugin's applied migrations by running its Down SQL, newest first, and "+
			"exit. Refuses rather than partly reversing if an applied version declares no "+
			"Down, or if another loaded plugin declares one of the same tables. Pair with "+
			"--uninstall-dry-run first.")
	uninstallDryRun = flag.Bool("uninstall-dry-run", false,
		"With --uninstall-plugin, report what would be reversed and exit without executing "+
			"any Down SQL.")
	createTenantDisplayName = flag.String("tenant-display-name", "", "Display name for --create-tenant (defaults to the name)")
	maxBodySize             = flag.Int64("max-body-size", 1048576, "Maximum request body size in bytes (default 1 MiB)")
	httpReadTimeout         = flag.Duration("http-read-timeout", 30*time.Second, "HTTP read timeout")
	httpWriteTimeout        = flag.Duration("http-write-timeout", 60*time.Second, "HTTP write timeout")
	httpIdleTimeout         = flag.Duration("http-idle-timeout", 120*time.Second, "HTTP idle timeout")
	tenantResolver          = flag.String("tenant-resolver", "single-tenant", "Tenant resolution mode: 'single-tenant' (default), 'header:<name>' (header-based), 'api-key' (from API key)")
	rateLimit               = flag.Float64("rate-limit", 100, "Requests/second/IP rate limit (only when --api-addr is set)")
	rateLimitBurst          = flag.Int("rate-limit-burst", 200, "Rate limit burst size")
	rateLimitPerTenant      = flag.Float64("rate-limit-per-tenant", 0, "Requests/second per tenant (0 = disabled; requires --require-auth)")
	rateLimitPerTenantBurst = flag.Int("rate-limit-per-tenant-burst", 0, "Burst size for per-tenant rate limit")
	maxRetries              = flag.Int("max-retries", 100, "Maximum retry attempts for DurableCallWithRetry")
	// WHAT THIS FLAG ACTUALLY DOES: it clears compaction state. Its
	// event_history arm cannot match anything (cleat#1016).
	//
	// The arm selects `status IN ('done','failed')`, and those rows are already
	// gone -- finalize_workflow_status deletes a workflow's events when it
	// reaches either status, deliberately, so event_history stays bounded to
	// active workflows. See PostgresStore.DeleteExpiredEvents, which measures it.
	//
	// AND THE PART AN OPERATOR WILL GET WRONG: this flag does NOT bound
	// event_history for 'terminated' or 'dead_lettered' runs. The arm does not
	// select them, and neither TerminateWorkflow nor MoveToDeadLetterQueue calls
	// finalize_workflow_status -- so their events are deleted only when the
	// workflow row itself is swept, by --completed-workflow-retention-days or
	// --dead-letter-retention-days, both of which default to 0 (off).
	//
	// So on default settings the events of every terminated and dead-lettered
	// run are retained indefinitely, and the on-by-default flag whose name says
	// "retention" is not what bounds them.
	retentionDays                  = flag.Int("retention-days", 30, "Days after which the compaction state of completed/failed workflows is cleared (0 disables). This does NOT bound event_history: its event arm selects done/failed runs, whose events finalize_workflow_status already deleted at terminal time, so that arm cannot match. Events of 'terminated' and 'dead_lettered' runs are bounded only by --completed-workflow-retention-days and --dead-letter-retention-days, both off by default. See cleat#1016.")
	completedWorkflowRetentionDays = flag.Int("completed-workflow-retention-days", 0, "Days to retain workflow_instances rows for terminal workflows (done/failed/terminated) before permanently deleting them, along with any remaining event_history. 0 (default) disables this -- unlike --retention-days, this deletes the workflow record itself (status, result, error, def_name), not just its step-by-step history, so it is opt-in rather than on by default. dead_lettered workflows are never touched by this flag.")
	// VERSION GC. Three flags: one switch and two policy knobs. cleat#1315.
	//
	// --version-gc-interval is the switch and defaults to 0 (off), for the same
	// reason --completed-workflow-retention-days does: GC permanently deletes
	// workflow DEFINITIONS, and an in-flight instance whose version is gone
	// cannot find its WASM binary to replay against. That is a materially more
	// destructive thing to ship silently-on than clearing compaction state.
	//
	// Until this flag existed, GC ran only when a person invoked it --
	// `cleatctl versions gc` or POST /api/versions/gc -- and its policy was
	// engine.DefaultGCOptions(), compiled in and unreachable from either
	// surface. So an operator could neither schedule it nor tune it, and
	// docs/troubleshooting.md told them to adjust a retention policy with two
	// flags that did not exist.
	versionGCInterval = flag.Duration("version-gc-interval", 0,
		"Interval between automatic workflow-version garbage collection sweeps. "+
			"0 (default) disables the sweep entirely -- GC then runs only when invoked "+
			"through 'cleatctl versions gc' or POST /api/versions/gc. Opt-in because GC "+
			"deletes workflow definitions permanently, and an in-flight instance whose "+
			"version has been collected cannot replay.")
	versionGCMinVersions = flag.Int("version-gc-min-versions", engine.DefaultMinVersionsToKeep,
		"Minimum number of recent versions to retain per workflow during GC, regardless "+
			"of age or activity. Applies to the scheduled sweep; 'cleatctl versions gc' and "+
			"POST /api/versions/gc take their own overrides.")
	versionGCMaxAge = flag.Duration("version-gc-max-age", engine.DefaultMaxVersionAge,
		"Maximum age of a DEPRECATED version before it becomes eligible for GC. A version "+
			"that is not deprecated is never collected whatever its age.")

	deadLetterRetentionDays       = flag.Int("dead-letter-retention-days", 0, "Days to retain dead-lettered workflow_instances rows before permanently deleting them, along with their event_history, signals and promises. 0 (default) disables this. Separate from --completed-workflow-retention-days, which never touches dead-lettered workflows: a dead-lettered run is the one an operator most wants to inspect afterwards, so it has its own lifecycle and its own knob rather than being swept up with completed work.")
	wasmCacheMaxEntries           = flag.Int("wasm-cache-max-entries", 100, "Max WASM byte cache entries (LRU eviction)")
	wasmCacheMaxMB                = flag.Int("wasm-cache-max-mb", 500, "Max WASM byte cache total size in MB (LRU eviction)")
	wasmModuleCacheMaxEntries     = flag.Int("wasm-module-cache-max-entries", engine.DefaultModuleCacheMaxEntries, "Max COMPILED-MODULE cache entries (LRU eviction). Distinct from --wasm-cache-max-entries, which bounds the WASM BYTE cache: this one bounds compiled native code, and until cleat#1563 it had no bound at all. Entries rather than megabytes because a compiled module exposes no cheap size; observe it with cleat_wasm_compiled_module_cache_entries.")
	schemaName                    = flag.String("schema", "public", "PostgreSQL schema for cleat tables (default \"public\"). Sets search_path on connections; CREATE SCHEMA IF NOT EXISTS on startup.")
	disableChecksumVerification   = flag.Bool("disable-checksum-verification", false, "Disable event history checksum verification on replay (default: enabled)")
	wasmMemoryMaxMB               = flag.Int("wasm-memory-max-mb", 32, "Max WASM linear memory per module in MB (default 32 MB = 512 pages; 0 = use default)")
	wasmCumulativeAllocationMaxMB = flag.Int("wasm-cumulative-allocation-max-mb", 0, "Max cumulative WASM linear memory across all concurrent executions in MB (default 0 = unlimited)")
	wasmInstructionLimit          = flag.Int("wasm-instruction-limit", 0, "Max WASM instructions per invocation (0 = no limit). Enforced via wasmtime fuel (SetConsumeFuel/SetFuel).")
	wasmDeferBudget               = flag.Duration("wasm-defer-budget", engine.DefaultWasmtimeDeferBudget, "Max wall-clock time for the cleanup pass the host runs on a workflow it killed -- the defers of a workflow stopped by --wasm-instance-timeout, --wasm-instruction-limit, or an unrecoverable guest runtime failure. This is EXTRA execution granted to a workflow the fence already stopped, so the worst case a runaway workflow can occupy a worker is --wasm-instance-timeout plus this. 0 uses the built-in default.")
	wasmInstanceTimeout           = flag.Duration("wasm-instance-timeout", 30*time.Second, "Max GUEST EXECUTION time for a single WASM invocation (one fresh execution or one replay pass) before it is forcibly interrupted. Enforced via wasmtime epoch interruption, which bounds even a WASM module stuck in a tight loop that never calls back into the host. Time the guest spends blocked in a host call -- a service call, a plugin call, a retry backoff -- is NOT charged against it; use --wasm-wall-clock-ceiling to bound that. 0 disables it and is NOT recommended.")
	wasmWallClockCeiling          = flag.Duration("wasm-wall-clock-ceiling", 5*time.Minute, "Max WALL-CLOCK time for a single WASM invocation, including time spent waiting inside host calls. This is the bound that stops a workflow blocked on an unresponsive service from holding a worker slot indefinitely; --wasm-instance-timeout bounds the guest's own execution and does not cover waiting. Must be >= --wasm-instance-timeout to mean anything. 0 falls back to --wasm-instance-timeout, which is the pre-3.90 behaviour of one value doing both jobs.")
	hostRetryBudget               = flag.Duration("host-retry-budget", engine.DefaultHostRetryBudget, "CEILING on how much worst-case backoff a retry policy may carry and still be run on the host, inside one segment, holding the worker slot. A policy above this is refused (callErrorCode 6, RetryPolicyTooLong); the guest then runs it itself, suspending between attempts, which releases the slot. A tenant may set a LOWER value in tenant_settings and it is clamped to this; it can never raise it. This is ALSO the boundary at which a backoff survives a worker loss: on the host path the wait is worker-local and a crash discards its remainder (decided, cleat#1111), while the guest's own loop backs off with a durable sleep and resumes. Moving this flag moves policies between those two behaviours as well as between holding a slot and suspending. Keep it well below --wasm-wall-clock-ceiling: that ceiling covers the whole invocation, so a budget near it lets one retry policy consume everything the workflow had. 0 uses the built-in default.")
	noPerStepFlush                = flag.Bool("no-per-step-flush", false, "Skip per-step event flush; rely on batch finalization for persistence (higher throughput, weaker crash safety)")
	writeAheadIntentOps           = flag.String("write-ahead-intent-ops", "", "Comma-separated service.operation pairs that must use write-ahead call intent: the engine commits a pending event before dispatching, so a crash mid-call is reported as ambiguous on replay instead of silently repeating the side effect. Costs one extra synchronous round trip per call, so declare only operations that are not safe to repeat (a card charge, not a GET). Independent of --no-per-step-flush, which does not defer these writes.")
	batchFlushDisabled            = flag.Bool("batch-flush-disabled", false, "Disable adaptive batch flushing (always use direct per-step flush)")
	batchFlushMaxWaitMs           = flag.Int("batch-flush-max-wait-ms", 8, "Max milliseconds to wait accumulating events in batch mode")
	batchFlushMaxSize             = flag.Int("batch-flush-max-size", 200, "Max events per batch flush transaction")
	batchFlushEnterRate           = flag.Int("batch-flush-enter-rate", 500, "Steps/sec threshold to enter adaptive batch mode")
	batchFlushExitRate            = flag.Int("batch-flush-exit-rate", 250, "Steps/sec threshold to exit batch mode (hysteresis, must be < enter-rate)")
	batchFlushMaxConns            = flag.Int("batch-flush-max-connections", 50, "Max DB connections for adaptive flusher's dedicated pool")
	flushRetryWindow              = flag.Duration("flush-retry-window", 0, "How long a failed event flush keeps retrying before the step is reported as unpersisted. 0 (the default) uses the historical 750ms, which is the sleep budget the batch path already had -- so the default changes the batch path not at all and gives the DIRECT path, which made one attempt and no retry, the same budget. Raise it to ride out a database failover: set it to your failover budget (tens of seconds for streaming replication, minutes for managed Multi-AZ). RAISING IT ABOVE --reclaim-timeout BUYS NOTHING BY ITSELF, because an outage that long also stops this worker's heartbeat, so its runs are reclaimable the moment the database returns and the retry that finally succeeds loses its fence -- raise both together. It also bounds how long one unpersisted step can stall a workflow, including during shutdown, and errors the engine does not recognise are retried, so a permanent one costs the whole window.")
	syncCommitOff                 = flag.Bool("synchronous-commit-off", false, "SET LOCAL synchronous_commit = off in finalize transactions (higher throughput, weaker durability)")
	wasmOutputBufferSize          = flag.Int("wasm-output-buffer-size", 32768, "WASM output buffer size in bytes (default 32 KB)")
	wasmMaxStringLen              = flag.Int("wasm-max-string-len", 65536, "Maximum WASM string parameter length in bytes (default 64 KB)")
	wasmCacheDir                  = flag.String("wasm-cache-dir", "", "Directory for disk-backed compiled WASM module cache (empty disables)")
	wasmDiskCacheMaxFiles         = flag.Int("wasm-disk-cache-max-files", 100, "Max files in the disk-backed compiled WASM module cache (LRU eviction)")
	redactPatternsFile            = flag.String("redact-patterns-file", "", "Path to file with custom redaction patterns (one per line)")
	childBindingOverride          = flag.String("child-binding-override", "", "Override child binding policy: 'latest' to always use latest child versions (for debugging). Also read from CLEAT_CHILD_BINDING_OVERRIDE env var.")
	dbCredentialProvider          = flag.String("db-credential-provider", "env", "DB credential provider: env, vault, or aws-secrets-manager")
	dbCredentialPath              = flag.String("db-credential-path", "", "Path/name for credential provider (vault path or AWS secret name)")
	encryptionKeyFile             = flag.String("encryption-key-file", "", "Path to file containing base64-encoded AES-256-GCM encryption key (32 bytes after decode)")

	// ROLE-PER-TENANT ISOLATION. cleat#1307.
	//
	// A SEPARATE FLAG, not something --require-auth turns on. The dead branch
	// this replaces was gated on *requireAuth, and --require-auth defaults
	// TRUE -- so reusing it would switch a new isolation mechanism on for every
	// existing deployment at upgrade, silently changing which connection plugin
	// code runs on. That is the trap --completed-workflow-retention-days
	// defaults to 0 to avoid.
	//
	// "rls" is what production does today: set_config('cleat.tenant_id', ...)
	// per transaction on the owner pool, where a path that forgets the call is
	// a cross-tenant read. "role" opens a pool per tenant authenticating AS
	// that tenant's PostgreSQL login role, so the credential carries the
	// identity and there is nothing to forget -- plugin.TenantPools' own
	// comment: "the connection IS the tenant".
	tenantIsolation = flag.String("tenant-isolation", "rls",
		"How tenant isolation is enforced for plugin host functions: 'rls' (default) sets "+
			"cleat.tenant_id per transaction on the shared owner pool; 'role' opens a "+
			"connection per tenant authenticating as that tenant's PostgreSQL login role. "+
			"'role' requires --tenant-role-secret-file and PostgreSQL.")
	tenantRoleSecretFile = flag.String("tenant-role-secret-file", "",
		"Path to a file holding the key that derives tenant role passwords, base64-encoded "+
			"(at least 32 bytes after decode). Required by --tenant-isolation=role. Each "+
			"tenant's password is HMAC-SHA256(key, tenant_id), so nothing per-tenant is "+
			"stored and any worker can open a tenant pool without reading a credential.")
	connectionBudgetFlag = flag.Int("connection-budget", 0,
		"Total database connections this worker may open across ALL its pools "+
			"(0 = unset, no check). A worker opens six independent pools -- core, plugin, "+
			"adaptive flusher, per-shard, migration and one per tenant under "+
			"--tenant-isolation=role -- and sizing from --concurrency alone under-provisions "+
			"a default worker by a factor of five. When set, the worker logs the breakdown at "+
			"startup and refuses to start if its fixed pools alone exceed it. cleat#1486")
	clusterConnectionBudgetFlag = flag.Int("cluster-connection-budget", 0,
		"Total database connections ALL workers together may open (0 = unset, no sharing). "+
			"Separate from --connection-budget, which bounds one worker: this is the number "+
			"the cluster divides, and each worker takes an equal share of it -- "+
			"budget/live-workers, floor 1. Requires the worker registry, so workers must "+
			"reach the same database. Set both to mean \"no worker above X, and no more than "+
			"Y between them\": the effective budget is the smaller. A worker shrinks its "+
			"share the moment another joins, and waits before growing when one leaves, "+
			"because a crashed worker's connections outlive its heartbeat. cleat#1487")
	maxStreamReadersFlag = flag.Int("max-stream-readers", 1024,
		"How many live SSE readers of GET /api/workflows/{id}/stream this worker may hold "+
			"at once (0 = unlimited). A reader costs a goroutine, a held HTTP connection "+
			"and a bounded chunk buffer. It HOLDS no database connection, so this is "+
			"separate from --connection-budget, which bounds database pools -- but each "+
			"reader does poll the run's status once per 15s heartbeat. Over the ceiling "+
			"the route answers 503 with Retry-After. "+
			"cleat#1572")
	unservableBackoffFlag = flag.Duration("unservable-release-backoff", defaultUnservableBackoff,
		"How long a run waits before it can be claimed again after a worker released it "+
			"because that worker could not serve it -- its loaded plugins do not satisfy the "+
			"workflow's plugin_deps, or its WASM binary disagrees with the def row. Both are "+
			"WORKER-LOCAL facts, so another worker may serve the run; before cleat#1710 either "+
			"one destroyed it. ReleaseWorkflow writes next_wake_at on the ROW, so this throttles "+
			"the whole pool rather than one worker: a run nothing can serve costs one "+
			"claim-and-release per interval cluster-wide and stays visible as 'ready' rather "+
			"than failing. Lower it to shorten the window in which a rolling deploy delays a run "+
			"a sibling could already take. cleat#1710")
	maxStreamPollReadersFlag = flag.Int("max-stream-poll-readers", 1024,
		"How many readers of GET /api/workflows/{id}/stream this worker may hold at once "+
			"that are following a run from event_history rather than from the in-memory "+
			"tail (0 = unlimited). That is every reader of a run this worker is NOT "+
			"executing, which behind a load balancer with N workers is roughly (N-1)/N of "+
			"them. A SEPARATE number from --max-stream-readers because the resource is "+
			"different: these readers hold no chunk buffer and instead issue a steady "+
			"query rate -- four round trips per interval on PostgreSQL, of which one "+
			"carries data. Over the ceiling the route answers 503 with Retry-After. "+
			"cleat#1639")
	streamPollIntervalFlag = flag.Duration("stream-poll-interval", defaultStreamPollInterval,
		"Base gap between two reads of a followed run's chunks from event_history. This is "+
			"latency the reader SEES, so it trades directly against query rate. It is a "+
			"base, not a period: a read that finds nothing backs off to at most "+
			"2s and any chunk resets it, so an idle run costs a fraction of this rate. "+
			"Only reached for readers on a worker not executing the run. cleat#1639")
	egressAllowlistFlag = flag.String("egress-allowlist", "",
		"Hosts THIS DEPLOYMENT may reach, comma-separated. Empty (the default) permits "+
			"every PUBLIC host -- the loopback, link-local and RFC1918 floor still applies, "+
			"so \"all public\" really is all public.\n"+
			"Egress needs BOTH permissions: a destination is reachable only when the "+
			"operator permits it and the requesting tenant permits it (cleatctl "+
			"egress-allow). Either saying no is a refusal, and the refusal names which. "+
			"A tenant can only ever narrow within this list.\n"+
			"It is the ONLY policy for egress that has no tenant at all -- plugin "+
			"background sweeps, and auth-exempt routes such as the OAuth callback, which "+
			"says in its own code that the state parameter identifies the tenant so there "+
			"is none to scope by.\n"+
			"Entry forms: an exact host, or a leading dot for any host ending in it, which "+
			"excludes the apex. cleat#1565")
	pluginEgressAllowPrivate = flag.String("plugin-egress-allow-private", "",
		"Hosts a PLUGIN may reach even though they resolve into private address space, "+
			"comma-separated. Empty (the default) means none, and the floor refuses every "+
			"private address as before.\n"+
			"This exists for a plugin endpoint the operator runs on purpose -- a "+
			"self-hosted model server is the motivating case, since plugins/llm's ollama "+
			"provider defaults to http://localhost:11434 and was otherwise unreachable "+
			"with no way to permit it.\n"+
			"It applies to PLUGIN egress only. A workflow's own fetches and the embedded "+
			"runner are unaffected: a guest is code cleat did not write, and nothing it "+
			"supplies should reach a private address whatever is configured here.\n"+
			"It CANNOT reach link-local (169.254.0.0/16, fe80::/10), the unspecified, "+
			"multicast or reserved ranges. Naming a host in one of those is accepted at "+
			"startup and still refused at call time, saying which range and why -- the "+
			"cloud metadata endpoint is what this whole policy exists to refuse.\n"+
			"Entry forms: an exact host as it appears in the endpoint URL. Matching is on "+
			"the HOST, so \"localhost\" and \"127.0.0.1\" are different entries. cleat#1627")
	encryptSensitivePayloads = flag.Bool("encrypt-sensitive-payloads", false, "Enable encryption of sensitive event payload fields")
	migrationLockTimeout     = flag.Duration("migration-lock-timeout", migration.DefaultLockTimeout,
		"How long a migration statement waits for a lock before failing. A migration needing ACCESS EXCLUSIVE "+
			"(ALTER TABLE, CREATE INDEX without CONCURRENTLY, DROP) otherwise waits forever behind any conflicting "+
			"lock -- an idle-in-transaction session is enough -- and queues every later reader behind itself. It is "+
			"worse than a slow boot: every worker migrates at boot, a worker stuck in migrations is not heartbeating, "+
			"so its runs go stale and the reaper collects them while the database is mid-DDL. 0 disables the bound and "+
			"restores the historical behaviour of waiting indefinitely, which is a real choice for a first migration "+
			"onto a large busy table. PostgreSQL only: the other dialects have no pinned migration session to set it "+
			"on, and setting it on a pooled handle would leak the bound into application traffic. See cleat#1775.")

	// DefaultMaxQuotaEvents bounds how much history one run may write.
	// cleat#1829.
	//
	// WHY THIS QUOTA HAS A DEFAULT AND THE THREE BELOW DO NOT. Exceeding this
	// one is a REFUSAL, not a failure: engine/callerrors.go's
	// eventCapCallError is reported before the call is dispatched, so no side
	// effect happens, the guest unwinds through its entry-point wrapper
	// draining its defers, and the executor records a continue_as_new
	// suspension. The run rolls over. The other three write an error back into
	// the workflow and fail it, so a default there would turn working
	// deployments into failing ones at whatever number was picked.
	//
	// It also costs nothing to enable: setup.go already loads the persisted
	// event count unconditionally -- it was ungated for the replay-tail check
	// in cleat#1507 -- so a cap adds no query.
	//
	// 50,000 is a STARTING POINT, not a measurement, in the same sense as
	// migration.DefaultLockTimeout. Far above any legitimate workflow, far
	// below a runaway, and wrong in the mild direction (one extra
	// continue-as-new) rather than the unbounded one. A distribution of real
	// per-run event counts would beat it.
	DefaultMaxQuotaEvents = 50000

	maxQuotaEvents = flag.Int("max-quota-events", DefaultMaxQuotaEvents,
		"Max events one workflow run may write before the engine continues it as new. This is a "+
			"ROLLOVER, not a failure: the call is refused before dispatch, so no side effect happens, "+
			"defers drain, and the run continues as a fresh one. 0 disables the bound, which leaves a "+
			"looping workflow free to fill event_history -- --retention-days only sweeps TERMINAL runs, "+
			"and a runaway is not terminal. The default is a starting point rather than a measurement; "+
			"see cleat#1829.")
	maxQuotaChildren        = flag.Int("max-quota-children", 0, "Max child workflows per workflow (0 = unlimited). Deliberately unbounded: unlike --max-quota-events, exceeding this FAILS the workflow rather than rolling it over, so a default would break working deployments at whatever number was chosen, and nobody has usage data to choose from. cleat#1829.")
	maxQuotaConcurrencyKeys = flag.Int("max-quota-concurrency-keys", 0, "Max concurrency keys per workflow (0 = unlimited). Deliberately unbounded: unlike --max-quota-events, exceeding this FAILS the workflow rather than rolling it over, so a default would break working deployments at whatever number was chosen, and nobody has usage data to choose from. cleat#1829.")
	maxQuotaSchedules       = flag.Int("max-quota-schedules", 0, "Max cron schedules per tenant (0 = unlimited). Deliberately unbounded: unlike --max-quota-events, exceeding this FAILS the workflow rather than rolling it over, so a default would break working deployments at whatever number was chosen, and nobody has usage data to choose from. cleat#1829.")
	claimAcrossTenants      = flag.Bool("claim-across-tenants", false, "Claim runnable work for every tenant in one query instead of only this worker's own. "+
		"Requires a database-side grant, and on SQL Server it now requires TWO steps rather than one.\n"+
		"PostgreSQL: migrations/postgres/023_cross_tenant_claim.sql.\n"+
		"SQL Server: apply migrations/mssql/optional/cross_tenant_claim.sql -- which is NOT applied "+
		"automatically, because the predicate it installs costs the index seek on any query that does "+
		"not carry its own tenant predicate (5760 logical reads against 33, measured; cleat#1491) -- "+
		"and THEN grant dbo.cleat_admin membership as migrations/mssql/012_admin_role.sql documents. "+
		"012 alone is no longer enough: since cleat#1541 the shipped predicate is the plain one, and a "+
		"member of cleat_admin under it reads IS_ROLEMEMBER = 1 and sees zero rows.\n"+
		"A worker started with this flag reports on both loops at startup whether it actually has the "+
		"capability, so a half-completed setup says so rather than running silently single-tenant.")
	maxWorkflowDuration  = flag.Duration("max-workflow-duration", 0, "CEILING on wall-clock duration for ONE workflow execution segment (0 = no limit); a workflow that suspends and resumes gets a fresh deadline each time. Workflows exceeding it are cancelled and fail with a timeout error. A tenant may set a LOWER value in tenant_settings, and a single run a lower one still at start; neither can raise it. With 0 here the operator sets no bound, so a tenant's value stands alone -- which is how a deployment that never set this flag can still give one tenant a deadline. cleat#1117.")
	healthCheckInterval  = flag.Duration("health-check-interval", 30*time.Second, "Interval for background loop health checks (0 disables watchdog)")
	maxPluginConnections = flag.Int("max-plugin-connections", 10, "Maximum database connections across all plugins (0 = no separate pool)")
	otelEndpoint         = flag.String("otel-endpoint", "", "OTLP HTTP endpoint for trace export (e.g., localhost:4318)")
	otelDisabled         = flag.Bool("otel-disabled", false, "Disable OpenTelemetry trace export")
	serviceEndpointsFlag = flag.String("service-endpoints", "",
		"Comma-separated name=url pairs mapping a service to the base URL that serves it, "+
			"e.g. \"billing=https://billing.internal,crm=https://crm.internal\". A workflow's "+
			"h.DurableCall(\"billing\", \"charge\", ...) is POSTed to {url}/call/billing/charge "+
			"with the caller's traceparent and a replay-stable Idempotency-Key. Registering a "+
			"service here needs no plugin and no worker rebuild. Keyed by SERVICE, not "+
			"service.operation: the operation is a route on the service. Every outbound call "+
			"goes through the egress guard, so the host must also satisfy --egress-allowlist "+
			"and the non-overridable floor. A malformed entry stops the worker at boot.")

	benchSvcURL        = flag.String("bench-svc-url", "", "Base URL for bench-svc HTTP service (e.g., http://localhost:8080). When set, unknown service calls are forwarded to this endpoint.")
	tenantPoolMaxConns = flag.Int("tenant-pool-max-conns", 25, "Max open connections per tenant pool, used by --tenant-isolation=role. PostgreSQL only: plugin.TenantPools authenticates as a PostgreSQL login role (cleat#1307). The help text said MySQL/MSSQL, which was the opposite of the implementation.")
	logLevel           = flag.String("log-level", "info", "Log level: debug, info, warn, error")
	enableAdminAPI     = flag.Bool("enable-admin-api", false, "Enable admin API endpoints (force-complete, force-fail, re-replay)")
	verifyBackend      = flag.Bool("verify-backend", false, "Report whether this binary has the wasmtime backend and exit (0 = yes, 1 = no). Intended as a build-time gate: see the Dockerfile.")
	listPlugins        = flag.Bool("list-plugins", false, "Print the plugins linked into this binary and exit. A plugin registers via init(), so this reports the import block in main.go -- see IMPROVEMENT-PLAN.md 3.315.")
)

func applyChildBindingOverrideEnv() {
	if *childBindingOverride == "" {
		if env := os.Getenv("CLEAT_CHILD_BINDING_OVERRIDE"); env != "" {
			childBindingOverride = &env
		}
	}
}

func resolveDBURL() {
	if *dbURL == "" {
		*dbURL = os.Getenv("DATABASE_URL")
	}
}

// parseWriteAheadIntentOps splits the --write-ahead-intent-ops value into
// "service.operation" keys, dropping empties and surrounding whitespace so a
// trailing comma or a value wrapped across a YAML line does not silently
// declare an operation named "".
//
// It takes the flag pointer rather than reading the global directly so tests
// can exercise it without mutating process-wide flag state.
func parseWriteAheadIntentOps(v *string) []string {
	if v == nil || *v == "" {
		return nil
	}
	var ops []string
	for _, part := range strings.Split(*v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			ops = append(ops, part)
		}
	}
	return ops
}
