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
	"container/list"
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	_ "github.com/lib/pq"

	"github.com/google/uuid"
	"github.com/rcownie/cleat/internal/auth"
	"github.com/rcownie/cleat/internal/host"
	"github.com/rcownie/cleat/internal/migration"
	"github.com/rcownie/cleat/internal/plugin"
	"golang.org/x/time/rate"

	// Plugins
	_ "github.com/rcownie/cleat/plugins/llm"
	_ "github.com/rcownie/cleat/plugins/pgvector"
)

//go:embed web/dist
var webDist embed.FS

// signalMaxBodySize is the maximum request body size for signal and update
// endpoints (64 KB). General endpoints use the configurable --max-body-size.
const signalMaxBodySize = 65536

// globalWorker is set during worker startup for access from HTTP handlers
// that cannot easily receive a *Worker parameter (e.g. handleMetrics).
var globalWorker *Worker

func main() {
	dbURL := flag.String("db", "", "PostgreSQL connection URL (required)")
	concurrency := flag.Int("concurrency", 10, "Max concurrent workflow executions")
	heartbeatInterval := flag.Duration("heartbeat", 5*time.Second, "Heartbeat interval")
	pollInterval := flag.Duration("poll", 500*time.Millisecond, "Poll interval when no work")
	apiAddr := flag.String("api-addr", "", "HTTP API listen address (e.g., :8080)")
	namespace := flag.String("namespace", "default", "Workflow namespace to claim from")
	taskQueuesStr := flag.String("task-queue", "default", "Comma-separated task queues to poll (e.g. \"default,gpu,high-memory\")")
	compactionThreshold := flag.Int("compaction-threshold", host.DefaultCompactionThreshold, "Number of events before history compaction triggers")
	compactionInterval := flag.Duration("compaction-interval", 5*time.Minute, "Interval between compaction checks")
	shardsFile := flag.String("shards-file", "", "Path to shards JSON config for multi-shard operation")
	pluginConfigFile := flag.String("plugin-config", "", "path to plugin config JSON file")
	memorySoftLimit := flag.Float64("memory-soft-limit", 0.80, "Memory soft limit fraction 0.0-1.0 (stop claiming new work)")
	memoryHardLimit := flag.Float64("memory-hard-limit", 0.95, "Memory hard limit fraction 0.0-1.0 (reject API workflows)")
	memoryCheckInterval := flag.Duration("memory-check-interval", 2*time.Second, "Interval between memory readings")
	memorySampleRetention := flag.Int("memory-sample-retention", 1000, "Max samples per workflow definition")
	requireAuth := flag.Bool("require-auth", true, "Require API key authentication (default: true when --api-addr is set)")
	generateAPIKeyFor := flag.String("generate-api-key", "", "Generate a new API key for the given tenant UUID and exit")
	maxBodySize := flag.Int64("max-body-size", 1048576, "Maximum request body size in bytes (default 1 MiB)")
	httpReadTimeout := flag.Duration("http-read-timeout", 30*time.Second, "HTTP read timeout")
	httpWriteTimeout := flag.Duration("http-write-timeout", 60*time.Second, "HTTP write timeout")
	httpIdleTimeout := flag.Duration("http-idle-timeout", 120*time.Second, "HTTP idle timeout")
	rateLimit := flag.Float64("rate-limit", 100, "Requests/second/IP rate limit (only when --api-addr is set)")
	rateLimitBurst := flag.Int("rate-limit-burst", 200, "Rate limit burst size")
	maxRetries := flag.Int("max-retries", 100, "Maximum retry attempts for DurableCallWithRetry")
	redact := flag.Bool("redact", false, "Enable redaction of sensitive fields in event history (auto-enabled when --require-auth is set)")
	retentionDays := flag.Int("retention-days", 30, "Days to retain completed/failed workflow event history (0 disables)")
	wasmCacheMaxEntries := flag.Int("wasm-cache-max-entries", 100, "Max WASM byte cache entries (LRU eviction)")
	wasmCacheMaxMB := flag.Int("wasm-cache-max-mb", 500, "Max WASM byte cache total size in MB (LRU eviction)")
	flag.Parse()

	// Auto-enable redaction when --require-auth is set (authentication implies
	// sensitive data may be present).
	if *requireAuth {
		*redact = true
	}

	workerID := generateWorkerID()
	log.Printf("[worker %s] Starting with concurrency=%d", workerID, *concurrency)

	// Handle --generate-api-key (standalone mode: generate key and exit).
	if *generateAPIKeyFor != "" {
		tenantID, err := uuid.Parse(*generateAPIKeyFor)
		if err != nil {
			log.Fatalf("invalid tenant UUID for --generate-api-key: %v", err)
		}
		dbURL := *dbURL
		if dbURL == "" {
			dbURL = os.Getenv("DATABASE_URL")
		}
		if dbURL == "" {
			log.Fatalf("--db or DATABASE_URL required for --generate-api-key")
		}
		gdb, err := sql.Open("postgres", dbURL)
		if err != nil {
			log.Fatalf("failed to connect to database: %v", err)
		}
		defer gdb.Close()
		store := auth.NewTenantStore(gdb)
		key := auth.GenerateAPIKey()
		if err := store.CreateAPIKey(context.Background(), tenantID, "generated by --generate-api-key", key); err != nil {
			log.Fatalf("failed to create API key: %v", err)
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

	taskQueues := strings.Split(*taskQueuesStr, ",")

	// Plugin state (populated in both sharded and non-sharded paths).
	var (
		pluginRegistry       = host.NewPluginRegistry()
		pluginStreamRegistry = host.NewPluginStreamRegistry()
		plugList             []*plugin.LoadedPlugin
		plugHandler    http.Handler
		plugMux        *http.ServeMux
		bgWg          sync.WaitGroup
		ratelim       *ipRateLimiter
	)

	var store host.WorkflowStore
	var db *sql.DB
	var tenantPools *plugin.TenantPools
	if *shardsFile != "" {
		configs, err := loadShardConfigs(*shardsFile)
		if err != nil {
			log.Fatalf("[worker %s] Failed to load shards config: %v", workerID, err)
		}
		shardedStore, err := host.NewShardedStore(ctx, configs, taskQueues...)
		if err != nil {
			log.Fatalf("[worker %s] Failed to create sharded store: %v", workerID, err)
		}
		store = shardedStore
		defer shardedStore.Close()

		// Use the first shard's database for plugin migrations and
		// administration. Plugin tables (event_subscriptions,
		// webhook_sources, etc.) live on this shard.
		if len(shardedStore.Shards()) > 0 {
			db = shardedStore.Shards()[0].DB
		}

		// Start idempotency key cleanup on each shard.
		for _, shard := range shardedStore.Shards() {
			go idempotencyCleanupLoop(ctx, shard.DB, 1*time.Hour)
		}
	} else {
		if *dbURL == "" {
			*dbURL = os.Getenv("DATABASE_URL")
		}
		if *dbURL == "" {
			fmt.Fprintln(os.Stderr, "error: the --db flag or DATABASE_URL environment variable must be set to a PostgreSQL connection string")
			os.Exit(1)
		}

		var err error
		db, err = sql.Open("postgres", *dbURL)
		if err != nil {
			log.Fatalf("[worker %s] Failed to connect to database: %v", workerID, err)
		}
		defer db.Close()

		db.SetMaxOpenConns(*concurrency + 5)
		db.SetMaxIdleConns(5)
		db.SetConnMaxLifetime(5 * time.Minute)
		store = host.NewPostgresStore(db, taskQueues...)

		// Start periodic cleanup of expired idempotency keys.
		go idempotencyCleanupLoop(ctx, db, 1*time.Hour)

		// Create per-tenant database connection pools for tenant-scoped plugin operations.
		baseDSN := baseDSNFromURL(*dbURL)
		if baseDSN != "" {
			tenantPools = plugin.NewTenantPools(db, baseDSN)
		}

	}

	// ---- Plugin loading (always, regardless of --api-addr) ----
	if *apiAddr != "" {
		plugMux = http.NewServeMux()
	}

	var rawPluginConfig []byte
	if *pluginConfigFile != "" {
		data, ferr := os.ReadFile(*pluginConfigFile)
		if ferr != nil {
			log.Fatalf("[worker %s] plugin config: %v", workerID, ferr)
		}
		if json.Valid(data) {
			rawPluginConfig = data
		} else {
			log.Fatalf("[worker %s] plugin config: must be valid JSON", workerID)
		}
	}

	pluginEnv := &plugin.Environment{
		DB:     db,
		Mux:    plugMux,
		Config: rawPluginConfig,
		Logger: slog.Default(),
		Done:   ctx.Done(),
		StartWorkflow: func(ctx context.Context, defName string, input json.RawMessage) (string, error) {
			versions, err := store.ListVersions(ctx, defName)
			if err != nil {
				return "", fmt.Errorf("start workflow %s: %w", defName, err)
			}
			if len(versions) == 0 {
				return "", fmt.Errorf("start workflow %s: no versions deployed", defName)
			}
			runID, _, err := store.StartNewRun(ctx, defName, versions[0], input, "")
			return runID, err
		},

		SignalWorkflow: func(ctx context.Context, workflowID, signalName, payload string) error {
			return store.DeliverSignal(ctx, workflowID, signalName, payload)
		},
	}

	var err error
	plugList, err = plugin.Discover()
	if err != nil {
		log.Fatalf("[worker %s] plugin: %v", workerID, err)
	}

		// Run core schema migrations before plugin migrations.
		migrator := migration.NewRunner(db, "migrations")
		if err := migrator.Run(ctx); err != nil {
			log.Fatalf("[worker %s] core migrations: %v", workerID, err)
		}

	if err := plugin.RunMigrations(ctx, db, nil, plugList); err != nil {
		log.Fatalf("[worker %s] plugin migrations: %v", workerID, err)
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
			envCopy.DB = &host.ReadOnlyDB{Inner: db}
		default: // DatabaseAccessReadWrite or empty (backward compat)
			envCopy.DB = db
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					lp.Healthy = false
					lp.Error = fmt.Errorf("panic during Init: %v", r)
					log.Printf("[worker %s] plugin %s init panicked: %v", workerID, lp.Plugin.Info().Name, r)
				}
			}()
			if err := lp.Plugin.Init(ctx, &envCopy); err != nil {
				lp.Healthy = false
				lp.Error = err
				log.Printf("[worker %s] plugin %s init failed: %v", workerID, lp.Plugin.Info().Name, err)
			}
		}()
	}

	for _, lp := range plugList {
		if !lp.Healthy {
			continue
		}
		if p, ok := lp.Plugin.(plugin.HasRoutes); ok && plugMux != nil {
			if rerr := p.RegisterRoutes(plugMux); rerr != nil {
				log.Printf("[worker %s] plugin %s: route registration failed: %v",
					workerID, lp.Plugin.Info().Name, rerr)
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
				log.Printf("[worker %s] plugin %s: host functions failed: %v",
					workerID, lp.Plugin.Info().Name, rerr)
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
				if berr := bg.Run(ctx); berr != nil {
					log.Printf("[worker %s] plugin %s: background worker exited: %v",
						workerID, bg.Info().Name, berr)
				}
			}(p)
		}
	}

	w := &Worker{
		id:                  workerID,
		store:               store,
		concurrency:         *concurrency,
		heartbeatInterval:   *heartbeatInterval,
		pollInterval:        *pollInterval,
		ctx:                 ctx,
		cancel:              cancel,
		namespace:           *namespace,
		wasmCache:           newWasmLRUCache(*wasmCacheMaxEntries, *wasmCacheMaxMB),
		scheduleInterval:    15 * time.Second,
		compactionThreshold: *compactionThreshold,
		compactionInterval:  *compactionInterval,
		pluginRegistry:       pluginRegistry,
			tenantPools:          tenantPools,
			memorySampleRetention: *memorySampleRetention,
			retentionDays:       *retentionDays,
			maxRetries:          *maxRetries,
			redactionEnabled:    *redact,
			drainCh:             make(chan struct{}),
		}

	// Initialize memory-aware concurrency controller.
	monitor := NewMemoryMonitor(*memoryCheckInterval)
	mc := NewMemoryController(monitor, store, workerID, *concurrency, *memorySoftLimit, *memoryHardLimit)
	if err := mc.LoadEstimates(ctx); err != nil {
		log.Printf("[worker %s] Warning: failed to load memory estimates: %v", workerID, err)
	}
	w.memoryController = mc
	atomic.StoreInt64(&metricsDesiredConcurrency, int64(*concurrency))
	globalWorker = w

		// Start HTTP API server if configured.
	if *apiAddr != "" {
		api := &apiServer{store: store, worker: w, maxBodySize: *maxBodySize, db: db}

		// Use plugin mux if available, otherwise create a fresh one.
		mux := plugMux
		if mux == nil {
			mux = http.NewServeMux()
		}

		mux.HandleFunc("/healthz", api.handleHealthz)
		mux.HandleFunc("/metrics", handleMetrics)
		mux.HandleFunc("/api/admin/drain", api.handleDrain)
		// Schedule API routes (registered before workflows so /api/schedules is not caught by /api/workflows/).
		mux.HandleFunc("/api/schedules/", api.handleSchedules)
		mux.HandleFunc("/api/schedules", api.handleSchedulesList)
		mux.HandleFunc("/api/workflows/", api.handleWorkflows)
		mux.HandleFunc("/api/workflows", api.handleWorkflowsList)

		// Workflow definitions endpoint.
		mux.HandleFunc("/api/definitions", api.handleDefinitions)

		// Plugin discovery endpoint.
		mux.HandleFunc("/api/plugins", func(w http.ResponseWriter, r *http.Request) {
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

		// Serve embedded SPA for non-API paths.
		webFS, fsErr := fs.Sub(webDist, "web/dist")
		if fsErr != nil {
			log.Printf("[worker %s] WARNING: web/dist not found in embedded FS (build without SPA?): %v", workerID, fsErr)
		} else {
			fileServer := http.FileServer(http.FS(webFS))
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
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
		// Use plugin middleware chain if available.
		handler := plugHandler
		if handler == nil {
			handler = mux
		}

		// Wrap with auth middleware if --require-auth is true.
		if *requireAuth {
			handler = auth.Middleware(db)(handler)

			// If no API keys exist, auto-generate one for the default tenant.
			var keyCount int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tenant_api_keys`).Scan(&keyCount); err != nil {
				log.Printf("[worker %s] Warning: cannot check API key count: %v", workerID, err)
			} else if keyCount == 0 {
				key := auth.GenerateAPIKey()
				defaultTenantID := uuid.MustParse("00000000-0000-0000-0000-000000000000")
				ts := auth.NewTenantStore(db)
				if err := ts.CreateAPIKey(ctx, defaultTenantID, "auto-generated startup key", key); err != nil {
					log.Printf("[worker %s] Warning: failed to auto-generate API key: %v", workerID, err)
				} else {
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
		}

		// Create rate limiter and wrap handler.
		ratelim = newIPRateLimiter(rate.Limit(*rateLimit), *rateLimitBurst)
		handler = rateLimitMiddleware(ratelim)(handler)

		srv := &http.Server{
			Addr:         *apiAddr,
			Handler:      handler,
			ReadTimeout:  *httpReadTimeout,
			WriteTimeout: *httpWriteTimeout,
			IdleTimeout:  *httpIdleTimeout,
		}
		go func() {
			log.Printf("[worker %s] HTTP API listening on %s", workerID, *apiAddr)
			if err := srv.ListenAndServe(); err != http.ErrServerClosed {
				log.Printf("[worker %s] HTTP server error: %v", workerID, err)
			}
		}()
		go func() {
			<-ctx.Done()
			srv.Shutdown(context.Background())
		}()
	}

	// Handle shutdown signals.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Printf("[worker %s] Shutting down...", workerID)
		cancel()
		if ratelim != nil {
			ratelim.stop()
		}
		log.Printf("[worker %s] waiting for background workers to stop...", workerID)
		done := make(chan struct{})
		go func() {
			bgWg.Wait()
			close(done)
		}()
		select {
		case <-done:
			log.Printf("[worker %s] all background workers stopped", workerID)
		case <-time.After(30 * time.Second):
			log.Printf("[worker %s] timed out waiting for background workers after 30s", workerID)
		}
	}()

	workerCount.Set(1)
	defer workerCount.Set(0)
	w.Run()
	log.Printf("[worker %s] Shutdown complete", workerID)
}

// wasmLRUEntry is stored in container/list for LRU ordering.
type wasmLRUEntry struct {
	key   string
	bytes []byte
}

// wasmLRUCache is a bounded LRU cache for WASM byte slices keyed by
// "defName:version". Evicts LRU when entry count or total bytes exceeded.
type wasmLRUCache struct {
	mu      sync.Mutex
	list    *list.List
	index   map[string]*list.Element
	maxBytes int64
	maxEnts  int
}

func newWasmLRUCache(maxEntries, maxMB int) *wasmLRUCache {
	return &wasmLRUCache{
		list:     list.New(),
		index:    make(map[string]*list.Element),
		maxEnts:  maxEntries,
		maxBytes: int64(maxMB) * 1024 * 1024,
	}
}

func (c *wasmLRUCache) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.index[key]; ok {
		c.list.MoveToFront(elem)
		return elem.Value.(*wasmLRUEntry).bytes, true
	}
	return nil, false
}

func (c *wasmLRUCache) put(key string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.index[key]; ok {
		c.list.MoveToFront(elem)
		elem.Value.(*wasmLRUEntry).bytes = data
		return
	}
	for c.list.Len() >= c.maxEnts || c.sizeBytesLocked()+int64(len(data)) > c.maxBytes {
		c.evictLocked()
	}
	entry := &wasmLRUEntry{key: key, bytes: data}
	elem := c.list.PushFront(entry)
	c.index[key] = elem
}

func (c *wasmLRUCache) sizeBytesLocked() int64 {
	var total int64
	for e := c.list.Front(); e != nil; e = e.Next() {
		total += int64(len(e.Value.(*wasmLRUEntry).bytes))
	}
	return total
}

func (c *wasmLRUCache) evictLocked() {
	if elem := c.list.Back(); elem != nil {
		entry := elem.Value.(*wasmLRUEntry)
		delete(c.index, entry.key)
		c.list.Remove(elem)
	}
}

type Worker struct {
	id                string
	store             host.WorkflowStore
	concurrency       int
	heartbeatInterval time.Duration
	pollInterval      time.Duration
	namespace         string
	pluginRegistry       *host.PluginRegistry
	pluginStreamRegistry *host.PluginStreamRegistry
	tenantPools          *plugin.TenantPools

	ctx    context.Context
	cancel context.CancelFunc
		draining atomic.Bool
	wg     sync.WaitGroup

	inflight    sync.Map // map[workflowID]*host.WorkflowInstance
	execEngines sync.Map // map[workflowID]*host.Engine
	wasmCache   *wasmLRUCache

	scheduleMu       sync.Mutex
	scheduleInterval time.Duration

	// Backpressure / circuit breaker state.
	consecutiveDBErrors int
	backoffUntil        time.Time
	circuitOpen         atomic.Bool

	// Compaction settings.
	compactionThreshold int
	compactionInterval  time.Duration

	memoryController    *MemoryController
	maxRetries          int
	redactionEnabled    bool
	memorySampleRetention int
	retentionDays        int

	drainCh    chan struct{}
	drainOnce  sync.Once
}

// DrainComplete returns a channel that is closed when the drain completes
// (all in-flight workflows have finished).
func (w *Worker) DrainComplete() <-chan struct{} {
	return w.drainCh
}

func (w *Worker) Run() {
	// Initialize the global time seed so the first workflow execution
	// (before the dispatch loop updates it) sees a real wall clock.
	host.UpdateNowMs()

	// Background heartbeat goroutine.
	w.wg.Add(1)
	go w.heartbeatLoop()

	// Background zombie reaper goroutine.
	w.wg.Add(1)
	go w.reaperLoop()

	// Background concurrency key reaper goroutine (Feature 5).
	w.wg.Add(1)
	go w.concurrencyKeyReaperLoop()

	// Dispatch loop.
	w.wg.Add(1)
	go w.dispatchLoop()

	// Cron schedule loop.
	w.wg.Add(1)
	go w.scheduleLoop()

	// Compaction loop.
	w.wg.Add(1)
	go w.compactionLoop()

	// Memory estimate reload loop.
	w.wg.Add(1)
	go w.memoryReloadLoop()

	// Memory sample cleanup loop.
	w.wg.Add(1)
	go w.memoryCleanupLoop(w.memorySampleRetention)
	w.wg.Add(1)
	go w.retentionLoop(w.retentionDays)

	log.Printf("[worker %s] Running", w.id)

	<-w.ctx.Done()

	// Graceful shutdown: wait for in-flight workflows.
	log.Printf("[worker %s] Waiting for in-flight workflows to complete...", w.id)
	w.wg.Wait()
}

func (w *Worker) dispatchLoop() {
	defer w.wg.Done()

	// Keep the global time seed fresh for workflow sessions.
	host.UpdateNowMs()

	const maxBatchSize = 20 // cap claims per query to avoid oversized batches
	idleTicks := 0
	const maxIdleTicks = 6 // progressive backoff caps at 6 * pollInterval

	for {
		select {
		case <-w.ctx.Done():
			return
		default:
		}


		// If draining and no in-flight work, exit cleanly.
		if w.draining.Load() {
			inflight := 0
			w.inflight.Range(func(_, _ interface{}) bool { inflight++; return true })
			if inflight == 0 {
				log.Printf("[worker %s] drain complete, exiting", w.id)
				w.cancel()
				return
			}
			time.Sleep(w.pollInterval)
			continue
		}
		// Memory-aware tick: read system memory, compute pressure, adjust concurrency.
		w.memoryController.Tick(w.ctx)
		state := w.memoryController.State()
		updateMemoryMetrics(state)

		if !w.memoryController.CanClaim() {
			time.Sleep(w.pollInterval)
			continue
		}

		// Count in-flight workflows.
		count := 0
		w.inflight.Range(func(_, _ interface{}) bool {
			count++
			return true
		})

		free := w.memoryController.DynamicConcurrency() - count
		if free <= 0 {
			time.Sleep(w.pollInterval)
			continue
		}

		// If draining, stop claiming new work.
		if w.draining.Load() {
			time.Sleep(w.pollInterval)
			continue
		}

		batchSize := free
		if batchSize > maxBatchSize {
			batchSize = maxBatchSize
		}

		// Improvement 2: Try sticky fast-path first (low contention).
		pollStart := time.Now()
		stickyWfs, err := w.store.ClaimStickyWorkflows(w.ctx, w.id, w.namespace, batchSize)
		if err != nil {
			if isConnectionError(err) {
				w.consecutiveDBErrors++
				backoff := time.Duration(w.consecutiveDBErrors) * time.Second
				if backoff > 30*time.Second {
					backoff = 30 * time.Second
				}
				log.Printf("[worker %s] DB unreachable during sticky claim, backing off %v", w.id, backoff)
				select {
				case <-w.ctx.Done():
					return
				case <-time.After(backoff):
				}
				continue
			}
			log.Printf("[worker %s] sticky claim error: %v", w.id, err)
			time.Sleep(time.Second)
			continue
		}

		// Improvement 1: Fill remaining capacity with general batch claim.
		remaining := batchSize - len(stickyWfs)
		var generalWfs []*host.WorkflowInstance
		if remaining > 0 {
			var err error
			generalWfs, err = w.store.ClaimWorkflows(w.ctx, w.id, w.namespace, remaining)
			pollWaitDuration.Observe(time.Since(pollStart).Seconds())
			if err != nil {
				if isConnectionError(err) {
					w.consecutiveDBErrors++
					backoff := time.Duration(w.consecutiveDBErrors) * time.Second
					if backoff > 30*time.Second {
						backoff = 30 * time.Second
					}
					log.Printf("[worker %s] DB unreachable during claim, backing off %v", w.id, backoff)
					select {
					case <-w.ctx.Done():
						return
					case <-time.After(backoff):
					}
					continue
				}
				log.Printf("[worker %s] claim error: %v", w.id, err)
				time.Sleep(time.Second)
				continue
			}
		}

		// Combine results.
		wfs := append(stickyWfs, generalWfs...)

		if len(wfs) == 0 {
			// No work found — progressive backoff.
			idleTicks++
			sleep := time.Duration(idleTicks) * w.pollInterval
			if idleTicks > maxIdleTicks {
				sleep = maxIdleTicks * w.pollInterval
			}
			select {
			case <-w.ctx.Done():
				return
			case <-time.After(sleep):
			}
			continue
		}

		// Improvement 3: Found work — reset idle counter (coalesced polling).
		// Don't sleep before the next poll; there may be more work ready.
		idleTicks = 0
		w.consecutiveDBErrors = 0 // reset circuit breaker on success

		workflowsClaimed.Add(float64(len(wfs)))

		for _, wf := range wfs {
			log.Printf("[worker %s] Claimed workflow %s (%s v%d)",
				w.id, wf.ID, wf.DefName, wf.DefVersion)
			dispatchLatency.WithLabelValues("").Observe(time.Since(wf.CreatedAt).Seconds())

			w.inflight.Store(wf.ID, wf)
			w.wg.Add(1)
			go w.executeWorkflow(wf)
		}
	}
}

func (w *Worker) executeWorkflow(wf *host.WorkflowInstance) {
	defer w.wg.Done()
	defer w.inflight.Delete(wf.ID)
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[worker %s] PANIC in %s: %v — releasing", w.id, wf.ID, r)
			w.releaseOrFail(wf, fmt.Sprintf("panic: %v", r))
		}
	}()
	workflowsActive.Inc()
	defer workflowsActive.Dec()
	workflowStartTime := time.Now()

	// Measure memory usage before and after to estimate per-workflow footprint.
	beforeMem := w.memoryController.monitor.SampleUsage()
	defer func() {
		afterMem := w.memoryController.monitor.SampleUsage()
		if afterMem > beforeMem {
			delta := afterMem - beforeMem
			if delta > 0 {
				w.memoryController.RecordWorkflowMemory(context.Background(), wf.DefName, delta)
			}
		}
	}()

	// ---- Assign trace ID ----
	traceID := generateTraceID()
	if true {
		if _, err := w.store.TraceWorkflow(context.Background(), wf.ID, traceID); err != nil {
			log.Printf("[worker %s] %s: failed to set trace_id: %v", w.id, wf.ID, err)
		}
	}

	// ---- Load WASM ----
	wasmStart := time.Now()
	wasmBytes, err := w.loadWASM(wf.DefName, wf.DefVersion)
	wasmCompileDuration.WithLabelValues(wf.DefName).Observe(time.Since(wasmStart).Seconds())
	if err != nil {
		log.Printf("[worker %s] %s: failed to load WASM: %v", w.id, wf.ID, err)
		workflowsFailed.WithLabelValues(wf.DefName, "").Inc()
		workflowDuration.WithLabelValues(wf.DefName, "failed", "").Observe(time.Since(workflowStartTime).Seconds())
		var ce *host.CleatError
		errorCode := host.ErrUnknown.String()
		errorOp := ""
		if errors.As(err, &ce) {
			errorCode = ce.Code.String()
			errorOp = ce.Op
		}
		w.store.FailWorkflow(context.Background(), wf.ID, w.id, err.Error(), errorCode, errorOp, nil)
		return
	}

	// ---- Load event history ----
	history, err := w.store.LoadEventHistory(w.ctx, wf.ID)
	if err != nil {
		if isConnectionError(err) {
			log.Printf("[worker %s] DB down loading history for %s — releasing", w.id, wf.ID)
			w.store.ReleaseWorkflow(context.Background(), wf.ID, w.id, wf.NextWakeAt)
			return
		}
		workflowsFailed.WithLabelValues(wf.DefName, "").Inc()
		workflowDuration.WithLabelValues(wf.DefName, "failed", "").Observe(time.Since(workflowStartTime).Seconds())
		w.store.FailWorkflow(context.Background(), wf.ID, w.id, fmt.Sprintf("history load: %v", err), host.ErrUnknown.String(), "", nil)
		return
	}

	log.Printf("[worker %s] %s: loaded %d history events", w.id, wf.ID, len(history))

	// ---- Determine entry point ----
	entryPoint := determineEntryPoint(wf.Input)

	// ---- Load compaction state if present ----
	var compactionState *host.CompactionState
	compactionState, err = w.store.LoadCompactionState(w.ctx, wf.ID)
	if err != nil {
		log.Printf("[worker %s] %s: warning: failed to load compaction state: %v", w.id, wf.ID, err)
		compactionState = nil
	}

	// ---- Create engine ----
	rt, err := host.NewRuntime(w.ctx)
	if err != nil {
		workflowsFailed.WithLabelValues(wf.DefName, "").Inc()
		workflowDuration.WithLabelValues(wf.DefName, "failed", "").Observe(time.Since(workflowStartTime).Seconds())
		w.store.FailWorkflow(context.Background(), wf.ID, w.id, fmt.Sprintf("create runtime: %v", err), host.ErrUnknown.String(), "", nil)
		return
	}
	defer rt.Close(w.ctx)

	caller := &dbServiceCaller{store: w.store, workerID: w.id}
	engineOpts := []host.EngineOption{
		host.WithSignalStore(w.store.(host.SignalStore)),
		host.WithWorkflowState(&dbWorkflowState{version: wf.DefVersion}),
		host.WithWorkflowID(wf.ID),
		host.WithTenantID(wf.TenantID),
		host.WithChildWorkflowStore(w.store),
		host.WithPluginRegistry(w.pluginRegistry),
		host.WithMaxRetryAttempts(w.maxRetries),
	}
	// If the store supports concurrency keys (PostgresStore, ShardedStore),
	// enable virtual object scope enforcement.
	if cks, ok := w.store.(host.ConcurrencyKeyStore); ok {
		engineOpts = append(engineOpts, host.WithConcurrencyKeyStore(cks))
	}
	// Use tenant-scoped database connection for plugin host functions.
	if w.tenantPools != nil && wf.TenantID != "" {
		tenantID, err := uuid.Parse(wf.TenantID)
		if err == nil {
			tenantDB, err := w.tenantPools.For(w.ctx, tenantID)
			if err != nil {
				log.Printf("[worker %s] %s: cannot get tenant pool for %s: %v", w.id, wf.ID, wf.TenantID, err)
				workflowsFailed.WithLabelValues(wf.DefName, "").Inc()
				workflowDuration.WithLabelValues(wf.DefName, "failed", "").Observe(time.Since(workflowStartTime).Seconds())
				w.store.FailWorkflow(context.Background(), wf.ID, w.id, fmt.Sprintf("tenant pool: %v", err), host.ErrUnknown.String(), "", nil)
				return
			}
			engineOpts = append(engineOpts, host.WithDB(tenantDB))
		}
	}
	if compactionState != nil {
		engineOpts = append(engineOpts, host.WithCompactionState(compactionState))
		log.Printf("[worker %s] %s: loaded compaction state (compacted_step=%d)", w.id, wf.ID, compactionState.CompactedStep)
	}
	engine := host.NewEngine(rt, caller, engineOpts...)

	// ---- Execute/Resume ----
	inputJSON := wf.Input
	engineStart := time.Now()
	result, resultHistory, suspended, deferrals, queryState, err := engine.Replay(w.ctx, wasmBytes, entryPoint, inputJSON, history)
	engineElapsed := time.Since(engineStart)
	if len(history) > 0 {
		replayDuration.Observe(engineElapsed.Seconds())
	} else {
		freshDuration.WithLabelValues(wf.DefName).Observe(engineElapsed.Seconds())
	}
	if err != nil {
		log.Printf("[worker %s] %s: execution error: %v", w.id, wf.ID, err)
		workflowsFailed.WithLabelValues(wf.DefName, "").Inc()
		workflowDuration.WithLabelValues(wf.DefName, "failed", "").Observe(time.Since(workflowStartTime).Seconds())
		var ce *host.CleatError
		errorCode := host.ErrUnknown.String()
		errorOp := ""
		if errors.As(err, &ce) {
			errorCode = ce.Code.String()
			errorOp = ce.Op
		}
		w.store.FailWorkflow(context.Background(), wf.ID, w.id, err.Error(), errorCode, errorOp, nil)
		return
	}

	// ---- Handle result ----
	// Determine the final status before any DB writes so we can use the
	// appropriate atomic method for each path.

	// Collect new events (if any) so the same slice is available to all branches.
	var newEvents []host.EventRecord
	if len(resultHistory) > len(history) {
		newEvents = resultHistory[len(history):]
		// Redact sensitive fields in new events before persisting.
		if w.redactionEnabled {
			for i := range newEvents {
				newEvents[i].Request = host.Redact(newEvents[i].Request)
				newEvents[i].Response = host.Redact(newEvents[i].Response)
			}
		}
	}

	if suspended != nil && suspended.Reason == "continue_as_new" {
		// ContinueAsNew: persist events first, then atomically create a new run
		// AND complete the current one in a single transaction.
		if len(newEvents) > 0 {
			queryStart := time.Now()
			if err := w.store.AppendEventHistoryBatch(w.ctx, wf.ID, newEvents); err != nil {
				dbQueryDuration.WithLabelValues("append_events").Observe(time.Since(queryStart).Seconds())
				if isConnectionError(err) {
					log.Printf("[worker %s] DB down saving events for %s — releasing", w.id, wf.ID)
					w.store.ReleaseWorkflow(context.Background(), wf.ID, w.id, wf.NextWakeAt)
					return
				}
				log.Printf("[worker %s] %s: save events error: %v", w.id, wf.ID, err)
				workflowsFailed.WithLabelValues(wf.DefName, "").Inc()
				workflowDuration.WithLabelValues(wf.DefName, "failed", "").Observe(time.Since(workflowStartTime).Seconds())
				var ce *host.CleatError
				errorCode := host.ErrUnknown.String()
				errorOp := ""
				if errors.As(err, &ce) {
					errorCode = ce.Code.String()
					errorOp = ce.Op
				}
				w.store.FailWorkflow(context.Background(), wf.ID, w.id, err.Error(), errorCode, errorOp, nil)
				return
			}
			dbQueryDuration.WithLabelValues("append_events").Observe(time.Since(queryStart).Seconds())
		}

		// Atomically create the new run and complete the current one.
		log.Printf("[worker %s] %s: continue_as_new → starting new run", w.id, wf.ID)
		newRunID, err := w.store.ContinueAsNew(w.ctx, wf.ID, w.id, wf.DefName, wf.DefVersion, json.RawMessage(suspended.NewInput), result, queryState)
		if err != nil {
			log.Printf("[worker %s] %s: continue_as_new failed: %v", w.id, wf.ID, err)
			workflowsFailed.WithLabelValues(wf.DefName, "").Inc()
			workflowDuration.WithLabelValues(wf.DefName, "failed", "").Observe(time.Since(workflowStartTime).Seconds())
			w.store.FailWorkflow(context.Background(), wf.ID, w.id, fmt.Sprintf("continue_as_new: %v", err), host.ErrUnknown.String(), "", nil)
			return
		}
		log.Printf("[worker %s] %s: continued as new run %s", w.id, wf.ID, newRunID)
		workflowsCompleted.WithLabelValues(wf.DefName, "").Inc()
		workflowDuration.WithLabelValues(wf.DefName, "done", "").Observe(time.Since(workflowStartTime).Seconds())
		return
	}

	// Non-ContinueAsNew: atomically append events and finalize the workflow status
	// in a single database transaction.
	finalStatus := "done"
	var nextWakeAt time.Time
	if suspended != nil {
		finalStatus = "ready"
		nextWakeAt = suspended.SuspendUntil
	}

	queryStart := time.Now()
	err = w.store.FinalizeWorkflowSegment(w.ctx, wf.ID, w.id, newEvents, finalStatus, result, "", "", queryState, nextWakeAt)
	if err != nil {
		dbQueryDuration.WithLabelValues("finalize").Observe(time.Since(queryStart).Seconds())
		if isConnectionError(err) {
			log.Printf("[worker %s] DB down finalizing %s — releasing", w.id, wf.ID)
			w.store.ReleaseWorkflow(context.Background(), wf.ID, w.id, wf.NextWakeAt)
			return
		}
		log.Printf("[worker %s] %s: finalize error: %v", w.id, wf.ID, err)
		workflowsFailed.WithLabelValues(wf.DefName, "").Inc()
		workflowDuration.WithLabelValues(wf.DefName, "failed", "").Observe(time.Since(workflowStartTime).Seconds())
		var ce *host.CleatError
		errorCode := host.ErrUnknown.String()
		errorOp := ""
		if errors.As(err, &ce) {
			errorCode = ce.Code.String()
			errorOp = ce.Op
		}
		w.store.FailWorkflow(context.Background(), wf.ID, w.id, err.Error(), errorCode, errorOp, nil)
		return
	}
	dbQueryDuration.WithLabelValues("finalize").Observe(time.Since(queryStart).Seconds())

	// Post-finalization: logging and non-DB side effects.
	if finalStatus == "done" {
		// Workflow completed. Run any registered defer callbacks in LIFO order.
		if len(deferrals) > 0 {
			w.runDefers(wasmBytes, deferrals)
		}

		duration := time.Since(workflowStartTime)
		workflowDuration.WithLabelValues(wf.DefName, "done", "").Observe(duration.Seconds())
		workflowsCompleted.WithLabelValues(wf.DefName, "").Inc()
		log.Printf("[worker %s] %s: completed (duration=%v)", w.id, wf.ID, duration)
	} else {
		log.Printf("[worker %s] %s: suspended (%s), waking at %s",
			w.id, wf.ID, suspended.Reason, suspended.SuspendUntil)
	}
}

func (w *Worker) heartbeatLoop() {
	defer w.wg.Done()
	ticker := time.NewTicker(w.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
			hbStart := time.Now()
			_, err := w.store.BatchHeartbeat(w.ctx, w.id)
			if err != nil {
				backgroundLoopsTotal.WithLabelValues("heartbeat", "error").Inc()
				if isConnectionError(err) {
					log.Printf("[worker %s] BatchHeartbeat failed — DB appears down", w.id)
				} else {
					log.Printf("[worker %s] BatchHeartbeat error: %v", w.id, err)
				}
			} else {
				backgroundLoopsTotal.WithLabelValues("heartbeat", "ok").Inc()
			}
			backgroundLoopDuration.WithLabelValues("heartbeat").Set(time.Since(hbStart).Seconds())
		}
	}
}

func (w *Worker) reaperLoop() {
	defer w.wg.Done()
	// Reap stale instances on a configurable interval derived from the
	// heartbeat interval so that the reaper never runs more often than
	// the heartbeat (but at least every 10s).
	interval := max(w.heartbeatInterval, 10*time.Second)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
			reaperStart := time.Now()
			// A workflow must miss at least two consecutive heartbeats
			// before it is considered stale — otherwise a slow heartbeat
			// could cause false-positive reaping.
			staleTimeout := max(w.heartbeatInterval*2, 10*time.Second)
			reaped, err := w.store.ReapStaleInstances(w.ctx, staleTimeout)
			if err != nil {
				if isConnectionError(err) {
					log.Printf("[worker %s] Reaper: DB appears down", w.id)
			} else {
					log.Printf("[worker %s] Reaper: %v", w.id, err)
				}
				backgroundLoopsTotal.WithLabelValues("reaper", "error").Inc()
				backgroundLoopDuration.WithLabelValues("reaper").Set(time.Since(reaperStart).Seconds())
				continue
			}
			if reaped > 0 {
				log.Printf("[worker %s] Reaper: reclaimed %d stale instances", w.id, reaped)
				backgroundLoopItemsProcessed.WithLabelValues("reaper").Set(float64(reaped))
			}
			backgroundLoopsTotal.WithLabelValues("reaper", "ok").Inc()
			backgroundLoopDuration.WithLabelValues("reaper").Set(time.Since(reaperStart).Seconds())
		}
	}
}

func (w *Worker) concurrencyKeyReaperLoop() {
	defer w.wg.Done()
	// Reap expired concurrency keys every 60 seconds.
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
			ckStart := time.Now()
			reaped, err := w.store.ReapExpiredConcurrencyKeys(w.ctx)
			if err != nil {
				if isConnectionError(err) {
					log.Printf("[worker %s] Concurrency key reaper: DB appears down", w.id)
			} else {
					log.Printf("[worker %s] Concurrency key reaper: %v", w.id, err)
				}
				backgroundLoopsTotal.WithLabelValues("concurrency_key_reaper", "error").Inc()
				backgroundLoopDuration.WithLabelValues("concurrency_key_reaper").Set(time.Since(ckStart).Seconds())
				continue
			}
			if reaped > 0 {
				log.Printf("[worker %s] Concurrency key reaper: removed %d expired keys", w.id, reaped)
				backgroundLoopItemsProcessed.WithLabelValues("concurrency_key_reaper").Set(float64(reaped))
			}
			backgroundLoopsTotal.WithLabelValues("concurrency_key_reaper", "ok").Inc()
			backgroundLoopDuration.WithLabelValues("concurrency_key_reaper").Set(time.Since(ckStart).Seconds())
		}
	}
}

func (w *Worker) scheduleLoop() {
	defer w.wg.Done()
	ticker := time.NewTicker(w.scheduleInterval)
	defer ticker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
			schStart := time.Now()
			w.scheduleMu.Lock()
			schedules, err := w.store.GetDueSchedules(w.ctx)
			if err != nil {
				w.scheduleMu.Unlock()
				if isConnectionError(err) {
					log.Printf("[worker %s] Scheduler: DB appears down", w.id)
			} else {
					log.Printf("[worker %s] Scheduler: %v", w.id, err)
				}
				backgroundLoopsTotal.WithLabelValues("schedule", "error").Inc()
				backgroundLoopDuration.WithLabelValues("schedule").Set(time.Since(schStart).Seconds())
				continue
			}

			for _, sch := range schedules {
				// Build input with entry point if specified.
				input := sch.Input
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				if sch.EntryPoint != "" {
					var m map[string]interface{}
					json.Unmarshal(input, &m)
					if m == nil {
						m = make(map[string]interface{})
					}
					m["__entry_point"] = sch.EntryPoint
					input, _ = json.Marshal(m)
				}

				// Find latest version.
				versions, verr := w.store.ListVersions(w.ctx, sch.DefName)
				if verr != nil || len(versions) == 0 {
					log.Printf("[worker %s] Scheduler: definition %s not found for schedule %s",
						w.id, sch.DefName, sch.Name)
					continue
				}

				runID, _, serr := w.store.StartNewRun(w.ctx, sch.DefName, versions[0], input, "")
				if serr != nil {
					log.Printf("[worker %s] Scheduler: failed to start %s for schedule %s: %v",
						w.id, sch.DefName, sch.Name, serr)
					continue
				}

				// Compute next run time and update.
				nextRun := host.NextCronTime(sch.CronExpression, time.Now())
				if uerr := w.store.UpdateScheduleNextRun(w.ctx, sch.Name, nextRun); uerr != nil {
					log.Printf("[worker %s] Scheduler: failed to update next run for %s: %v",
						w.id, sch.Name, uerr)
				}

				log.Printf("[worker %s] Scheduler: fired %s → %s (next at %s)",
					w.id, sch.Name, runID, nextRun.Format(time.RFC3339))
			}
			w.scheduleMu.Unlock()
			backgroundLoopsTotal.WithLabelValues("schedule", "ok").Inc()
			backgroundLoopDuration.WithLabelValues("schedule").Set(time.Since(schStart).Seconds())
			backgroundLoopItemsProcessed.WithLabelValues("schedule").Set(float64(len(schedules)))
		}
	}
}

func (w *Worker) compactionLoop() {
	defer w.wg.Done()
	ticker := time.NewTicker(w.compactionInterval)
	defer ticker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
			compStart := time.Now()
			candidates, err := w.store.GetCompactionCandidates(w.ctx, w.compactionThreshold, 10)
			if err != nil {
				log.Printf("[worker %s] compaction: error finding candidates: %v", w.id, err)
				backgroundLoopsTotal.WithLabelValues("compaction", "error").Inc()
				backgroundLoopDuration.WithLabelValues("compaction").Set(time.Since(compStart).Seconds())
				continue
			}
			for _, wfID := range candidates {
				if err := host.CompactWorkflowHistory(w.ctx, w.store, wfID, w.compactionThreshold); err != nil {
					log.Printf("[worker %s] compaction: error compacting %s: %v", w.id, wfID, err)
				}
			}
			backgroundLoopsTotal.WithLabelValues("compaction", "ok").Inc()
			backgroundLoopDuration.WithLabelValues("compaction").Set(time.Since(compStart).Seconds())
			backgroundLoopItemsProcessed.WithLabelValues("compaction").Set(float64(len(candidates)))
		}
	}
}

func (w *Worker) memoryReloadLoop() {
	defer w.wg.Done()
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
			mrStart := time.Now()
			if err := w.memoryController.LoadEstimates(w.ctx); err != nil {
				log.Printf("[worker %s] memory reload: %v", w.id, err)
				backgroundLoopsTotal.WithLabelValues("memory_reload", "error").Inc()
			} else {
				backgroundLoopsTotal.WithLabelValues("memory_reload", "ok").Inc()
			}
			backgroundLoopDuration.WithLabelValues("memory_reload").Set(time.Since(mrStart).Seconds())
		}
	}
}

func (w *Worker) retentionLoop(retentionDays int) {
	defer w.wg.Done()
	if retentionDays <= 0 {
		return
	}
	interval := 24 * time.Hour
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
			deleted, err := w.store.DeleteExpiredEvents(w.ctx, cutoff)
			if err != nil {
				log.Printf("[worker %s] Retention: error deleting expired events: %v", w.id, err)
			} else if deleted > 0 {
				eventsDeletedTotal.Add(float64(deleted))
				log.Printf("[worker %s] Retention: deleted %d expired event rows", w.id, deleted)
			}
			retentionLastRunTimestamp.Set(float64(time.Now().Unix()))
		}
	}
}

func (w *Worker) memoryCleanupLoop(maxSamples int) {
	defer w.wg.Done()
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
			mcStart := time.Now()
			deleted, err := w.store.CleanupMemorySamples(w.ctx, maxSamples)
			if err != nil {
				log.Printf("[worker %s] memory cleanup: %v", w.id, err)
				backgroundLoopsTotal.WithLabelValues("memory_cleanup", "error").Inc()
			} else if deleted > 0 {
				log.Printf("[worker %s] memory cleanup: removed %d old samples", w.id, deleted)
			}
			if err == nil {
				backgroundLoopsTotal.WithLabelValues("memory_cleanup", "ok").Inc()
				if deleted > 0 {
					backgroundLoopItemsProcessed.WithLabelValues("memory_cleanup").Set(float64(deleted))
				}
			}
			backgroundLoopDuration.WithLabelValues("memory_cleanup").Set(time.Since(mcStart).Seconds())
		}
	}
}

func (w *Worker) updateDispatchLoop() {
	defer w.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
			w.dispatchPendingUpdates()
		}
	}
}

func (w *Worker) dispatchPendingUpdates() {
	ctx := context.Background()

	// Iterate over all claimed workflows.
	w.inflight.Range(func(key, value interface{}) bool {
		wfID := key.(string)

		// Get pending update requests for this workflow.
		updates, err := w.store.GetPendingUpdateRequests(ctx, wfID)
		if err != nil {
			log.Printf("[worker %s] %s: error fetching pending updates: %v", w.id, wfID, err)
			return true
		}
		if len(updates) == 0 {
			return true
		}

		// Find the running engine for this workflow.
		envVal, ok := w.execEngines.Load(wfID)
		if !ok {
			// Engine not found (maybe not running on this worker right now).
			// Leave the updates pending for the next claim cycle.
			return true
		}
		env := envVal.(*host.Engine)

		for _, upd := range updates {
			// Dispatch the update via the engine.
			result, dErr := env.DispatchUpdate(ctx, upd.UpdateName, upd.Payload)

			var resultStr, errStr string
			if dErr != nil {
				errStr = dErr.Error()
				log.Printf("[worker %s] %s: update %q failed: %v", w.id, wfID, upd.UpdateName, dErr)
			} else {
				resultStr = result
				log.Printf("[worker %s] %s: update %q completed", w.id, wfID, upd.UpdateName)
			}

			// Store the result in the workflow_update_requests table.
			if cErr := w.store.CompleteUpdateRequest(ctx, wfID, upd.UpdateName, resultStr, errStr); cErr != nil {
				log.Printf("[worker %s] %s: error completing update %q: %v", w.id, wfID, upd.UpdateName, cErr)
			}

			// If the update request has an associated promise, resolve or reject it.
			if upd.PromiseID != "" {
				if dErr != nil {
					if rErr := w.store.RejectPromise(ctx, wfID, upd.PromiseID, errStr); rErr != nil {
						log.Printf("[worker %s] %s: error rejecting promise %s: %v", w.id, wfID, upd.PromiseID, rErr)
					}
				} else {
					if rErr := w.store.ResolvePromise(ctx, wfID, upd.PromiseID, resultStr); rErr != nil {
						log.Printf("[worker %s] %s: error resolving promise %s: %v", w.id, wfID, upd.PromiseID, rErr)
					}
				}
			}
		}
		return true
	})
}

func (w *Worker) loadWASM(defName string, defVersion int) ([]byte, error) {
	key := fmt.Sprintf("%s:%d", defName, defVersion)

	if cached, ok := w.wasmCache.get(key); ok {
		wasmCacheHits.Inc()
		return cached, nil
	}
	wasmCacheMisses.Inc()

	wasmBytes, err := w.store.LoadWASM(w.ctx, defName, defVersion)
	if err != nil {
		return nil, err
	}

	w.wasmCache.put(key, wasmBytes)
	return wasmBytes, nil
}

func (w *Worker) waitForDB() {
	backoff := 500 * time.Millisecond
	for i := 0; i < 20; i++ {
		select {
		case <-w.ctx.Done():
			return
		default:
		}

		if _, err := w.store.ClaimWorkflow(w.ctx, "", w.namespace); err == nil || !isConnectionError(err) {
			// DB is back (or claim returned no work, which means DB is reachable).
			log.Printf("[worker %s] DB connection re-established", w.id)
			return
		}

		log.Printf("[worker %s] DB reconnect attempt %d/20 failed, retrying in %v",
			w.id, i+1, backoff)
		time.Sleep(backoff)
		backoff = time.Duration(math.Min(float64(backoff*2), 10e9))
	}
}

func (w *Worker) releaseOrFail(wf *host.WorkflowInstance, errMsg string) {
	if errMsg != "" {
		w.store.FailWorkflow(context.Background(), wf.ID, w.id, errMsg, "", "", nil)
	} else {
		w.store.ReleaseWorkflow(context.Background(), wf.ID, w.id, wf.NextWakeAt)
	}
}

// dbServiceCaller implements host.ServiceCaller for the worker.
type dbServiceCaller struct {
	store    host.WorkflowStore
	workerID string
}

// fetchRequest is the JSON payload for DurableFetch calls.
type fetchRequest struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

func (c *dbServiceCaller) Call(ctx context.Context, service, operation, requestJSON string) (string, error) {
	if service == "http" && operation == "fetch" {
		return c.handleHTTPFetch(ctx, requestJSON)
	}
	return "", fmt.Errorf("service %s.%s not configured: no endpoint registered", service, operation)
}

func (c *dbServiceCaller) handleHTTPFetch(ctx context.Context, requestJSON string) (string, error) {
	var req fetchRequest
	if err := json.Unmarshal([]byte(requestJSON), &req); err != nil {
		return "", fmt.Errorf("http.fetch: invalid request JSON: %w", err)
	}
	if req.URL == "" {
		return "", fmt.Errorf("http.fetch: url is required")
	}
	if req.Method == "" {
		req.Method = "GET"
	}
	var body io.Reader
	if req.Body != "" {
		body = strings.NewReader(req.Body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, body)
	if err != nil {
		return "", fmt.Errorf("http.fetch: %w", err)
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("http.fetch: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("http.fetch: reading response: %w", err)
	}
	respHeaders := make(map[string]string)
	for k := range resp.Header {
		respHeaders[k] = resp.Header.Get(k)
	}
	result, _ := json.Marshal(map[string]interface{}{
		"status":  resp.StatusCode,
		"headers": respHeaders,
		"body":    string(respBody),
	})
	return string(result), nil
}

// dbWorkflowState implements host.WorkflowState.
type dbWorkflowState struct {
	version    int
	minVersion int
}

func (s *dbWorkflowState) Version() int    { return s.version }
func (s *dbWorkflowState) MinVersion() int { return s.minVersion }

// hostPluginRegistryAdapter bridges plugin.FuncRegistry and plugin.StreamFuncRegistry
// to host.PluginRegistry and host.PluginStreamRegistry.
type hostPluginRegistryAdapter struct {
	registry       *host.PluginRegistry
	streamRegistry *host.PluginStreamRegistry
	pluginName     string
}

func (a *hostPluginRegistryAdapter) Register(opts plugin.FuncOptions, fn plugin.PluginFunc) error {
	if opts.Name == "" {
		return fmt.Errorf("function name must not be empty")
	}
	if strings.Contains(opts.Name, "/") || strings.Contains(opts.Name, "\x00") {
		return fmt.Errorf("function name %q contains invalid characters", opts.Name)
	}
	if a.registry.Has(a.pluginName, opts.Name) {
		return fmt.Errorf("function %q already registered for plugin %q", opts.Name, a.pluginName)
	}
	if opts.Idempotent {
		return a.registry.RegisterIdempotent(a.pluginName, opts.Name, host.PluginFunc(fn))
	}
	return a.registry.Register(a.pluginName, opts.Name, host.PluginFunc(fn))
}

func (a *hostPluginRegistryAdapter) RegisterStream(opts plugin.FuncOptions, fn plugin.PluginStreamFunc) error {
	if opts.Name == "" {
		return fmt.Errorf("function name must not be empty")
	}
	if strings.Contains(opts.Name, "/") || strings.Contains(opts.Name, "\x00") {
		return fmt.Errorf("function name %q contains invalid characters", opts.Name)
	}
	if a.streamRegistry == nil {
		return fmt.Errorf("stream function registry not initialized")
	}
	if a.streamRegistry.Has(a.pluginName, opts.Name) {
		return fmt.Errorf("stream function %q already registered for plugin %q", opts.Name, a.pluginName)
	}
	return a.streamRegistry.RegisterStream(a.pluginName, opts, fn)
}

// determineEntryPoint extracts the entry point name from workflow input.
// The default entry point is "place_order". This can be overridden by
// including an "__entry_point" field in the input.
func determineEntryPoint(input json.RawMessage) string {
	if len(input) == 0 {
		return "place_order"
	}
	var meta struct {
		EntryPoint string `json:"__entry_point"`
	}
	if err := json.Unmarshal(input, &meta); err == nil && meta.EntryPoint != "" {
		return meta.EntryPoint
	}
	return "place_order"
}

// runDefers executes registered defer callbacks in LIFO (reverse) order.
// Each defer is invoked as a WASM export named "__defer_<description>".
// Errors during defer execution are logged but do not prevent other defers
// from running.
func (w *Worker) runDefers(wasmBytes []byte, deferrals map[string]string) {
	rt, err := host.NewRuntime(w.ctx)
	if err != nil {
		log.Printf("[worker %s] runDefers: create runtime: %v", w.id, err)
		return
	}
	defer rt.Close(w.ctx)

	engine := host.NewEngine(rt, &dbServiceCaller{store: w.store, workerID: w.id})

	// Collect defer IDs sorted by step number for LIFO ordering.
	type defEntry struct {
		id   string
		desc string
	}
	var entries []defEntry
	for id, desc := range deferrals {
		entries = append(entries, defEntry{id: id, desc: desc})
	}

	// Reverse order (LIFO): "defer-3" before "defer-2" before "defer-1".
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		deferName := "__defer_" + entry.desc
		_, err := engine.RunDefer(w.ctx, wasmBytes, deferName, nil)
		if err != nil {
			log.Printf("[worker %s] defer %s (%s) failed: %v", w.id, entry.id, entry.desc, err)
		} else {
			log.Printf("[worker %s] defer %s (%s) completed", w.id, entry.id, entry.desc)
		}
	}
}

func generateWorkerID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func generateTraceID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	patterns := []string{
		"connection refused",
		"connection reset",
		"connection closed",
		"no reachable servers",
		"server closed the connection",
		"connection timed out",
		"broken pipe",
		"EOF",
		"driver: bad connection",
	}
	for _, p := range patterns {
		if strings.Contains(strings.ToLower(s), strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// loadShardConfigs reads a JSON file containing an array of ShardConfig.
// The file format is:
//
//	[
//	  {"name": "shard-0", "conn_str": "postgres://...", "tenants": ["tenant-a"]},
//	  {"name": "shard-1", "conn_str": "postgres://...", "tenants": ["tenant-b"]}
//	]
func loadShardConfigs(path string) ([]host.ShardConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read shards file: %w", err)
	}
	var configs []host.ShardConfig
	if err := json.Unmarshal(data, &configs); err != nil {
		return nil, fmt.Errorf("parse shards file: %w", err)
	}
	if len(configs) == 0 {
		return nil, fmt.Errorf("shards file %q contains no shard definitions", path)
	}
	for i, cfg := range configs {
		if cfg.Name == "" {
			return nil, fmt.Errorf("shards file %q: entry %d has empty name", path, i)
		}
		if cfg.ConnStr == "" {
			return nil, fmt.Errorf("shards file %q: shard %q has empty conn_str", path, cfg.Name)
		}
	}
	return configs, nil
}

// ---- Rate limiter ----

// ipRateLimiter provides per-IP token-bucket rate limiting using a sync.Map
// with periodic cleanup of stale entries.
type rateLimiterEntry struct {
	limiter  *rate.Limiter
	lastUsed time.Time
}

type ipRateLimiter struct {
	mu       sync.Mutex
	limits   map[string]*rateLimiterEntry
	rate     rate.Limit
	burst    int
	stopCh   chan struct{}
}

func newIPRateLimiter(r rate.Limit, burst int) *ipRateLimiter {
	rl := &ipRateLimiter{
		limits: make(map[string]*rateLimiterEntry),
		rate:   r,
		burst:  burst,
		stopCh: make(chan struct{}),
	}
	// Background cleanup: every 10 minutes remove limiter entries that
	// have not been used in the last hour.
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				rl.mu.Lock()
				now := time.Now()
						for ip, entry := range rl.limits {
					if now.Sub(entry.lastUsed) > time.Hour {
						delete(rl.limits, ip)
					}
				}
				rl.mu.Unlock()
			case <-rl.stopCh:
				return
			}
		}
	}()
	return rl
}

func (rl *ipRateLimiter) stop() {
	close(rl.stopCh)
}

func (rl *ipRateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	entry, ok := rl.limits[ip]
	if !ok {
		entry = &rateLimiterEntry{
			limiter: rate.NewLimiter(rl.rate, rl.burst),
		}
		rl.limits[ip] = entry
	}
	entry.lastUsed = time.Now()
	return entry.limiter.Allow()
}

// clientIP extracts the client IP from the request, preferring the
// X-Forwarded-For header when present.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		parts := strings.Split(fwd, ",")
		return strings.TrimSpace(parts[0])
	}
	// Strip port from RemoteAddr.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rateLimitMiddleware returns an HTTP middleware that rate-limits requests
// per client IP. On rate limit exceeded, it returns HTTP 429 with a JSON
// error body and a Retry-After header.
func rateLimitMiddleware(rl *ipRateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := clientIP(r)
			if !rl.allow(ip) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"error":          "rate limit exceeded",
					"retry_after_ms": 1000,
				})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ---- HTTP API server ----

type apiServer struct {
	store       host.WorkflowStore
	worker      *Worker
	maxBodySize int64
	db          *sql.DB
}

func (s *apiServer) writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *apiServer) writeError(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, map[string]string{"error": msg})
}

func (s *apiServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if s.worker.memoryController != nil && s.worker.memoryController.Pressure() > 0 {
		s.writeJSON(w, 200, map[string]interface{}{
			"ok":       true,
			"degraded": true,
			"reason":   "memory_pressure",
			"pressure": s.worker.memoryController.Pressure(),
		})
		return
	}
	s.writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleDrain handles POST and GET /api/admin/drain for graceful worker drain.
func (s *apiServer) handleDrain(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handleDrainStart(w, r)
	case http.MethodGet:
		s.handleDrainStatus(w, r)
	default:
		s.writeError(w, 405, "method not allowed")
	}
}

// handleDrainStart initiates a worker drain.
func (s *apiServer) handleDrainStart(w http.ResponseWriter, r *http.Request) {
	s.worker.draining.Store(true)

	count := 0
	s.worker.inflight.Range(func(_, _ interface{}) bool {
		count++
		return true
	})

	s.writeJSON(w, 202, map[string]interface{}{
		"draining":  true,
		"in_flight": count,
	})
}

// handleDrainStatus returns the current drain status.
func (s *apiServer) handleDrainStatus(w http.ResponseWriter, r *http.Request) {
	draining := s.worker.draining.Load()

	count := 0
	s.worker.inflight.Range(func(_, _ interface{}) bool {
		count++
		return true
	})

	resp := map[string]interface{}{
		"draining":  draining,
		"in_flight": count,
	}

	if draining && count == 0 {
		s.worker.drainOnce.Do(func() {
			close(s.worker.drainCh)
		})
		resp["complete"] = true
	}

	s.writeJSON(w, 200, resp)
}

// handleWorkflowsList handles GET /api/workflows (without trailing path).
func (s *apiServer) handleWorkflowsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, 405, "method not allowed")
		return
	}
	q := r.URL.Query()
	filter := host.WorkflowFilter{
		Status:        q.Get("status"),
		InputContains: q.Get("input_contains"),
		ErrorContains: q.Get("error_contains"),
		Search:        q.Get("search"),
		Limit:         100,
	}
	workflows, err := s.store.ListWorkflows(r.Context(), filter)
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	if workflows == nil {
		workflows = []host.WorkflowInstance{}
	}
	s.writeJSON(w, 200, workflows)
}

// handleWorkflows routes /api/workflows/* requests.
func (s *apiServer) handleWorkflows(w http.ResponseWriter, r *http.Request) {
	// Strip /api/workflows/ prefix.
	path := strings.TrimPrefix(r.URL.Path, "/api/workflows/")
	if path == "" || path == "/" {
		s.handleWorkflowsList(w, r)
		return
	}

	// Split remaining path.
	parts := strings.Split(strings.TrimSuffix(path, "/"), "/")
	if len(parts) == 0 {
		s.writeError(w, 400, "bad request")
		return
	}

	id := parts[0]

	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		// GET /api/workflows/:id or GET /api/workflows/:id?key=X
		s.handleGetWorkflow(w, r, id)
	case len(parts) == 2 && parts[1] == "start" && r.Method == http.MethodPost:
		// POST /api/workflows/:name/start
		s.handleStartWorkflow(w, r, id)
	case len(parts) == 2 && parts[1] == "signal" && r.Method == http.MethodPost:
		// POST /api/workflows/:id/signal
		s.handleSignal(w, r, id)
	case len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost:
		// POST /api/workflows/:id/cancel
		s.handleCancel(w, r, id)
	case len(parts) == 2 && parts[1] == "history" && r.Method == http.MethodGet:
		// GET /api/workflows/:id/history
		s.handleGetHistory(w, r, id)
	case len(parts) == 2 && parts[1] == "query" && r.Method == http.MethodGet:
		// GET /api/workflows/:id/query?key=X
		s.handleGetQueryState(w, r, id)
	case len(parts) == 2 && parts[1] == "dag" && r.Method == http.MethodGet:
		// GET /api/workflows/:id/dag
		s.handleGetDAG(w, r, id)
	case len(parts) == 2 && parts[1] == "promises" && r.Method == http.MethodGet:
		// GET /api/workflows/:id/promises
		s.handleListPromises(w, r, id)
	case len(parts) >= 4 && parts[1] == "promises":
		// /api/workflows/:id/promises/:promiseId/resolve|reject
		if len(parts) == 4 {
			promiseID := parts[2]
			switch {
			case parts[3] == "resolve" && r.Method == http.MethodPost:
				s.handleResolvePromise(w, r, id, promiseID)
			case parts[3] == "reject" && r.Method == http.MethodPost:
				s.handleRejectPromise(w, r, id, promiseID)
			default:
			s.writeError(w, 404, "not found")
			}
		} else {
			s.writeError(w, 404, "not found")
		}
	case len(parts) == 3 && parts[1] == "update" && r.Method == http.MethodPost:
		// POST /api/workflows/:id/update/:name
		s.handleWorkflowUpdate(w, r, id, parts[2])
	default:
		s.writeError(w, 404, "not found")
	}
}

func (s *apiServer) handleGetWorkflow(w http.ResponseWriter, r *http.Request, id string) {
	// Check if this is a query state request.
	if key := r.URL.Query().Get("key"); key != "" {
		value, err := s.store.GetQueryState(r.Context(), id, key)
		if err != nil {
			s.writeError(w, 500, err.Error())
			return
		}
		s.writeJSON(w, 200, map[string]string{"key": key, "value": value})
		return
	}

	// Return full workflow info.
	wf, err := s.store.GetWorkflowByID(r.Context(), id)
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	if wf == nil {
		s.writeError(w, 404, "workflow not found")
		return
	}
	s.writeJSON(w, 200, wf)
}

func (s *apiServer) handleStartWorkflow(w http.ResponseWriter, r *http.Request, name string) {
	if s.worker.memoryController != nil && !s.worker.memoryController.CanAcceptAPIWorkflows() {
		s.writeError(w, 503, "worker is under memory pressure; cannot accept new workflows")
		return
	}

	var input struct {
		Input          json.RawMessage `json:"input"`
		EntryPoint     string          `json:"entry_point"`
		ConcurrencyKey string          `json:"concurrency_key"`
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, s.maxBodySize)
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				s.writeError(w, 413, "request body too large")
				return
			}
			s.writeError(w, 400, "invalid JSON: "+err.Error())
			return
		}
	}
	if input.Input == nil {
		input.Input = json.RawMessage("{}")
	}

	// Find the latest version of this workflow.
	versions, err := s.store.ListVersions(r.Context(), name)
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	if len(versions) == 0 {
		s.writeError(w, 404, "workflow definition not found")
		return
	}

	// Inject entry point into input if provided.
	in := input.Input
	if input.EntryPoint != "" {
		in, _ = json.Marshal(map[string]interface{}{
			"__entry_point": input.EntryPoint,
		})
		// Merge with provided input.
		var merged map[string]interface{}
		json.Unmarshal(input.Input, &merged)
		merged["__entry_point"] = input.EntryPoint
		in, _ = json.Marshal(merged)
	}

	// Support Concurrency-Key header or JSON body field (Feature 5).
	concurrencyKey := r.Header.Get("Cleat-Concurrency-Key")
	if concurrencyKey == "" {
		concurrencyKey = input.ConcurrencyKey
	}

	// Support Idempotency-Key header for exactly-once semantics.
	idempotencyKey := r.Header.Get("Idempotency-Key")
	// Redact sensitive fields in the input before storing.
	if s.worker.redactionEnabled {
		in = json.RawMessage(host.Redact(string(in)))
	}
	runID, alreadyExisted, err := s.store.StartNewRun(r.Context(), name, versions[0], in, idempotencyKey)
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	if alreadyExisted {
		s.writeJSON(w, 200, map[string]string{"workflow_id": runID, "already_started": "true"})
		return
	}

	// If concurrency key is specified, try to acquire it for the new run.
	if concurrencyKey != "" {
		ttl := 30 * time.Minute
		acquired, err := s.store.AcquireConcurrencyKey(r.Context(), concurrencyKey, runID, ttl)
		if err != nil {
			s.writeError(w, 500, err.Error())
			return
		}
		if !acquired {
			// Key already held by another workflow — fail the new run and return conflict.
			s.store.FailWorkflow(context.Background(), runID, "", "concurrency key conflict: "+concurrencyKey, "", "", nil)
			s.writeError(w, 409, "workflow already running with key "+concurrencyKey)
			return
		}
	}

	s.writeJSON(w, 201, map[string]string{"id": runID})
}

func (s *apiServer) handleSignal(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		SignalName string `json:"signal_name"`
		Payload    string `json:"payload"`
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, signalMaxBodySize)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				s.writeError(w, 413, "request body too large")
				return
			}
			s.writeError(w, 400, "invalid JSON: "+err.Error())
			return
		}
	}
	if req.SignalName == "" {
		s.writeError(w, 400, "signal_name is required")
		return
	}
	payload := req.Payload
	if s.worker.redactionEnabled {
		payload = host.Redact(payload)
	}
	if err := s.store.DeliverSignal(r.Context(), id, req.SignalName, payload); err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	s.writeJSON(w, 200, map[string]string{"status": "delivered"})
}

func (s *apiServer) handleCancel(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		Reason string `json:"reason"`
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, signalMaxBodySize)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				s.writeError(w, 413, "request body too large")
				return
			}
			s.writeError(w, 400, "invalid JSON: "+err.Error())
			return
		}
	}
	if err := s.store.RequestCancellation(r.Context(), id, req.Reason); err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	s.writeJSON(w, 200, map[string]string{"status": "cancellation_requested"})
}

func (s *apiServer) handleGetHistory(w http.ResponseWriter, r *http.Request, id string) {
	history, err := s.store.LoadEventHistory(r.Context(), id)
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	if history == nil {
		history = []host.EventRecord{}
	}
	s.writeJSON(w, 200, history)
}

func (s *apiServer) handleGetQueryState(w http.ResponseWriter, r *http.Request, id string) {
	key := r.URL.Query().Get("key")
	value, err := s.store.GetQueryState(r.Context(), id, key)
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	s.writeJSON(w, 200, map[string]string{"key": key, "value": value})
}

func (s *apiServer) handleGetDAG(w http.ResponseWriter, r *http.Request, id string) {
	// Look up the workflow instance to get def_name and def_version.
	wf, err := s.store.GetWorkflowByID(r.Context(), id)
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	if wf == nil {
		s.writeError(w, 404, "workflow not found")
		return
	}

	// Load the dag_spec from workflow_defs.
	spec, err := s.store.LoadDAGSpec(r.Context(), wf.DefName, wf.DefVersion)
	if err != nil {
		s.writeError(w, 404, err.Error())
		return
	}
	if spec == nil {
		s.writeError(w, 404, "no DAG spec for this workflow definition")
		return
	}

	// Parse the spec so we can add workflow_id metadata.
	var dagData map[string]interface{}
	if err := json.Unmarshal(spec, &dagData); err != nil {
		s.writeError(w, 500, "invalid dag_spec JSON: "+err.Error())
		return
	}

	response := map[string]interface{}{
		"workflow_id": wf.ID,
		"dag":         dagData,
	}
	s.writeJSON(w, 200, response)
}

// ---- Promise API handlers ----

// handleListPromises handles GET /api/workflows/:id/promises
func (s *apiServer) handleListPromises(w http.ResponseWriter, r *http.Request, id string) {
	promises, err := s.store.ListPromises(r.Context(), id)
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	if promises == nil {
		promises = []host.PromiseInfo{}
	}
	s.writeJSON(w, 200, promises)
}

// handleResolvePromise handles POST /api/workflows/:id/promises/:promiseId/resolve
func (s *apiServer) handleResolvePromise(w http.ResponseWriter, r *http.Request, id, promiseID string) {
	var req struct {
		Result string `json:"result"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodySize)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.writeError(w, 413, "request body too large")
			return
		}
		s.writeError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if err := s.store.ResolvePromise(r.Context(), id, promiseID, req.Result); err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	s.writeJSON(w, 200, map[string]string{"status": "resolved"})
}

// handleRejectPromise handles POST /api/workflows/:id/promises/:promiseId/reject
func (s *apiServer) handleRejectPromise(w http.ResponseWriter, r *http.Request, id, promiseID string) {
	var req struct {
		Reason string `json:"reason"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodySize)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.writeError(w, 413, "request body too large")
			return
		}
		s.writeError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if err := s.store.RejectPromise(r.Context(), id, promiseID, req.Reason); err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	s.writeJSON(w, 200, map[string]string{"status": "rejected"})
}

// handleWorkflowUpdate handles POST /api/workflows/:id/update/:name
func (s *apiServer) handleWorkflowUpdate(w http.ResponseWriter, r *http.Request, id, updateName string) {
	// Verify the workflow exists.
	wf, err := s.store.GetWorkflowByID(r.Context(), id)
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	if wf == nil {
		s.writeError(w, 404, "workflow not found")
		return
	}

	// Parse the request body as the update payload.
	var payload string
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, signalMaxBodySize)
		body, rErr := io.ReadAll(r.Body)
		if rErr != nil {
			var maxErr *http.MaxBytesError
			if errors.As(rErr, &maxErr) {
				s.writeError(w, 413, "request body too large")
				return
			}
			s.writeError(w, 400, "failed to read request body")
			return
		}
		payload = string(body)
	}
	if payload == "" {
		payload = "{}"
	}
	// Redact sensitive fields from the payload before persisting.
	if s.worker.redactionEnabled {
		payload = host.Redact(payload)
	}

	// Check if there's already a pending update with the same name.
	pending, pErr := s.store.GetPendingUpdateRequests(r.Context(), id)
	if pErr != nil {
		s.writeError(w, 500, pErr.Error())
		return
	}
	for _, p := range pending {
		if p.UpdateName == updateName {
			s.writeError(w, 409, "update already pending with name: "+updateName)
			return
		}
	}

	// Generate a promise ID so the caller can track the update outcome.
	promiseID, err := generateUpdatePromiseID()
	if err != nil {
		s.writeError(w, 500, "failed to generate promise ID: "+err.Error())
		return
	}

	// Create the update request in the database.
	if err := s.store.CreateUpdateRequest(r.Context(), id, updateName, payload, promiseID); err != nil {
		s.writeError(w, 500, err.Error())
		return
	}

	// Create an associated promise record so the caller can poll for the result.
	if ps, ok := s.store.(host.PromiseStore); ok {
		if pErr := ps.CreatePromise(r.Context(), id, "update:"+updateName, promiseID); pErr != nil {
			log.Printf("[api] %s: warning: failed to create promise for update %q: %v", id, updateName, pErr)
		}
	}

	s.writeJSON(w, 202, map[string]string{"promise_id": promiseID})
}

// generateUpdatePromiseID creates a unique ID for tracking an update's outcome.
func generateUpdatePromiseID() (string, error) {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}
	return "upd-" + hex.EncodeToString(b), nil
}

// ---- Schedule API handlers ----

// handleSchedulesList handles GET /api/schedules and POST /api/schedules
func (s *apiServer) handleSchedulesList(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.handleCreateSchedule(w, r)
		return
	}
	if r.Method != http.MethodGet {
		s.writeError(w, 405, "method not allowed")
		return
	}
	schedules, err := s.store.ListSchedules(r.Context())
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	if schedules == nil {
		schedules = []host.Schedule{}
	}
	s.writeJSON(w, 200, schedules)
}

// handleSchedules routes /api/schedules/* requests.
func (s *apiServer) handleSchedules(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/schedules/")
	if path == "" || path == "/" {
		s.handleSchedulesList(w, r)
		return
	}
	parts := strings.Split(strings.TrimSuffix(path, "/"), "/")
	if len(parts) == 0 {
		s.writeError(w, 400, "bad request")
		return
	}
	name := parts[0]

	switch {
	case len(parts) == 2 && parts[1] == "enable" && r.Method == http.MethodPost:
		if err := s.store.SetScheduleEnabled(r.Context(), name, true); err != nil {
			s.writeError(w, 500, err.Error())
			return
		}
		s.writeJSON(w, 200, map[string]string{"status": "enabled"})
	case len(parts) == 2 && parts[1] == "disable" && r.Method == http.MethodPost:
		if err := s.store.SetScheduleEnabled(r.Context(), name, false); err != nil {
			s.writeError(w, 500, err.Error())
			return
		}
		s.writeJSON(w, 200, map[string]string{"status": "disabled"})
	case len(parts) == 1 && r.Method == http.MethodDelete:
		if err := s.store.DeleteSchedule(r.Context(), name); err != nil {
			s.writeError(w, 500, err.Error())
			return
		}
		s.writeJSON(w, 200, map[string]string{"status": "deleted"})
	default:
		s.writeError(w, 404, "not found")
	}
}

// handleCreateSchedule handles POST /api/schedules
func (s *apiServer) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, 405, "method not allowed")
		return
	}
	var req struct {
		Name       string          `json:"name"`
		Cron       string          `json:"cron"`
		DefName    string          `json:"def_name"`
		EntryPoint string          `json:"entry_point"`
		Input      json.RawMessage `json:"input"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodySize)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.writeError(w, 413, "request body too large")
			return
		}
		s.writeError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if req.Name == "" || req.Cron == "" || req.DefName == "" {
		s.writeError(w, 400, "name, cron, and def_name are required")
		return
	}
	sch := host.Schedule{
		Name:           req.Name,
		DefName:        req.DefName,
		EntryPoint:     req.EntryPoint,
		CronExpression: req.Cron,
		Input:          req.Input,
		Enabled:        true,
	}
	if err := s.store.CreateSchedule(r.Context(), sch); err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	s.writeJSON(w, 201, map[string]string{"status": "created"})
}

// handleDefinitions handles GET /api/definitions
func (s *apiServer) handleDefinitions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, 405, "method not allowed")
		return
	}

	defs, err := s.store.ListWorkflowDefs(r.Context(), "")
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}

	// Load memory stats for enrichment.
	memoryStats := make(map[string]*host.WorkflowMemoryStats)
	if stats, err := s.store.LoadMemoryStats(r.Context()); err == nil {
		for i := range stats {
			memoryStats[stats[i].DefName] = &stats[i]
		}
	}

	type defResponse struct {
		Name            string                   `json:"name"`
		Version         int                      `json:"version"`
		ABIVersion      int                      `json:"abi_version"`
		MinVersion      int                      `json:"min_version"`
		CreatedAt       time.Time                `json:"created_at"`
		Deprecated      bool                     `json:"deprecated"`
		ActiveInstances int                      `json:"active_instances"`
		Memory          *host.WorkflowMemoryStats `json:"memory,omitempty"`
	}

	var response []defResponse
	for _, def := range defs {
		count, _ := s.store.CountActiveInstances(r.Context(), def.Name, def.Version)
		dr := defResponse{
			Name:            def.Name,
			Version:         def.Version,
			ABIVersion:      def.ABIVersion,
			MinVersion:      def.MinVersion,
			CreatedAt:       def.CreatedAt,
			Deprecated:      def.Deprecated,
			ActiveInstances: count,
		}
		if ms, ok := memoryStats[def.Name]; ok {
			dr.Memory = ms
		}
		response = append(response, dr)
	}
	if response == nil {
		response = []defResponse{}
	}
	s.writeJSON(w, 200, response)
}


func (s *apiServer) inflightCount() int {
	count := 0
	s.worker.inflight.Range(func(_, _ interface{}) bool { count++; return true })
	return count
}

// handleMetrics serves Prometheus-format metrics.
func idempotencyCleanupLoop(ctx context.Context, db *sql.DB, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, err := db.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE expires_at < now()`)
			if err != nil {
				log.Printf("idempotency cleanup: %v", err)
			}
		}
	}
}

// baseDSNFromURL parses a PostgreSQL URL and returns a key=value DSN
// suitable for use as a base connection string (without user/password).
// Example URL: "postgres://user:pass@localhost:5432/cleat?sslmode=disable"
// Returns:     "host=localhost port=5432 dbname=cleat sslmode=disable"
func baseDSNFromURL(dbURL string) string {
	u, err := url.Parse(dbURL)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	dbname := strings.TrimPrefix(u.Path, "/")
	sslmode := u.Query().Get("sslmode")
	if sslmode == "" {
		sslmode = "disable"
	}
	return fmt.Sprintf("host=%s port=%s dbname=%s sslmode=%s", host, port, dbname, sslmode)
}

// baseDSNFromDSN parses a PostgreSQL DSN in key=value format and returns a
// base DSN with user and password stripped. If the input DSN cannot be parsed,
// it is returned as-is (the tenant pool constructor will fail gracefully).
func baseDSNFromDSN(dsn string) string {
	// Simple approach: remove user=... and password=... from the DSN.
	// This handles the common case for shard connection strings.
	var parts []string
	for _, part := range strings.Fields(dsn) {
		if strings.HasPrefix(part, "user=") || strings.HasPrefix(part, "password=") {
			continue
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
}
