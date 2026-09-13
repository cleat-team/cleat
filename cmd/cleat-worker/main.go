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

	// TENANT ISOLATION IS RESOLVED FIRST, BEFORE ANY ONE-SHOT MODE. cleat#1307.
	//
	// The repo owner's decision was refuse-to-boot, and "boot" includes the
	// administrative modes below. --create-tenant PROVISIONS the tenant's login
	// role when role isolation is configured, so it needs the derivation key --
	// and a --create-tenant that silently created a tenant with no role, under
	// a configuration asking for per-tenant credentials, would leave a tenant
	// that no worker in that deployment can serve: TenantPools.For refuses
	// rather than falling back to the owner pool.
	//
	// Before any connection is opened, so a bad configuration costs nothing.
	tenantMode, tenantSecret, tiErr := resolveTenantIsolation(
		*tenantIsolation, *tenantRoleSecretFile, *driver)
	if tiErr != nil {
		// Surfaced as-is: resolveTenantIsolation's errors name the flag, the
		// value and what to do, and this layer cannot improve on that.
		logger.ErrorContext(context.Background(), "tenant isolation configuration refused",
			"worker_id", workerID, "error", tiErr)
		os.Exit(1)
	}

	// Handle --uninstall-plugin (standalone mode: reverse migrations and exit).
	//
	// cleat#1290. Migration.Down is populated by 18 plugins across 29 sites and
	// was read by nothing; this is its caller. Placed before --create-tenant
	// only because both are one-shot modes and this one is the most
	// destructive, so a reader scanning for "what can this binary do besides
	// run" meets it first.
	//
	// cleat-worker rather than `cleat plugin uninstall` because this is the
	// only binary that links the plugin registry -- plugin.Discover() returns
	// nothing in the others -- and a plugin's Down SQL lives in its compiled
	// Go, not in the database.
	if *uninstallPlugin != "" {
		dbURL := *dbURL
		if dbURL == "" {
			dbURL = os.Getenv("DATABASE_URL")
		}
		if dbURL == "" {
			logger.ErrorContext(context.Background(), "--db or DATABASE_URL required for --uninstall-plugin", "worker_id", workerID)
			os.Exit(1)
		}
		udb, err := sql.Open(sqlDriverName(*driver), dsnWithSchema(dbURL, *schemaName))
		if err != nil {
			logger.ErrorContext(context.Background(), "failed to connect to database", "worker_id", workerID, "error", err)
			os.Exit(1)
		}
		defer udb.Close()

		all, dErr := plugin.Discover()
		if dErr != nil {
			logger.ErrorContext(context.Background(), "failed to discover plugins", "worker_id", workerID, "error", dErr)
			os.Exit(1)
		}
		var target *plugin.LoadedPlugin
		names := make([]string, 0, len(all))
		for _, lp := range all {
			names = append(names, lp.Plugin.Info().Name)
			if lp.Plugin.Info().Name == *uninstallPlugin {
				target = lp
			}
		}
		if target == nil {
			// The available set is printed because a typo is the likeliest
			// reason to land here, and "not found" alone does not help.
			logger.ErrorContext(context.Background(), "no such plugin", "worker_id", workerID,
				"requested", *uninstallPlugin, "available", strings.Join(names, ", "))
			os.Exit(1)
		}

		if *uninstallDryRun {
			// Reports the DECISION without executing, which is the whole point:
			// RunDownMigrations refuses before touching anything, so a dry run
			// that reaches the same refusal tells an operator the real answer.
			fmt.Printf("\n=== DRY RUN: %s ===\n", *uninstallPlugin)
			for _, m := range target.Plugin.(plugin.HasMigrations).Migrations() {
				state := "no Down declared"
				if strings.TrimSpace(m.Down) != "" {
					state = "reversible"
				}
				fmt.Printf("  v%-4d %s\n", m.Version, state)
			}
			fmt.Printf("\nNothing was changed. Re-run without --uninstall-dry-run to reverse.\n\n")
			os.Exit(0)
		}

		res, uErr := plugin.RunDownMigrations(context.Background(), udb,
			plugin.Dialect(*driver), target, all)
		if uErr != nil {
			// Surfaced as-is. RunDownMigrations' errors name which version or
			// which colliding plugin stopped it, and say that nothing changed;
			// this layer cannot improve on that.
			logger.ErrorContext(context.Background(), "uninstall refused", "worker_id", workerID,
				"plugin", *uninstallPlugin, "error", uErr)
			os.Exit(1)
		}
		fmt.Printf("\n=== UNINSTALLED %s ===\n", *uninstallPlugin)
		fmt.Printf("Versions reversed (newest first): %v\n", res.Reversed)
		if len(res.TenantScopedTables) > 0 {
			fmt.Printf("Tables those migrations declared: %s\n", strings.Join(res.TenantScopedTables, ", "))
		}
		fmt.Printf("\n")
		os.Exit(0)
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
		// PROVISION THE ROLE, when role isolation is configured. cleat#1307.
		//
		// admin.create_tenant_role creates the tenant's PostgreSQL login role,
		// its tenant_<uuid> schema, and the cleat.tenant_id role default that
		// makes the connection self-identifying. Nothing called it: it was
		// reachable only from 002_defaults.sql's backfill, which runs at
		// migration time and cannot know a tenant created afterwards.
		//
		// HERE rather than inside auth.CreateTenant, because the password is
		// DERIVED from the worker's key and auth/ has no access to it -- and
		// because admin.tenant_roles.tenant_id REFERENCES admin.tenants, so the
		// role can only be provisioned once the tenant row exists.
		//
		// Only under --tenant-isolation=role. Creating login roles on a
		// deployment that does not use them would leave credentials nobody
		// asked for.
		if tenantMode == isolationRole {
			password, pErr := plugin.TenantRolePassword(tenantSecret, tid.String())
			if pErr != nil {
				logger.ErrorContext(context.Background(), "failed to derive the tenant role password",
					"worker_id", workerID, "tenant_id", tid, "error", pErr)
				os.Exit(1)
			}
			var roleName sql.NullString
			if rErr := gdb.QueryRowContext(context.Background(),
				`SELECT admin.create_tenant_role($1::uuid, $2)`, tid, password).Scan(&roleName); rErr != nil {
				logger.ErrorContext(context.Background(), "failed to provision the tenant role",
					"worker_id", workerID, "tenant_id", tid, "error", rErr)
				os.Exit(1)
			}
			if !roleName.Valid {
				// create_tenant_role RAISEs a warning and returns NULL when the
				// connection cannot CREATE ROLE. Fatal here rather than a
				// warning: the operator asked for role isolation, and a tenant
				// without a role cannot be served under it -- TenantPools.For
				// refuses rather than falling back to the owner pool.
				logger.ErrorContext(context.Background(),
					"tenant role was not created: this connection cannot CREATE ROLE",
					"worker_id", workerID, "tenant_id", tid,
					"hint", "--tenant-isolation=role needs a superuser or CREATEROLE connection")
				os.Exit(1)
			}
			logger.InfoContext(context.Background(), "provisioned tenant role",
				"worker_id", workerID, "tenant_id", tid, "role", roleName.String)
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
		bgPlugins            []plugin.HasBackground
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

			// PER-TENANT POOLS, WHEN --tenant-isolation=role. cleat#1307.
			//
			// This is the decided answer to the question the previous version of
			// this comment left open. What plugin.TenantPools provides is
			// defence in depth: a pool per tenant authenticating AS that
			// tenant's PostgreSQL login role, with cleat.tenant_id set as a
			// ROLE DEFAULT, so the RLS variable arrives with the credential and
			// isolation does not depend on the application remembering to
			// assert who it is. Its own comment: "the connection IS the tenant".
			//
			// POSTGRES ONLY, AND ONLY IN THIS ARM. TenantPools is
			// PostgreSQL-only in its implementation -- plugin/tenant_db.go
			// opens sql.Open("postgres", ...) against a libpq keyword DSN --
			// and resolveTenantIsolation refuses --tenant-isolation=role on any
			// other driver before we get here. Building it inside this arm
			// rather than after the switch is what makes that structural rather
			// than a matter of remembering.
			//
			// WHAT USED TO BE HERE, because the obvious repair was the
			// dangerous one and the next reader will think of it. The block read
			//
			//	if *driver != "postgres" && *requireAuth { tenantPools = ... }
			//
			// inside this `case "postgres":` arm -- so reaching it required
			// *driver == "postgres" while it tested the opposite. Unreachable,
			// and tenantPools was nil on every dialect. Moving it out of the
			// switch, which is what the old comment invited, would have handed a
			// MySQL worker a postgres connection builder.
			//
			// Gated on its own flag rather than on *requireAuth, which is what
			// the dead branch used: --require-auth defaults TRUE, so reusing it
			// would switch a new isolation mechanism on for every existing
			// deployment at upgrade.
			if tenantMode == isolationRole {
				baseDSN := baseDSNFromURL(*dbURL)
				if baseDSN == "" {
					// Refused, not skipped. A nil tenantPools here would fall
					// back to the owner pool for every tenant -- the silent
					// downgrade TenantPools.For was changed to refuse.
					logger.ErrorContext(context.Background(),
						"--tenant-isolation=role could not derive a base DSN from --db",
						"worker_id", workerID)
					os.Exit(1)
				}
				tenantPools = plugin.NewTenantPools(db, baseDSN, *tenantPoolMaxConns, tenantSecret)
				logger.InfoContext(context.Background(),
					"role-per-tenant isolation enabled for plugin host functions",
					"worker_id", workerID, "max_conns_per_tenant", *tenantPoolMaxConns)
			}

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

	// WithSchema, or every unqualified CREATE in migrations/postgres/ lands in
	// public while the runtime pool -- opened through dsnWithSchema above --
	// looks in --schema. That was cleat#1287: the migration run did not even
	// finish, because nineteen files pinned public and twenty-five did not.
	migrator := migration.NewRunner(migrateDB, migration.Dialect(factory.Dialect()), "migrations").
		WithSchema(*schemaName)
	if err := migrator.Run(ctx); err != nil {
		logger.ErrorContext(context.Background(), "core database migrations failed — check that the database user has CREATE/ALTER privileges (see --migrate-db)", "worker_id", workerID, "error", err)
		os.Exit(1)
	}

	// WithSchema, or plugin tables land in public while the runtime pool --
	// opened through dsnWithSchema -- looks in --schema, and every plugin's
	// first query fails with "relation ... does not exist". cleat#1287.
	if err := plugin.RunMigrations(ctx, migrateDB, plugin.Dialect(factory.Dialect()), nil, plugList,
		plugin.WithSchema(*schemaName)); err != nil {
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
			tm := migration.NewRunner(tenantDB, migration.Dialect(factory.Dialect()), "migrations").
				WithSchema(*schemaName)
			if terr = tm.Run(ctx); terr != nil {
				logger.ErrorContext(context.Background(), "tenant core migrations failed", "worker_id", workerID, "error", terr)
				os.Exit(1)
			}
			if terr = plugin.RunMigrations(ctx, tenantDB, plugin.Dialect(factory.Dialect()), nil, plugList,
				plugin.WithSchema(*schemaName)); terr != nil {
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
			// COLLECTED HERE, STARTED BY THE WORKER. They used to be spawned
			// on this line -- 124 lines before the *Worker that owns the
			// health tracker, the background-loop metric and
			// withPanicRecovery exists -- so a panicking plugin loop was
			// invisible to /healthz and to monitoring, and an operator
			// learned about it by reading logs. cleat#1347.
			//
			// Nothing between here and the Worker depends on a plugin loop
			// running: the window loads redaction patterns, starts the plugin
			// pool monitor, builds the WASM disk cache, the wasmtime backend,
			// the NOTIFY listener and the flusher registry. The dependency
			// runs the other way and is satisfied either way -- a plugin's
			// Run(ctx) uses the Environment it was handed at Init, which is
			// complete before this point.
			bgPlugins = append(bgPlugins, p)
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
		maxReclaimPerTick:                *maxReclaimPerTick,
		bgPlugins:                        bgPlugins,
		bgWg:                             &bgWg,
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
