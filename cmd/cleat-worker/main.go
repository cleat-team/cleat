// Command cleat-worker is a production worker daemon for executing cleat
// workflows. It polls PostgreSQL for runnable workflow instances using
// SELECT ... FOR UPDATE SKIP LOCKED, loads WASM modules, replays event history,
// and drives execution. It handles workflow suspension (sleep, await signals),
// heartbeating, and database failover.
//
// Build:
//
//	go build -o cleat-worker ./cmd/cleat-worker/
//
// Run:
//
//	cleat-worker --db "postgres://user:pass@localhost/cleat?sslmode=disable"
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	_ "net/http/pprof" //nolint:gosec // G108: registers /debug/pprof on DefaultServeMux, which this worker never serves. The API listener builds its own http.NewServeMux; pprof gets a separate opt-in listener behind --pprof-addr, empty by default. See the comment at the pprof server below.
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	// Embed the IANA timezone database in the binary.
	//
	// workflow_schedules.timezone holds an IANA name and the scheduler resolves
	// it with time.LoadLocation, which reads the SYSTEM zoneinfo database. On an
	// image without tzdata installed, every name except "UTC" and "Local" fails
	// to load -- and the failure is not loud: the schedule falls back to UTC and
	// fires at the wrong wall-clock time, on an image that is otherwise working.
	// A scaled-down base image would reintroduce the exact defect the timezone
	// column was added to remove.
	//
	// Roughly 450KB of binary. Cheap next to a scheduler that cannot be trusted
	// to know what "07:00 America/New_York" means.
	_ "time/tzdata"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/migration"
	"github.com/cleat-team/cleat/monitoring/prometheus"
	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
	"golang.org/x/time/rate"

	// Database drivers
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/microsoft/go-mssqldb"

	// Plugins
	// Every bundled plugin, blank-imported so its init() registers it.
	//
	// A plugin registers via an init() in its own package, which runs only if
	// the package is LINKED. --plugin-config supplies configuration and cannot
	// change that, so this block is the plugin feature set of the binary --
	// see IMPROVEMENT-PLAN.md 3.315, which found that only llm was here while
	// event-triggers, event-store, webhook-ingest and kafka-connect were built,
	// documented as features, and reachable from nothing. `cleat-worker
	// --list-plugins` prints what this block yields.
	//
	// Adding one here is not free: plugin.RunMigrations is FATAL at boot
	// (cmd/cleat-worker/main.go, os.Exit(1)), so a plugin whose migration
	// cannot run stops the worker starting.
	_ "github.com/cleat-team/cleat/plugins/auditlog"
	_ "github.com/cleat-team/cleat/plugins/blobstore"
	_ "github.com/cleat-team/cleat/plugins/dag"
	_ "github.com/cleat-team/cleat/plugins/datadogexport"
	_ "github.com/cleat-team/cleat/plugins/email"
	_ "github.com/cleat-team/cleat/plugins/eventstore"
	_ "github.com/cleat-team/cleat/plugins/eventtriggers"
	_ "github.com/cleat-team/cleat/plugins/featureflags"
	_ "github.com/cleat-team/cleat/plugins/jobqueue"
	_ "github.com/cleat-team/cleat/plugins/kafkaconnect"
	_ "github.com/cleat-team/cleat/plugins/kvstore"
	_ "github.com/cleat-team/cleat/plugins/llm"
	_ "github.com/cleat-team/cleat/plugins/notifications"
	_ "github.com/cleat-team/cleat/plugins/oauthprovider"
	_ "github.com/cleat-team/cleat/plugins/pagerdutyalert"
	_ "github.com/cleat-team/cleat/plugins/ratelimiter"
	_ "github.com/cleat-team/cleat/plugins/scheduledbackup"
	_ "github.com/cleat-team/cleat/plugins/scheduler"
	_ "github.com/cleat-team/cleat/plugins/slacknotify"
	_ "github.com/cleat-team/cleat/plugins/webhookingest"
	//
	// pgvector is deliberately NOT here, and the reason is stronger than the
	// "requires pgvector extension" note it replaces. Its Migrations() creates
	// an `embedding vector(1536)` column, which is the FATAL path -- so on any
	// PostgreSQL without the vector extension available, linking it would stop
	// cleat-worker booting. Its Init() does run `CREATE EXTENSION IF NOT
	// EXISTS vector`, and an Init failure is non-fatal (InitAll marks the
	// plugin unhealthy and continues), but that only helps on a server where
	// the extension is installable. Linking pgvector needs its migration to
	// degrade instead, which is a change to the plugin, not to this list.
	// _ "github.com/cleat-team/cleat/plugins/pgvector"
)

func main() {
	flag.Parse()

	// Before anything else, and before any database is needed: --verify-backend
	// answers "does this binary have the wasmtime backend?" and exits.
	if *verifyBackend {
		os.Exit(runVerifyBackend(os.Stdout))
	}

	// Likewise --list-plugins: a plugin is registered by an init() in a linked
	// package, so this answers "what does this binary actually have" without a
	// database, a config file, or reading the source. See IMPROVEMENT-PLAN 3.315.
	if *listPlugins {
		os.Exit(runListPlugins(os.Stdout))
	}

	// Apply CLEAT_CHILD_BINDING_OVERRIDE env var as fallback when the flag is not set.
	applyChildBindingOverrideEnv()

	// Fall back to DATABASE_URL env var if --db is empty.
	resolveDBURL()

	// Set WASM output buffer size before any Runtime is created.
	engine.OutBufSize = uint32(*wasmOutputBufferSize)
	engine.MaxWasmStringLen = uint32(*wasmMaxStringLen)

	workerID := generateWorkerID()

	var level slog.Level
	switch *logLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	logger.InfoContext(context.Background(), "starting worker", "worker_id", workerID, "concurrency", *concurrency)

	if *disableChecksumVerification {
		logger.InfoContext(context.Background(), "checksum verification disabled", "worker_id", workerID)
	} else {
		logger.InfoContext(context.Background(), "checksum verification enabled", "worker_id", workerID)
	}

	// Handle --create-tenant (standalone mode: create a tenant and exit).
	//
	// Placed before --generate-api-key because that is the order they are used
	// in: a key cannot be minted for a tenant that does not exist --
	// admin.tenant_api_keys.tenant_id REFERENCES admin.tenants(tenant_id) -- and
	// before this flag there was no command that created one. See cleat#1114.
	if *createTenantNamed != "" {
		dbURL := *dbURL
		if dbURL == "" {
			dbURL = os.Getenv("DATABASE_URL")
		}
		if dbURL == "" {
			logger.ErrorContext(context.Background(), "--db or DATABASE_URL required for --create-tenant", "worker_id", workerID)
			os.Exit(1)
		}
		gdb, err := sql.Open(sqlDriverName(*driver), dbURL)
		if err != nil {
			logger.ErrorContext(context.Background(), "failed to connect to database — check the --db flag or DATABASE_URL environment variable", "worker_id", workerID, "error", err)
			os.Exit(1)
		}
		defer gdb.Close()
		store, tsErr := auth.NewTenantStoreForDialect(gdb, *driver)
		if tsErr != nil {
			logger.ErrorContext(context.Background(), "cannot create a tenant", "worker_id", workerID, "error", tsErr)
			os.Exit(1)
		}
		display := *createTenantDisplayName
		if display == "" {
			display = *createTenantNamed
		}
		// auth.CreateTenant refuses on MySQL and SQL Server rather than emitting
		// PostgreSQL SQL at them. Surfaced as-is: the message names the dialect,
		// which is more useful than anything this layer could add.
		tid, cErr := store.CreateTenant(context.Background(), *createTenantNamed, display)
		if cErr != nil {
			logger.ErrorContext(context.Background(), "failed to create tenant", "worker_id", workerID, "name", *createTenantNamed, "error", cErr)
			os.Exit(1)
		}
		fmt.Printf("\n")
		fmt.Printf("=== CLEAT TENANT ===\n")
		fmt.Printf("Tenant ID: %s\n", tid)
		fmt.Printf("Name:      %s\n", *createTenantNamed)
		fmt.Printf("\n")
		fmt.Printf("Mint a key for it with:\n")
		fmt.Printf("  cleat-worker --generate-api-key %s --db \"$DSN\"\n", tid)
		fmt.Printf("\n")
		os.Exit(0)
	}

	// Handle --generate-api-key (standalone mode: generate key and exit).
	if *generateAPIKeyFor != "" {
		tenantID, err := uuid.Parse(*generateAPIKeyFor)
		if err != nil {
			logger.ErrorContext(context.Background(), "invalid tenant UUID for --generate-api-key", "worker_id", workerID, "error", err)
			os.Exit(1)
		}
		dbURL := *dbURL
		if dbURL == "" {
			dbURL = os.Getenv("DATABASE_URL")
		}
		if dbURL == "" {
			logger.ErrorContext(context.Background(), "--db or DATABASE_URL required for --generate-api-key", "worker_id", workerID)
			os.Exit(1)
		}
		gdb, err := sql.Open(sqlDriverName(*driver), dbURL)
		if err != nil {
			logger.ErrorContext(context.Background(), "failed to connect to database — check the --db flag or DATABASE_URL environment variable", "worker_id", workerID, "error", err)
			os.Exit(1)
		}
		defer gdb.Close()
		store, tsErr := auth.NewTenantStoreForDialect(gdb, *driver)
		if tsErr != nil {
			logger.ErrorContext(context.Background(), "cannot create an API key", "worker_id", workerID, "error", tsErr)
			os.Exit(1)
		}
		key := auth.GenerateAPIKey()
		if err := store.CreateAPIKey(context.Background(), tenantID, "generated by --generate-api-key", key); err != nil {
			logger.ErrorContext(context.Background(), "failed to create API key", "worker_id", workerID, "driver", *driver, "tenant_id", tenantID, "error", err)
			os.Exit(1)
		}
		fmt.Printf("\n")
		fmt.Printf("=== CLEAT API KEY ===\n")
		fmt.Printf("Key:       %s\n", key)
		fmt.Printf("Tenant ID: %s\n", tenantID)
		fmt.Printf("\n")
		fmt.Printf("Store this key securely. It will NOT be shown again.\n")
		fmt.Printf("Use it in the Authorization header: Authorization: Bearer %s\n", key)
		fmt.Printf("\n")
		os.Exit(0)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	shutdownTelemetry := setupTelemetry(ctx, *otelEndpoint, *otelDisabled, workerID)
	defer shutdownTelemetry()

	// Create OTel metrics instance.
	var metricsInstance *prometheus.Metrics
	{
		m, mErr := prometheus.New(prometheus.Config{
			WorkerID: workerID,
		})
		if mErr != nil {
			logger.ErrorContext(context.Background(), "failed to create metrics", "worker_id", workerID, "error", mErr)
			os.Exit(1)
		}
		metricsInstance = m
	}
	defer metricsInstance.Shutdown(context.Background())

	taskQueues := strings.Split(*taskQueuesStr, ",")

	// Plugin state (populated in both sharded and non-sharded paths).
	var (
		pluginRegistry       = engine.NewPluginRegistry()
		pluginStreamRegistry = engine.NewPluginStreamRegistry()
		plugList             []*plugin.LoadedPlugin
		plugHandler          http.Handler
		plugMux              *http.ServeMux
		bgWg                 sync.WaitGroup
		ratelim              *ipRateLimiter
		tenantLim            *keyedRateLimiter
	)

	defaultTenantID := "00000000-0000-0000-0000-000000000000"

	var store engine.WorkflowStore
	var db *sql.DB
	var pluginDB *sql.DB
	var tenantPools *plugin.TenantPools
	var factory engine.StoreFactory
	var payloadEncryption *engine.PayloadEncryption
	if *shardsFile != "" {
		configs, err := loadShardConfigs(*shardsFile)
		if err != nil {
			logger.ErrorContext(context.Background(), "failed to load shards config — check that the file exists and contains valid JSON", "worker_id", workerID, "file", *shardsFile, "error", err)
			os.Exit(1)
		}
		// Apply --schema flag to shards without an explicit schema.
		for i := range configs {
			if configs[i].Schema == "" {
				configs[i].Schema = *schemaName
			}
		}

		// Load encryption key if configured (sharded path).
		var payloadEncryption *engine.PayloadEncryption
		if *encryptSensitivePayloads {
			if *encryptionKeyFile == "" {
				logger.ErrorContext(context.Background(), "--encrypt-sensitive-payloads requires --encryption-key-file", "worker_id", workerID)
				os.Exit(1)
			}
		}
		if *encryptionKeyFile != "" {
			keyData, kerr := os.ReadFile(*encryptionKeyFile)
			if kerr != nil {
				logger.ErrorContext(context.Background(), "failed to read encryption key file — check that the file exists and is readable", "worker_id", workerID, "file", *encryptionKeyFile, "error", kerr)
				os.Exit(1)
			}
			keyStr := strings.TrimSpace(string(keyData))
			pe, perr := engine.NewPayloadEncryption(keyStr)
			if perr != nil {
				logger.ErrorContext(context.Background(), "invalid encryption key — expected a base64-encoded 256-bit AES key", "worker_id", workerID, "error", perr)
				os.Exit(1)
			}
			payloadEncryption = pe
			logger.InfoContext(context.Background(), "encryption at rest enabled for sensitive payload fields", "worker_id", workerID)
		}
		// Build stores, DB connections, and closers for each shard.
		stores := make([]engine.WorkflowStore, len(configs))
		closers := make([]func() error, len(configs))
		shardDBs := make([]*sql.DB, len(configs))
		shardFactories := make([]engine.StoreFactory, 0, len(configs))
		for i, cfg := range configs {
			dsn := cfg.ConnStr
			if cfg.Schema != "" && cfg.Schema != "public" && !strings.Contains(dsn, "search_path=") {
				sep := "?"
				if strings.Contains(dsn, "?") {
					sep = "&"
				}
				dsn = dsn + sep + "search_path=" + cfg.Schema
			}
			sdb, err := sql.Open("postgres", dsn)
			if err != nil {
				logger.ErrorContext(context.Background(), "shard open failed", "worker_id", workerID, "shard", cfg.Name, "error", err)
				os.Exit(1)
			}
			if err := sdb.PingContext(ctx); err != nil {
				sdb.Close()
				logger.ErrorContext(ctx, "shard ping failed", "worker_id", workerID, "shard", cfg.Name, "error", err)
				os.Exit(1)
			}
			sdb.SetMaxOpenConns(15)
			sdb.SetMaxIdleConns(5)
			sdb.SetConnMaxLifetime(5 * time.Minute)

			shardDBs[i] = sdb
			f := engine.NewPostgresStoreFactory(sdb, cfg.Schema)
			f.WithLogger(logger)
			if payloadEncryption != nil {
				f.WithEncryption(payloadEncryption, *encryptSensitivePayloads)
			}
			shardFactories = append(shardFactories, f)
			s, closer, err := f.OpenStore(ctx, defaultTenantID, taskQueues...)
			if err != nil {
				sdb.Close()
				logger.ErrorContext(context.Background(), "shard open store failed", "worker_id", workerID, "shard", cfg.Name, "error", err)
				os.Exit(1)
			}
			stores[i] = s
			closers[i] = closer.Close
		}

		shardedStore, err := engine.NewShardedStore(configs, stores, closers)
		if err != nil {
			logger.ErrorContext(context.Background(), "failed to create sharded store", "worker_id", workerID, "error", err)
			os.Exit(1)
		}
		store = shardedStore

		// Span every shard, not just the first. This used to be `factory = f`
		// under `if i == 0`, which was harmless while the factory was only used
		// for background work but would have narrowed every tenant-scoped HTTP
		// request to shard 0 once handlers started opening stores from it.
		factory = &shardedStoreFactory{configs: configs, factories: shardFactories}
		defer shardedStore.Close()

		// Use the first shard's database for plugin migrations and
		// administration. Plugin tables (event_subscriptions,
		// webhook_sources, etc.) live on this shard.
		if len(shardDBs) > 0 {
			db = shardDBs[0]
		}
		// Create plugin-dedicated connection pool from the first shard's DSN.
		if *maxPluginConnections > 0 && len(configs) > 0 {
			pdb, pErr := sql.Open("postgres", configs[0].ConnStr)
			if pErr != nil {
				logger.WarnContext(context.Background(), "failed to open plugin connection pool", "worker_id", workerID, "error", pErr)
			} else {
				pluginDB = pdb
				pluginDB.SetMaxOpenConns(*maxPluginConnections)
				pluginDB.SetMaxIdleConns(max(1, *maxPluginConnections/2))
				pluginDB.SetConnMaxLifetime(5 * time.Minute)
				metricsInstance.SetPluginConnectionsMax(context.Background(), int64(*maxPluginConnections))
				defer pluginDB.Close()
				logger.InfoContext(context.Background(), "plugin DB pool created", "worker_id", workerID, "max_connections", *maxPluginConnections)
			}
		}
		// Start idempotency key cleanup on each shard. Sharding is a
		// PostgreSQL configuration -- shardDBs come from the postgres
		// connection strings above -- so the driver is named explicitly rather
		// than inherited from *driver, which this branch does not consult.
		for _, sdb := range shardDBs {
			go idempotencyCleanupLoop(ctx, sdb, "postgres", 1*time.Hour)
		}
	} else {
		// Resolve DB connection string via the configured credential provider.
		credProvider, credErr := engine.NewDBCredentialProvider(*dbCredentialProvider, *dbURL, *dbCredentialPath)
		if credErr != nil {
			logger.ErrorContext(context.Background(), "credential provider error — check that the provider is configured correctly", "worker_id", workerID, "provider", *dbCredentialProvider, "error", credErr)
			os.Exit(1)
		}
		resolvedURL, credErr := credProvider.GetConnectionString(ctx)
		if credErr != nil {
			logger.ErrorContext(context.Background(), "failed to resolve database credentials — check the credential path or provider configuration", "worker_id", workerID, "provider", *dbCredentialProvider, "path", *dbCredentialPath, "error", credErr)
			os.Exit(1)
		}
		*dbURL = resolvedURL
		if *dbURL == "" {
			*dbURL = os.Getenv("DATABASE_URL")
		}
		if *dbURL == "" {
			fmt.Fprintln(os.Stderr, "error: the --db flag or DATABASE_URL environment variable must be set to a database connection string")
			os.Exit(1)
		}

		sqlDriver := sqlDriverName(*driver)
		dbDSN := dsnWithSchema(*dbURL, *schemaName)

		var err error
		switch *driver {
		case "postgres":
			db, err = sql.Open(sqlDriver, dbDSN)
			if err != nil {
				logger.ErrorContext(context.Background(), "failed to connect to database", "worker_id", workerID, "error", err)
				os.Exit(1)
			}
			defer db.Close()
			db.SetMaxOpenConns(*concurrency + 5)
			db.SetMaxIdleConns(max(10, *concurrency/2))
			db.SetConnMaxLifetime(5 * time.Minute)
			factory = engine.NewPostgresStoreFactory(db, *schemaName).WithNotifyChannel(*notifyChannel).WithLogger(logger)

			// NO PER-TENANT POOLS ARE BUILT HERE, ON ANY DIALECT, AND THAT IS
			// CORRECT. cleat#1307.
			//
			// PostgreSQL does not need them: set_config('cleat.tenant_id', ...)
			// per transaction gives RLS what it needs on the owner pool.
			//
			// MySQL does not get them because it is single-tenant by decision --
			// tiers.yaml, "DECIDED 2026-09-03: MySQL is single-tenant only, and a
			// second, very different implementation of multi-tenancy is not worth
			// building", enforced by migrations/mysql/038_single_tenant_guard.sql.
			//
			// WHAT USED TO BE HERE, because the obvious repair is the dangerous
			// one. This block read
			//
			//	if *driver != "postgres" && *requireAuth { tenantPools = ... }
			//
			// inside this `case "postgres":` arm, so reaching it required
			// *driver == "postgres" while it tested the opposite: unreachable, and
			// tenantPools was nil on every dialect. The tempting fix is to move it
			// out of the switch so MySQL and MSSQL reach it. That would be worse
			// than the dead code, because plugin.TenantPools is PostgreSQL-ONLY in
			// its implementation -- plugin/tenant_db.go opens `sql.Open("postgres",
			// ...)` against a libpq keyword DSN with $1 placeholders. A MySQL
			// worker would get a postgres connection builder.
			//
			// So the guard selected one dialect and the body served the other, and
			// the comment above it asserted a third thing. Whether to build tenant
			// pools for PostgreSQL, or retire plugin.TenantPools, is open in
			// cleat#1307; it is not something this line can decide.
			// TestTheWorkerBuildsNoTenantPools pins the current answer.

			// Create plugin-dedicated connection pool.
			if *maxPluginConnections > 0 {
				pluginDB, err = sql.Open(sqlDriver, dbDSN)
				if err != nil {
					logger.ErrorContext(context.Background(), "failed to open plugin connection pool", "worker_id", workerID, "error", err)
					os.Exit(1)
				}
				pluginDB.SetMaxOpenConns(*maxPluginConnections)
				pluginDB.SetMaxIdleConns(max(1, *maxPluginConnections/2))
				pluginDB.SetConnMaxLifetime(5 * time.Minute)
				metricsInstance.SetPluginConnectionsMax(context.Background(), int64(*maxPluginConnections))
				defer pluginDB.Close()
				logger.InfoContext(context.Background(), "plugin DB pool configured", "worker_id", workerID, "max_connections", *maxPluginConnections)
			}
		case "mysql":
			db, err = sql.Open(sqlDriver, *dbURL)
			if err != nil {
				logger.ErrorContext(context.Background(), "failed to connect to database", "worker_id", workerID, "error", err)
				os.Exit(1)
			}
			defer db.Close()
			db.SetMaxOpenConns(*concurrency + 5)
			db.SetMaxIdleConns(5)
			db.SetConnMaxLifetime(5 * time.Minute)
			factory = engine.NewMySQLStoreFactory(db, mysqlBaseDSN(*dbURL)).WithTenantPoolMaxConns(*tenantPoolMaxConns).WithLogger(logger)

			// Create plugin-dedicated connection pool.
			if *maxPluginConnections > 0 {
				pluginDB, err = sql.Open(sqlDriver, *dbURL)
				if err != nil {
					logger.ErrorContext(context.Background(), "failed to open plugin connection pool", "worker_id", workerID, "error", err)
					os.Exit(1)
				}
				pluginDB.SetMaxOpenConns(*maxPluginConnections)
				pluginDB.SetMaxIdleConns(max(1, *maxPluginConnections/2))
				pluginDB.SetConnMaxLifetime(5 * time.Minute)
				metricsInstance.SetPluginConnectionsMax(context.Background(), int64(*maxPluginConnections))
				defer pluginDB.Close()
				logger.InfoContext(context.Background(), "plugin DB pool configured", "worker_id", workerID, "max_connections", *maxPluginConnections)
			}
		case "mssql":
			factory = engine.NewMSSQLStoreFactory(*dbURL).WithTenantPoolMaxConns(*tenantPoolMaxConns).WithLogger(logger)
			// Open a connection to verify and for plugin/migration use.
			db, err = sql.Open(sqlDriver, *dbURL)
			if err != nil {
				logger.ErrorContext(context.Background(), "failed to connect to database", "worker_id", workerID, "error", err)
				os.Exit(1)
			}
			defer db.Close()
			db.SetMaxOpenConns(*concurrency + 5)
			db.SetMaxIdleConns(5)
			db.SetConnMaxLifetime(5 * time.Minute)

			// Create plugin-dedicated connection pool.
			if *maxPluginConnections > 0 {
				pluginDB, err = sql.Open(sqlDriver, *dbURL)
				if err != nil {
					logger.ErrorContext(context.Background(), "failed to open plugin connection pool", "worker_id", workerID, "error", err)
					os.Exit(1)
				}
				pluginDB.SetMaxOpenConns(*maxPluginConnections)
				pluginDB.SetMaxIdleConns(max(1, *maxPluginConnections/2))
				pluginDB.SetConnMaxLifetime(5 * time.Minute)
				metricsInstance.SetPluginConnectionsMax(context.Background(), int64(*maxPluginConnections))
				defer pluginDB.Close()
				logger.InfoContext(context.Background(), "plugin DB pool configured", "worker_id", workerID, "max_connections", *maxPluginConnections)
			}
		default:
			logger.ErrorContext(context.Background(), "invalid driver", "worker_id", workerID, "driver", *driver)
			os.Exit(1)
		}

		// Load encryption key if configured.
		if *encryptSensitivePayloads {
			if *driver != "postgres" {
				logger.ErrorContext(context.Background(), "--encrypt-sensitive-payloads requires --driver=postgres", "worker_id", workerID)
				os.Exit(1)
			}
			if *encryptionKeyFile == "" {
				logger.ErrorContext(context.Background(), "--encrypt-sensitive-payloads requires --encryption-key-file", "worker_id", workerID)
				os.Exit(1)
			}
		}
		if *encryptionKeyFile != "" {
			keyData, kerr := os.ReadFile(*encryptionKeyFile)
			if kerr != nil {
				logger.ErrorContext(context.Background(), "failed to read encryption key file", "worker_id", workerID, "error", kerr)
				os.Exit(1)
			}
			keyStr := strings.TrimSpace(string(keyData))
			pe, perr := engine.NewPayloadEncryption(keyStr)
			if perr != nil {
				logger.ErrorContext(context.Background(), "invalid encryption key", "worker_id", workerID, "error", perr)
				os.Exit(1)
			}
			payloadEncryption = pe
			logger.InfoContext(context.Background(), "encryption at rest enabled for sensitive payload fields", "worker_id", workerID)
		}

		// Propagate encryption to the store factory.
		if pgFactory, ok := factory.(*engine.PostgresStoreFactory); ok && payloadEncryption != nil {
			pgFactory.WithEncryption(payloadEncryption, *encryptSensitivePayloads)
		}

		// Load encryption key if configured.
		if *encryptSensitivePayloads {
			if *driver != "postgres" {
				log.Fatalf("[worker %s] --encrypt-sensitive-payloads requires --driver=postgres (MySQL and MSSQL are not yet supported for encryption at rest)", workerID)
			}
			if *encryptionKeyFile == "" {
				log.Fatalf("[worker %s] --encrypt-sensitive-payloads requires --encryption-key-file", workerID)
			}
		}
		if *encryptionKeyFile != "" {
			keyData, kerr := os.ReadFile(*encryptionKeyFile)
			if kerr != nil {
				log.Fatalf("[worker %s] Failed to read encryption key file %s: %v", workerID, *encryptionKeyFile, kerr)
			}
			keyStr := strings.TrimSpace(string(keyData))
			pe, perr := engine.NewPayloadEncryption(keyStr)
			if perr != nil {
				log.Fatalf("[worker %s] Invalid encryption key — expected a base64-encoded 256-bit AES key: %v", workerID, perr)
			}
			payloadEncryption = pe
			logger.InfoContext(context.Background(), "encryption at rest enabled for sensitive payload fields", "worker_id", workerID)
		}

		// Propagate encryption to the store factory.
		if pgFactory, ok := factory.(*engine.PostgresStoreFactory); ok && payloadEncryption != nil {
			pgFactory.WithEncryption(payloadEncryption, *encryptSensitivePayloads)
		}

		s, _, err := factory.OpenStore(ctx, defaultTenantID, taskQueues...)
		if err != nil {
			logger.ErrorContext(context.Background(), "failed to open database store — check that the database is accessible and the schema exists", "worker_id", workerID, "error", err)
			os.Exit(1)
		}
		store = s

		// Start periodic cleanup of expired idempotency keys. Every dialect:
		// the postgres-only guard that used to stand here left MySQL and SQL
		// Server with nothing that ever removed a key (cleat#1256).
		go idempotencyCleanupLoop(ctx, db, *driver, 1*time.Hour)

	}

	// ---- Plugin loading (always, regardless of --api-addr) ----
	if *apiAddr != "" {
		plugMux = http.NewServeMux()
	}

	var rawPluginConfig []byte
	if *pluginConfigFile != "" {
		data, ferr := os.ReadFile(*pluginConfigFile)
		if ferr != nil {
			logger.ErrorContext(context.Background(), "failed to read plugin config file", "worker_id", workerID, "file", *pluginConfigFile, "error", ferr)
			os.Exit(1)
		}
		data = []byte(os.ExpandEnv(string(data)))
		if json.Valid(data) {
			rawPluginConfig = data
		} else {
			logger.ErrorContext(context.Background(), "plugin config must be valid JSON", "worker_id", workerID, "file", *pluginConfigFile)
			os.Exit(1)
		}
	}

	pluginEnv := &plugin.Environment{
		DB:      getPluginDB(db, pluginDB, plugin.Dialect(factory.Dialect())),
		Mux:     plugMux,
		Config:  rawPluginConfig,
		Logger:  slog.Default(),
		Done:    ctx.Done(),
		Dialect: plugin.Dialect(factory.Dialect()),
		StartWorkflow: func(ctx context.Context, defName string, input json.RawMessage) (string, error) {
			versions, err := store.ListVersions(ctx, defName)
			if err != nil {
				return "", fmt.Errorf("start workflow %s: %w", defName, err)
			}
			if len(versions) == 0 {
				return "", fmt.Errorf("start workflow %s: no versions deployed", defName)
			}
			runID, _, err := store.StartNewRun(ctx, "", defName, versions[0], input, "", engine.DefaultTenantUUID, 0)
			return runID, err
		},

		SignalWorkflow: func(ctx context.Context, workflowID, signalName, payload string) error {
			return store.DeliverSignal(ctx, workflowID, signalName, payload)
		},
	}

	var err error
	plugList, err = plugin.Discover()
	if err != nil {
		logger.ErrorContext(context.Background(), "plugin initialization failed — check that all plugin binaries are present and compatible", "worker_id", workerID, "error", err)
		os.Exit(1)
	}

	// Run core schema migrations before plugin migrations.
	//
	// On a separate connection when --migrate-db is set: --db may be an
	// unprivileged role (see migrations/postgres/005_app_role.sql), which is
	// what makes it subject to row-level security, and such a role cannot
	// run DDL. Falls back to db so an unsplit deployment behaves as before.
	migrateDB := db
	if *migrateDBURL != "" {
		mdb, mErr := sql.Open(sqlDriverName(*driver), dsnWithSchema(*migrateDBURL, *schemaName))
		if mErr != nil {
			logger.ErrorContext(context.Background(), "failed to connect to the migration database (--migrate-db)", "worker_id", workerID, "error", mErr)
			os.Exit(1)
		}
		// Migrations are serialised by an advisory lock and run once at boot;
		// a couple of connections is plenty, and this pool must not compete
		// with the runtime one.
		mdb.SetMaxOpenConns(2)
		defer mdb.Close()
		migrateDB = mdb
	}

	migrator := migration.NewRunner(migrateDB, migration.Dialect(factory.Dialect()), "migrations")
	if err := migrator.Run(ctx); err != nil {
		logger.ErrorContext(context.Background(), "core database migrations failed — check that the database user has CREATE/ALTER privileges (see --migrate-db)", "worker_id", workerID, "error", err)
		os.Exit(1)
	}

	if err := plugin.RunMigrations(ctx, migrateDB, plugin.Dialect(factory.Dialect()), nil, plugList); err != nil {
		logger.ErrorContext(context.Background(), "plugin database migrations failed — check plugin logs for details", "worker_id", workerID, "error", err)
		os.Exit(1)
	}

	// Now that the schema (and its policies) exist, check that the *runtime*
	// connection is actually subject to them. See engine.CheckRLSEnforced:
	// GetWorkflowByID and ListWorkflows have no application-level tenant
	// filter, so on a connection that bypasses RLS they return every tenant's
	// data. Every configuration cleat shipped connected as a superuser.
	if *driver == "postgres" && *rlsCheck != "off" {
		reasons, rErr := engine.CheckRLSEnforced(ctx, db)
		switch {
		case rErr != nil:
			// Could not tell. Refusing on an inconclusive check would make an
			// unrelated database hiccup fatal, so this is reported and the
			// worker continues.
			logger.WarnContext(context.Background(), "could not verify row-level security enforcement", "worker_id", workerID, "error", rErr)
		case len(reasons) == 0:
			logger.InfoContext(context.Background(), "row-level security is enforced on this connection", "worker_id", workerID)
		case *rlsCheck == "require" || (*rlsCheck == "auto" && *requireAuth):
			logger.ErrorContext(context.Background(), "refusing to start: "+engine.FormatRLSBypass(reasons), "worker_id", workerID)
			os.Exit(1)
		default:
			logger.WarnContext(context.Background(), engine.FormatRLSBypass(reasons), "worker_id", workerID)
		}
	}

	// For MySQL, the factory creates a per-tenant database that needs its
	// own copy of the schema. Run core and plugin migrations on it.
	if *driver == "mysql" {
		if mf, ok := factory.(*engine.MySQLStoreFactory); ok {
			tenantDB, terr := mf.TenantDB(ctx, defaultTenantID)
			if terr != nil {
				logger.ErrorContext(context.Background(), "failed to get tenant database", "worker_id", workerID, "error", terr)
				os.Exit(1)
			}
			tm := migration.NewRunner(tenantDB, migration.Dialect(factory.Dialect()), "migrations")
			if terr = tm.Run(ctx); terr != nil {
				logger.ErrorContext(context.Background(), "tenant core migrations failed", "worker_id", workerID, "error", terr)
				os.Exit(1)
			}
			if terr = plugin.RunMigrations(ctx, tenantDB, plugin.Dialect(factory.Dialect()), nil, plugList); terr != nil {
				logger.ErrorContext(context.Background(), "tenant plugin migrations failed", "worker_id", workerID, "error", terr)
				os.Exit(1)
			}
		}
	}

	// Initialize plugins with per-plugin DB access control.
	// Each plugin receives a copy of the environment with its DB handle
	// wrapped (or nil) according to its declared DatabaseAccess level.
	for _, lp := range plugList {
		if !lp.Healthy {
			continue
		}
		envCopy := *pluginEnv
		switch lp.Plugin.Info().DatabaseAccess {
		case plugin.DatabaseAccessNone:
			envCopy.DB = nil
		case plugin.DatabaseAccessReadOnly:
			envCopy.DB = getPluginReadOnlyDB(db, pluginDB, plugin.Dialect(factory.Dialect()))
		default: // DatabaseAccessReadWrite or empty (backward compat)
			envCopy.DB = getPluginDB(db, pluginDB, plugin.Dialect(factory.Dialect()))
		}
		// Wrap SignalWorkflow with signal authorization.
		// The plugin name is the caller identity checked against allowed_signals.
		if *requireSignalAuth {
			pluginName := lp.Plugin.Info().Name
			envCopy.SignalWorkflow = func(ctx context.Context, workflowID, signalName, payload string) error {
				callers, err := store.GetAllowedSignalCallers(ctx, workflowID)
				if err != nil {
					return err
				}
				if !signalCallerAllowed(callers, pluginName) {
					return fmt.Errorf("signal auth denied: %s not in allowed_signals of %s", pluginName, workflowID)
				}
				return store.DeliverSignal(ctx, workflowID, signalName, payload)
			}
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					lp.Healthy = false
					lp.Error = fmt.Errorf("panic during Init: %v", r)
					logger.ErrorContext(context.Background(), "plugin init panicked", "worker_id", workerID, "plugin", lp.Plugin.Info().Name, "error", r)
				}
			}()
			if err := lp.Plugin.Init(ctx, &envCopy); err != nil {
				lp.Healthy = false
				lp.Error = err
				logger.ErrorContext(context.Background(), "plugin init failed", "worker_id", workerID, "plugin", lp.Plugin.Info().Name, "error", err)
			}
		}()
	}

	for _, lp := range plugList {
		if !lp.Healthy {
			continue
		}
		if p, ok := lp.Plugin.(plugin.HasRoutes); ok && plugMux != nil {
			if rerr := p.RegisterRoutes(plugMux); rerr != nil {
				logger.ErrorContext(context.Background(), "plugin route registration failed", "worker_id", workerID, "plugin", lp.Plugin.Info().Name, "error", rerr)
			}
		}
	}

	if plugMux != nil {
		plugHandler = plugMux
		for _, lp := range plugList {
			if !lp.Healthy {
				continue
			}
			if p, ok := lp.Plugin.(plugin.HasMiddleware); ok {
				plugHandler = p.Middleware(plugHandler)
			}
		}
	}

	for _, lp := range plugList {
		if !lp.Healthy {
			continue
		}
		if p, ok := lp.Plugin.(plugin.HasHostFunctions); ok {
			adapter := &hostPluginRegistryAdapter{
				registry:       pluginRegistry,
				streamRegistry: pluginStreamRegistry,
				pluginName:     lp.Plugin.Info().Name,
			}
			if rerr := p.RegisterHostFunctions(adapter); rerr != nil {
				logger.ErrorContext(context.Background(), "plugin host functions failed", "worker_id", workerID, "plugin", lp.Plugin.Info().Name, "error", rerr)
			}
		}
	}

	for _, lp := range plugList {
		if !lp.Healthy {
			continue
		}
		if p, ok := lp.Plugin.(plugin.HasBackground); ok {
			bgWg.Add(1)
			go func(bg plugin.HasBackground) {
				defer bgWg.Done()
				runPluginBackground(ctx, logger, workerID, bg)
			}(p)
		}
	}

	// Load custom redaction patterns from file (if configured).
	if *redactPatternsFile != "" {
		if err := engine.LoadRedactPatterns(*redactPatternsFile); err != nil {
			logger.ErrorContext(context.Background(), "failed to load redact patterns — check that the file exists and contains valid patterns", "worker_id", workerID, "file", *redactPatternsFile, "error", err)
			os.Exit(1)
		}
	}

	// Start plugin connection pool monitor (only when a separate pool exists).
	if pluginDB != nil {
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					stats := pluginDB.Stats()
					metricsInstance.SetPluginConnectionsInUse(context.Background(), int64(stats.InUse))
					openConns := stats.OpenConnections
					if openConns > 0 && *maxPluginConnections > 0 && float64(openConns) > 0.8*float64(*maxPluginConnections) {
						logger.WarnContext(context.Background(), "plugin DB connections near limit", "worker_id", workerID, "used", openConns, "max", *maxPluginConnections, "pct", 100*float64(openConns)/float64(*maxPluginConnections))
					}
				}
			}
		}()
	}

	// Create disk-backed compiled WASM module cache.
	wasmDiskCache := engine.NewWasmDiskCache(*wasmCacheDir, *wasmDiskCacheMaxFiles)
	if wasmDiskCache != nil {
		logger.InfoContext(context.Background(), "WASM disk cache configured", "worker_id", workerID, "dir", *wasmCacheDir, "max_files", *wasmDiskCacheMaxFiles)
	}

	// Register the wasmtime backend. wasmtime is the only WASM backend cleat
	// has: there is no fallback to fall back to, so a construction failure
	// is fatal. See CLAUDE.md and IMPROVEMENT-PLAN.md 3.30 for why the
	// former wazero fallback was removed rather than kept as a degraded
	// option -- it could not fence a compute-bound guest, so silently
	// running on it was a safety-relevant degradation no operator asked for.
	//
	// Bound wasmtime execution: epoch interruption (wall-clock, primary
	// defense against a runaway workflow hanging the worker — see
	// IMPROVEMENT-PLAN.md 1.5) and StoreLimits (memory/table/instance
	// ceilings), plus optional fuel-based instruction metering when
	// --wasm-instruction-limit is set.
	wasmtimeMemoryLimitBytes := int64(0) // 0 => wasmtimeBackend applies its own default
	if *wasmMemoryMaxMB > 0 {
		wasmtimeMemoryLimitBytes = int64(*wasmMemoryMaxMB) * 1024 * 1024
	}
	wt, wasmtimeErr := engine.NewWasmtimeBackend(ctx,
		engine.WithWasmtimeExecutionTimeout(*wasmInstanceTimeout),
		engine.WithWasmtimeInstructionLimit(uint64(*wasmInstructionLimit)),
		engine.WithWasmtimeMemoryLimits(wasmtimeMemoryLimitBytes, 0, 0),
		engine.WithWasmtimeDeferBudget(*wasmDeferBudget),
		// Without this the backend writes to slog.Default(), and this worker's
		// configured handler never sees the one record that says whether a
		// KILLED workflow's defers ran. See WithWasmtimeLogger.
		engine.WithWasmtimeLogger(logger),
	)
	if wasmtimeErr != nil {
		logger.ErrorContext(context.Background(), "wasmtime backend failed to initialize; wasmtime is the only WASM backend cleat has, there is no fallback", "worker_id", workerID, "error", wasmtimeErr)
		os.Exit(1)
	}
	// A ceiling below the instance timeout is a configuration that cannot mean
	// what it says: configureStore clamps the epoch deadline to whatever the
	// context has left, so the guest's execution bound silently becomes the
	// ceiling and --wasm-instance-timeout stops being the number that decides.
	if *wasmWallClockCeiling > 0 && *wasmWallClockCeiling < *wasmInstanceTimeout {
		logger.WarnContext(context.Background(),
			"--wasm-wall-clock-ceiling is below --wasm-instance-timeout, so the guest's execution bound is effectively the ceiling",
			"worker_id", workerID,
			"wall_clock_ceiling", *wasmWallClockCeiling,
			"instance_timeout", *wasmInstanceTimeout)
	}

	var wasmtimeBackend engine.WasmBackend = wt
	logger.InfoContext(context.Background(), "wasmtime backend registered for Go WASM", "worker_id", workerID, "instance_timeout", *wasmInstanceTimeout, "wall_clock_ceiling", *wasmWallClockCeiling, "instruction_limit", *wasmInstructionLimit, "memory_limit_bytes", wasmtimeMemoryLimitBytes, "defer_budget", *wasmDeferBudget)

	// Start PostgreSQL NOTIFY listener for low-latency dispatch wake-up.
	var notifyCh chan struct{}
	var closeNotify func()
	if *driver == "postgres" && *notifyChannel != "" {
		notifyCh = make(chan struct{}, 1)
		closeNotify = startNotifyListener(*dbURL, *notifyChannel, notifyCh, logger)
		defer closeNotify()
	}

	// Set up per-tenant adaptive flusher registry if batch flushing is not disabled.
	var flusherRegistry *engine.TenantFlusherRegistry
	var flusherDB *sql.DB
	if !*batchFlushDisabled && !*noPerStepFlush {
		// Open a dedicated DB pool for the adaptive flusher so batch flushes
		// never queue behind workflow claims, history loads, or finalizations.
		flusherDB, err = sql.Open(sqlDriverName(*driver), dsnWithSchema(*dbURL, *schemaName))
		if err != nil {
			logger.ErrorContext(ctx, "failed to open flusher DB pool", "worker_id", workerID, "error", err)
			os.Exit(1)
		}
		flusherDB.SetMaxOpenConns(*batchFlushMaxConns)
		flusherDB.SetMaxIdleConns(max(1, *batchFlushMaxConns/2))
		flusherDB.SetConnMaxLifetime(5 * time.Minute)
		registry := engine.NewTenantFlusherRegistry(flusherDB, engine.FlusherConfig{
			MaxWait:        time.Duration(*batchFlushMaxWaitMs) * time.Millisecond,
			MaxBatch:       *batchFlushMaxSize,
			EnterThreshold: float64(*batchFlushEnterRate),
			ExitThreshold:  float64(*batchFlushExitRate),
		})
		registry.SetEncryption(*encryptSensitivePayloads, payloadEncryption)
		flusherRegistry = registry
		logger.InfoContext(ctx, "adaptive flusher registry enabled", "worker_id", workerID, "max_wait_ms", *batchFlushMaxWaitMs, "max_batch", *batchFlushMaxSize, "enter_rate", *batchFlushEnterRate, "exit_rate", *batchFlushExitRate)
	}
	w := &Worker{
		Metrics:                          metricsInstance,
		id:                               workerID,
		logger:                           logger,
		store:                            store,
		storeTenantID:                    defaultTenantID,
		storeFactory:                     factory,
		taskQueues:                       taskQueues,
		claimAcrossTenants:               *claimAcrossTenants,
		concurrency:                      *concurrency,
		maxQueued:                        *maxQueued,
		heartbeatInterval:                *heartbeatInterval,
		pollInterval:                     *pollInterval,
		ctx:                              ctx,
		cancel:                           cancel,
		wasmCache:                        newWasmLRUCache(*wasmCacheMaxEntries, *wasmCacheMaxMB),
		scheduleInterval:                 15 * time.Second,
		compactionThreshold:              *compactionThreshold,
		compactionInterval:               *compactionInterval,
		retentionInterval:                *retentionInterval,
		versionGCInterval:                *versionGCInterval,
		versionGCMinVersions:             *versionGCMinVersions,
		versionGCMaxAge:                  *versionGCMaxAge,
		pluginRegistry:                   pluginRegistry,
		pluginStreamRegistry:             pluginStreamRegistry,
		plugList:                         plugList,
		tenantPools:                      tenantPools,
		memorySampleRetention:            *memorySampleRetention,
		retentionDays:                    *retentionDays,
		deadLetterRetentionDays:          *deadLetterRetentionDays,
		completedWorkflowRetentionDays:   *completedWorkflowRetentionDays,
		schemaName:                       *schemaName,
		disableChecksumVerification:      disableChecksumVerification,
		requireSignalAuth:                requireSignalAuth,
		maxRetries:                       *maxRetries,
		wasmMemoryMaxMB:                  wasmMemoryMaxMB,
		wasmInstructionLimit:             wasmInstructionLimit,
		wasmInstanceTimeout:              *wasmInstanceTimeout,
		wasmCumulativeAllocationMaxBytes: int64(*wasmCumulativeAllocationMaxMB) * 1024 * 1024,
		wasmDiskCache:                    wasmDiskCache,
		wasmtimeBackend:                  wasmtimeBackend,
		maxQuotaEvents:                   *maxQuotaEvents,
		maxQuotaChildren:                 *maxQuotaChildren,
		maxQuotaConcurrencyKeys:          *maxQuotaConcurrencyKeys,
		maxQuotaSchedules:                *maxQuotaSchedules,
		maxWorkflowDuration:              *maxWorkflowDuration,
		wasmWallClockCeiling:             *wasmWallClockCeiling,
		hostRetryBudget:                  *hostRetryBudget,
		childBindingOverride:             *childBindingOverride,
		healthCheckInterval:              *healthCheckInterval,
		encryption:                       payloadEncryption,
		encryptSensitivePayloads:         *encryptSensitivePayloads,
		drainCh:                          make(chan struct{}),
		parentWakeCh:                     make(chan struct{}, 1),
		notifyCh:                         notifyCh,
		flusherRegistry:                  flusherRegistry,
		db:                               db,
	}

	// Initialize memory-aware concurrency controller.
	monitor := NewMemoryMonitor(*memoryCheckInterval)
	monitor.logger = logger
	mc := NewMemoryController(monitor, store, workerID, *concurrency, *memorySoftLimit, *memoryHardLimit)
	mc.logger = logger
	if err := mc.LoadEstimates(ctx, defaultTenantID); err != nil {
		logger.WarnContext(context.Background(), "failed to load memory estimates", "worker_id", workerID, "error", err)
	}
	w.memoryController = mc
	metricsInstance.RecordDesiredConcurrency(context.Background(), int64(*concurrency))
	globalWorker = w

	// Set metrics on the store factory so stores created during workflow
	// execution inherit the OTel metrics instance.
	if pf, ok := factory.(*engine.PostgresStoreFactory); ok {
		pf.WithMetrics(metricsInstance)
		if syncCommitOff != nil && *syncCommitOff {
			pf.WithSyncCommitOff(true)
		}
	}
	// Start HTTP API server if configured.

	if *apiAddr != "" {
		api := &apiServer{
			store:       store,
			worker:      w,
			maxBodySize: *maxBodySize,
			db:          db,
			factory:     factory,
			taskQueues:  taskQueues,
			requireAuth: *requireAuth,
		}

		// Use plugin mux if available, otherwise create a fresh one.
		mux := plugMux
		if mux == nil {
			mux = http.NewServeMux()
		}

		// Plugin discovery endpoint. Closes over the loaded plugin list, so it
		// is built here and handed to the route table rather than declared in
		// it.
		api.plugins = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			type pluginStatus struct {
				plugin.PluginInfo
				Healthy bool   `json:"healthy"`
				Error   string `json:"error,omitempty"`
			}
			var statuses []pluginStatus
			for _, lp := range plugList {
				ps := pluginStatus{
					PluginInfo: lp.Plugin.Info(),
					Healthy:    lp.Healthy,
				}
				if lp.Error != nil {
					ps.Error = lp.Error.Error()
				}
				statuses = append(statuses, ps)
			}
			json.NewEncoder(w).Encode(statuses)
		})

		// Embedded SPA for non-API paths. registerRoutes wraps it so that an
		// unmatched /api/ path is a JSON 404 instead of index.html.
		webFS, fsErr := fs.Sub(webDist, "web/dist")
		if fsErr != nil {
			logger.WarnContext(context.Background(), "web/dist not found in embedded FS", "worker_id", workerID, "error", fsErr)
		} else {
			fileServer := http.FileServer(http.FS(webFS))
			api.spa = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path := strings.TrimPrefix(r.URL.Path, "/")
				f, ferr := webFS.Open(path)
				if ferr != nil {
					r.URL.Path = "/"
				} else {
					f.Close()
				}
				fileServer.ServeHTTP(w, r)
			})
		}

		// One route table, shared with the tests. See registerRoutes.
		registerRoutes(mux, api)

		// Use plugin middleware chain if available.
		handler := plugHandler
		if handler == nil {
			handler = mux
		}

		// Wrap with auth middleware if --require-auth is true.
		if *requireAuth {
			// S6: these two plugin endpoints are meant to be called by
			// parties who cannot present a cleat API key -- an external
			// webhook sender (plugins/webhookingest, verifies its own
			// HMAC signature) and a third-party IdP's OAuth redirect
			// (plugins/oauthprovider) -- so they must stay reachable
			// without one even though --require-auth wraps the same
			// mux/plugHandler every other plugin route goes through. See
			// auth.Middleware's doc comment for why this is a
			// hand-maintained list rather than something plugins declare
			// themselves.
			// The resolver is built on `db` -- the connection the DSN
			// names -- and NOT on `store`, which is
			// factory.OpenStore(ctx, defaultTenantID, ...) and therefore
			// tenant-scoped.
			//
			// Resolving an API key is what tells you which tenant's store
			// to open, so it cannot run through a store that already
			// knows the tenant. On PostgreSQL and SQL Server that
			// distinction costs nothing -- one database, isolation by RLS
			// or session context, so either connection reaches the table.
			// On MySQL tenant isolation IS a database boundary, so the
			// tenant-scoped store looked in cleat_<tenant> while every
			// writer put the key in the base database, and the API
			// answered 401 to every request with no key that could work.
			// cleat#866.
			authResolver, arErr := auth.NewTenantStoreForDialect(db, *driver)
			if arErr != nil {
				logger.ErrorContext(context.Background(), "cannot build the API key resolver, so no request could be authenticated", "worker_id", workerID, "error", arErr)
				os.Exit(1)
			}
			handler = auth.Middleware(authResolver, true,
				"POST /ingest/{source_id}",
				"GET /oauth/{provider}/callback",
			)(handler)

			// If no API keys exist, auto-generate one for the default tenant.
			//
			// The table is admin.tenant_api_keys on PostgreSQL, and this was
			// the one site that named it unqualified -- every other Postgres
			// caller (engine/store_deployment.go, auth/tenant_store.go) gets
			// it right. The default search_path does not include admin, so
			// this always failed with 42P01 and the only trace was a warning:
			// no startup key was ever generated on a fresh PostgreSQL
			// deployment, while --require-auth defaults to true.
			//
			// SQL Server is the same case, and the previous version of this
			// comment had it wrong: it said "MySQL and SQL Server keep the
			// unqualified name; there the table is not in a separate schema."
			// True of MySQL, which puts each tenant in its own database. Not
			// true of SQL Server -- migrations/mssql/001_schema.sql creates
			// BOTH admin.tenant_api_keys and dbo.tenant_api_keys, an
			// unqualified name resolves to dbo, and dbo is the one nothing
			// writes. So this counted rows in an always-empty table and
			// concluded a key needed generating on every start.
			keyCountQuery := `SELECT COUNT(*) FROM tenant_api_keys`
			if *driver == "postgres" || *driver == "mssql" {
				keyCountQuery = `SELECT COUNT(*) FROM admin.tenant_api_keys`
			}
			var keyCount int
			if err := db.QueryRowContext(ctx, keyCountQuery).Scan(&keyCount); err != nil {
				// ERROR, not WARN: if this query fails the auth middleware
				// reads the same table on every request, so the API is
				// unusable rather than merely missing a convenience.
				logger.ErrorContext(context.Background(),
					"cannot read the API key table; authentication will not work and no startup key will be generated",
					"worker_id", workerID, "query", keyCountQuery, "error", err)
			} else if keyCount == 0 {
				key := auth.GenerateAPIKey()
				defaultTenantID := uuid.MustParse("00000000-0000-0000-0000-000000000000")
				ts, tsErr := auth.NewTenantStoreForDialect(db, *driver)
				switch {
				case tsErr != nil:
					logger.WarnContext(context.Background(), "cannot auto-generate an API key", "worker_id", workerID, "error", tsErr)
				default:
					if err := ts.CreateAPIKey(ctx, defaultTenantID, "auto-generated startup key", key); err != nil {
						logger.WarnContext(context.Background(), "failed to auto-generate API key", "worker_id", workerID, "error", err)
						break
					}
					fmt.Println()
					fmt.Println("=== CLEAT API KEY (auto-generated — no keys were configured) ===")
					fmt.Printf("Key:       %s\n", key)
					fmt.Printf("Tenant ID: %s\n", defaultTenantID)
					fmt.Println()
					fmt.Println("Store this key securely. It will NOT be shown again.")
					fmt.Printf("Use it in the Authorization header: Authorization: Bearer %s\n", key)
					fmt.Println()
				}
			}
		} else {
			logger.WarnContext(context.Background(), "authentication disabled", "worker_id", workerID)
		}

		// Tenant resolver middleware.
		if strings.HasPrefix(*tenantResolver, "header:") {
			headerName := strings.TrimPrefix(*tenantResolver, "header:")
			prev := handler
			handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tidStr := r.Header.Get(headerName); tidStr != "" {
					if tid, err := uuid.Parse(tidStr); err == nil {
						ctx := auth.WithTenantID(r.Context(), tid)
						r = r.WithContext(ctx)
					}
				}
				prev.ServeHTTP(w, r)
			})
		}

		// Create rate limiters and wrap handler.
		// A rate-limit of 0 disables IP-based rate limiting.
		if *rateLimit > 0 {
			ratelim = newIPRateLimiter(rate.Limit(*rateLimit), *rateLimitBurst)
		}
		if *rateLimit > 0 || *rateLimitPerTenant > 0 {
			tenantLim = newKeyedRateLimiter()
			handler = rateLimitMiddleware(ratelim, tenantLim, rate.Limit(*rateLimitPerTenant), *rateLimitPerTenantBurst)(handler)
		}

		srv := &http.Server{
			Addr:         *apiAddr,
			Handler:      handler,
			ReadTimeout:  *httpReadTimeout,
			WriteTimeout: *httpWriteTimeout,
			IdleTimeout:  *httpIdleTimeout,
		}
		go func() {
			logger.InfoContext(context.Background(), "HTTP API listening", "worker_id", workerID, "addr", *apiAddr)
			if err := srv.ListenAndServe(); err != http.ErrServerClosed {
				logger.ErrorContext(context.Background(), "HTTP server error", "worker_id", workerID, "error", err)
			}
		}()
		go func() {
			<-ctx.Done()
			srv.Shutdown(context.Background())
		}()
	}

	// Start pprof server on a separate port for CPU profiling.
	if *pprofAddr != "" {
		go func() {
			logger.InfoContext(context.Background(), "pprof listening", "worker_id", workerID, "addr", *pprofAddr)
			// An explicit Server rather than http.ListenAndServe, for the
			// ReadHeaderTimeout (gosec G114/G112): the convenience function
			// cannot set one, so a client that opens a connection and sends
			// headers slowly pins a goroutine forever.
			//
			// Handler stays nil, i.e. DefaultServeMux, which is where the
			// blank net/http/pprof import registers /debug/pprof. That is
			// deliberate and is why this is on its own opt-in address
			// (--pprof-addr, empty by default) rather than on the API port:
			// the API server above is built with its own mux, so profiling
			// endpoints are not reachable there. Worth keeping that way --
			// a heap profile from this process contains workflow payloads.
			pprofSrv := &http.Server{
				Addr:              *pprofAddr,
				ReadHeaderTimeout: 10 * time.Second,
			}
			if err := pprofSrv.ListenAndServe(); err != nil {
				logger.ErrorContext(context.Background(), "pprof server error", "worker_id", workerID, "error", err)
			}
		}()
	}

	// Handle shutdown signals.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		logger.InfoContext(context.Background(), "shutting down", "worker_id", workerID)
		cancel()
		if ratelim != nil {
			ratelim.stop()
		}
		if tenantLim != nil {
			tenantLim.stop()
		}
	}()

	metricsInstance.SetWorkerCount(context.Background(), 1)
	defer func() {
		metricsInstance.SetWorkerCount(context.Background(), 0)
	}()
	w.Run()

	// Drain the flusher and close its dedicated pool AFTER the engine
	// has stopped so in-flight events are not lost.
	if flusherRegistry != nil {
		flusherRegistry.Shutdown()
	}
	if flusherDB != nil {
		flusherDB.Close()
	}

	// Wait for background workers to finish.
	bgDone := make(chan struct{})
	go func() {
		bgWg.Wait()
		close(bgDone)
	}()
	select {
	case <-bgDone:
		logger.InfoContext(context.Background(), "all background workers stopped", "worker_id", workerID)
	case <-time.After(30 * time.Second):
		logger.WarnContext(context.Background(), "timed out waiting for background workers after 30s", "worker_id", workerID)
	}
	logger.InfoContext(context.Background(), "shutdown complete", "worker_id", workerID)
}

// runPluginBackground runs one plugin's background loop and recovers a panic
// from it.
//
// A panic here used to take the PROCESS with it, and every workflow in flight
// on the worker (cleat#1304). An unrecovered panic in a goroutine cannot be
// caught by the parent, so the recover has to live in the function the
// goroutine runs -- which is why this is a named function rather than an inline
// closure: a closure inside main() cannot be driven by a test, and a guard
// nothing exercises is how the gap lasted.
//
// The worker is hardened against ITS OWN loops panicking -- withPanicRecovery
// at setup.go:875, applied to every one -- and was not hardened against the
// loops it runs on behalf of third-party plugin code, which is the weaker trust
// assumption of the two. Twelve plugins ship a background.go and none contains a
// recover().
//
// NOT routed through withPanicRecovery, and that is a scope decision rather than
// an oversight: that is a method on *Worker, and the Worker is constructed well
// after this spawn in main(). Health-tracker integration, metrics and watchdog
// restart all need it, and getting them requires the plugin loops to be started
// BY the Worker -- a startup-ordering change, tracked separately. This is the
// half that stops the process dying, and it needs nobody to decide anything.
//
// The loop is NOT restarted here. A plugin that panics every iteration would
// spin, and choosing the backoff is part of the same deferred decision. Stopping
// one plugin's background work is a real loss and it is logged as one; losing
// the worker is a larger one.
func runPluginBackground(ctx context.Context, logger *slog.Logger, workerID string, bg plugin.HasBackground) {
	defer func() {
		if r := recover(); r != nil {
			logger.ErrorContext(context.Background(),
				"PANIC in plugin background worker — this plugin's background work has stopped; the worker continues",
				"worker_id", workerID, "plugin", bg.Info().Name,
				"error", r, "stack", string(debug.Stack()))
		}
	}()
	if err := bg.Run(ctx); err != nil {
		logger.ErrorContext(context.Background(), "plugin background worker exited",
			"worker_id", workerID, "plugin", bg.Info().Name, "error", err)
	}
}
