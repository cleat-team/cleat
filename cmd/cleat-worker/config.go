package main

import (
	"flag"
	"os"
	"strings"
	"time"
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
	rlsCheck = flag.String("rls-check", "auto",
		"Row-level security enforcement check on startup: \"auto\" refuses to start when "+
			"--require-auth is set (multi-tenant) and warns otherwise, \"require\" always "+
			"refuses, \"off\" skips the check. PostgreSQL only.")
	driver                        = flag.String("driver", "postgres", "Database driver: postgres, mysql, or mssql")
	concurrency                   = flag.Int("concurrency", 10, "Max concurrent workflow executions")
	maxQueued                     = flag.Int("max-queued", 0, "Max queued (ready) workflows before rejecting new starts (0 = unlimited)")
	heartbeatInterval             = flag.Duration("heartbeat", 5*time.Second, "Heartbeat interval")
	pollInterval                  = flag.Duration("poll", 500*time.Millisecond, "Poll interval when no work")
	notifyChannel                 = flag.String("notify-channel", "cleat_dispatch", "PostgreSQL NOTIFY channel for dispatch wake-up (empty disables)")
	apiAddr                       = flag.String("api-addr", "", "HTTP API listen address (e.g., :8080)")
	pprofAddr                     = flag.String("pprof-addr", "", "Go pprof HTTP listen address (e.g., :6060)")
	taskQueuesStr                 = flag.String("task-queue", "default", "Comma-separated task queues to poll (e.g. \"default,gpu,high-memory\")")
	compactionThreshold           = flag.Int("compaction-threshold", 100, "Number of events before history compaction triggers")
	compactionInterval            = flag.Duration("compaction-interval", 5*time.Minute, "Interval between compaction checks")
	shardsFile                    = flag.String("shards-file", "", "Path to shards JSON config for multi-shard operation")
	pluginConfigFile              = flag.String("plugin-config", "", "path to plugin config JSON file")
	memorySoftLimit               = flag.Float64("memory-soft-limit", 0.80, "Memory soft limit fraction 0.0-1.0 (stop claiming new work)")
	memoryHardLimit               = flag.Float64("memory-hard-limit", 0.95, "Memory hard limit fraction 0.0-1.0 (reject API workflows)")
	memoryCheckInterval           = flag.Duration("memory-check-interval", 2*time.Second, "Interval between memory readings")
	memorySampleRetention         = flag.Int("memory-sample-retention", 1000, "Max samples per workflow definition")
	requireAuth                   = flag.Bool("require-auth", true, "Require API key authentication (default: true when --api-addr is set)")
	requireSignalAuth             = flag.Bool("require-signal-auth", true, "Require signal authorization: checks caller identity against target's allowed_signals (default: true)")
	generateAPIKeyFor             = flag.String("generate-api-key", "", "Generate a new API key for the given tenant UUID and exit")
	maxBodySize                   = flag.Int64("max-body-size", 1048576, "Maximum request body size in bytes (default 1 MiB)")
	httpReadTimeout               = flag.Duration("http-read-timeout", 30*time.Second, "HTTP read timeout")
	httpWriteTimeout              = flag.Duration("http-write-timeout", 60*time.Second, "HTTP write timeout")
	httpIdleTimeout               = flag.Duration("http-idle-timeout", 120*time.Second, "HTTP idle timeout")
	tenantResolver                = flag.String("tenant-resolver", "single-tenant", "Tenant resolution mode: 'single-tenant' (default), 'header:<name>' (header-based), 'api-key' (from API key)")
	rateLimit                     = flag.Float64("rate-limit", 100, "Requests/second/IP rate limit (only when --api-addr is set)")
	rateLimitBurst                = flag.Int("rate-limit-burst", 200, "Rate limit burst size")
	rateLimitPerTenant            = flag.Float64("rate-limit-per-tenant", 0, "Requests/second per tenant (0 = disabled; requires --require-auth)")
	rateLimitPerTenantBurst       = flag.Int("rate-limit-per-tenant-burst", 0, "Burst size for per-tenant rate limit")
	maxRetries                    = flag.Int("max-retries", 100, "Maximum retry attempts for DurableCallWithRetry")
	retentionDays                 = flag.Int("retention-days", 30, "Days to retain completed/failed workflow event history (0 disables)")
	wasmCacheMaxEntries           = flag.Int("wasm-cache-max-entries", 100, "Max WASM byte cache entries (LRU eviction)")
	wasmCacheMaxMB                = flag.Int("wasm-cache-max-mb", 500, "Max WASM byte cache total size in MB (LRU eviction)")
	schemaName                    = flag.String("schema", "public", "PostgreSQL schema for cleat tables (default \"public\"). Sets search_path on connections; CREATE SCHEMA IF NOT EXISTS on startup.")
	peerSchemas                   = flag.String("peer-schemas", "", "Comma-separated list of peer cleat schemas this instance can interact with (cross-instance child workflows, signals)")
	disableChecksumVerification   = flag.Bool("disable-checksum-verification", false, "Disable event history checksum verification on replay (default: enabled)")
	wasmMemoryMaxMB               = flag.Int("wasm-memory-max-mb", 32, "Max WASM linear memory per module in MB (default 32 MB = 512 pages; 0 = use default)")
	wasmCumulativeAllocationMaxMB = flag.Int("wasm-cumulative-allocation-max-mb", 0, "Max cumulative WASM linear memory across all concurrent executions in MB (default 0 = unlimited)")
	wasmInstructionLimit          = flag.Int("wasm-instruction-limit", 0, "Max WASM instructions per invocation (0 = no limit). Backend-agnostic: enforced via a wazero function-call listener on the wazero backend and via wasmtime fuel (SetConsumeFuel/SetFuel) on the wasmtime backend — the two mechanisms count at different granularities (function calls vs. individual WASM instructions), so the same numeric value bounds differently on each backend.")
	wasmInstanceTimeout           = flag.Duration("wasm-instance-timeout", 30*time.Second, "Max wall-clock time for a single WASM invocation (one fresh execution or one replay pass) before it is forcibly interrupted. Enforced via wasmtime epoch interruption on the wasmtime backend (bounds even a WASM module stuck in a tight loop that never calls back into the host) and via context cancellation on the wazero backend. This is the primary bound against a runaway workflow hanging a worker; 0 disables it and is NOT recommended.")
	noPerStepFlush                = flag.Bool("no-per-step-flush", false, "Skip per-step event flush; rely on batch finalization for persistence (higher throughput, weaker crash safety)")
	writeAheadIntentOps           = flag.String("write-ahead-intent-ops", "", "Comma-separated service.operation pairs that must use write-ahead call intent: the engine commits a pending event before dispatching, so a crash mid-call is reported as ambiguous on replay instead of silently repeating the side effect. Costs one extra synchronous round trip per call, so declare only operations that are not safe to repeat (a card charge, not a GET). Independent of --no-per-step-flush, which does not defer these writes.")
	batchFlushDisabled            = flag.Bool("batch-flush-disabled", false, "Disable adaptive batch flushing (always use direct per-step flush)")
	batchFlushMaxWaitMs           = flag.Int("batch-flush-max-wait-ms", 8, "Max milliseconds to wait accumulating events in batch mode")
	batchFlushMaxSize             = flag.Int("batch-flush-max-size", 200, "Max events per batch flush transaction")
	batchFlushEnterRate           = flag.Int("batch-flush-enter-rate", 500, "Steps/sec threshold to enter adaptive batch mode")
	batchFlushExitRate            = flag.Int("batch-flush-exit-rate", 250, "Steps/sec threshold to exit batch mode (hysteresis, must be < enter-rate)")
	batchFlushMaxConns            = flag.Int("batch-flush-max-connections", 50, "Max DB connections for adaptive flusher's dedicated pool")
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
	encryptSensitivePayloads      = flag.Bool("encrypt-sensitive-payloads", false, "Enable encryption of sensitive event payload fields")
	maxQuotaEvents                = flag.Int("max-quota-events", 0, "Max events per workflow (0 = unlimited)")
	maxQuotaChildren              = flag.Int("max-quota-children", 0, "Max child workflows per workflow (0 = unlimited)")
	maxQuotaConcurrencyKeys       = flag.Int("max-quota-concurrency-keys", 0, "Max concurrency keys per workflow (0 = unlimited)")
	maxWorkflowDuration           = flag.Duration("max-workflow-duration", 0, "Maximum wall-clock duration per workflow execution (0 = no limit). Workflows exceeding this are cancelled and fail with a timeout error.")
	healthCheckInterval           = flag.Duration("health-check-interval", 30*time.Second, "Interval for background loop health checks (0 disables watchdog)")
	maxPluginConnections          = flag.Int("max-plugin-connections", 10, "Maximum database connections across all plugins (0 = no separate pool)")
	otelEndpoint                  = flag.String("otel-endpoint", "", "OTLP HTTP endpoint for trace export (e.g., localhost:4318)")
	otelDisabled                  = flag.Bool("otel-disabled", false, "Disable OpenTelemetry trace export")
	benchSvcURL                   = flag.String("bench-svc-url", "", "Base URL for bench-svc HTTP service (e.g., http://localhost:8080). When set, unknown service calls are forwarded to this endpoint.")
	tenantPoolMaxConns            = flag.Int("tenant-pool-max-conns", 25, "Max open connections per tenant pool (MySQL/MSSQL only)")
	logLevel                      = flag.String("log-level", "info", "Log level: debug, info, warn, error")
	enableAdminAPI                = flag.Bool("enable-admin-api", false, "Enable admin API endpoints (force-complete, force-fail, re-replay)")
	verifyBackend                 = flag.Bool("verify-backend", false, "Report whether this binary has the wasmtime backend and exit (0 = yes, 1 = no). Intended as a build-time gate: see the Dockerfile.")
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
