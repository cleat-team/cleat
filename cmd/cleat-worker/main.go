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
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"math"
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
	"github.com/rcownie/cleat/internal/host"
	"github.com/rcownie/cleat/internal/plugin"

	// Plugins
	_ "github.com/rcownie/cleat/plugins/llm"
	_ "github.com/rcownie/cleat/plugins/pgvector"
)

//go:embed web/dist
var webDist embed.FS

// -- Prometheus metrics --
var (
	metricsWorkflowsActive    int64
	metricsWorkflowsCompleted int64
	metricsWorkflowsFailed    int64
	metricsWorkflowsClaimed   int64
	metricsDurableCallsTotal  int64
	metricsReplayDurationUs   int64
	metricsReplayCount        int64
	metricsPollWaitCount      int64
	metricsPollWaitTotalUs    int64
)

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
	flag.Parse()

	workerID := generateWorkerID()
	log.Printf("[worker %s] Starting with concurrency=%d", workerID, *concurrency)

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

	if err := plugin.RunMigrations(ctx, db, nil, plugList); err != nil {
		log.Fatalf("[worker %s] plugin migrations: %v", workerID, err)
	}

	plugin.InitAll(ctx, pluginEnv, plugList)

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
		wasmCache:           make(map[string][]byte),
		scheduleInterval:    15 * time.Second,
		compactionThreshold: *compactionThreshold,
		compactionInterval:  *compactionInterval,
		pluginRegistry:      pluginRegistry,
			tenantPools:         tenantPools,
		}

		// Start HTTP API server if configured.
	if *apiAddr != "" {
		api := &apiServer{store: store, worker: w}

		// Use plugin mux if available, otherwise create a fresh one.
		mux := plugMux
		if mux == nil {
			mux = http.NewServeMux()
		}

		mux.HandleFunc("/healthz", api.handleHealthz)
		mux.HandleFunc("/metrics", handleMetrics)
		// Schedule API routes (registered before workflows so /api/schedules is not caught by /api/workflows/).
		mux.HandleFunc("/api/schedules/", api.handleSchedules)
		mux.HandleFunc("/api/schedules", api.handleSchedulesList)
		mux.HandleFunc("/api/workflows/", api.handleWorkflows)
		mux.HandleFunc("/api/workflows", api.handleWorkflowsList)

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

		srv := &http.Server{Addr: *apiAddr, Handler: handler}
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

	w.Run()
	log.Printf("[worker %s] Shutdown complete", workerID)
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
	wg     sync.WaitGroup

	inflight    sync.Map // map[workflowID]*host.WorkflowInstance
	execEngines sync.Map // map[workflowID]*host.Engine
	wasmCache   map[string][]byte
	wasmMu    sync.RWMutex

	scheduleMu       sync.Mutex
	scheduleInterval time.Duration

	// Backpressure / circuit breaker state.
	consecutiveDBErrors int
	backoffUntil        time.Time
	circuitOpen         atomic.Bool

	// Compaction settings.
	compactionThreshold int
	compactionInterval  time.Duration
}

func (w *Worker) Run() {
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

	log.Printf("[worker %s] Running", w.id)

	<-w.ctx.Done()

	// Graceful shutdown: wait for in-flight workflows.
	log.Printf("[worker %s] Waiting for in-flight workflows to complete...", w.id)
	w.wg.Wait()
}

func (w *Worker) dispatchLoop() {
	defer w.wg.Done()

	const maxBatchSize = 20 // cap claims per query to avoid oversized batches
	idleTicks := 0
	const maxIdleTicks = 6 // progressive backoff caps at 6 * pollInterval

	for {
		select {
		case <-w.ctx.Done():
			return
		default:
		}

		// Count in-flight workflows.
		count := 0
		w.inflight.Range(func(_, _ interface{}) bool {
			count++
			return true
		})

		free := w.concurrency - count
		if free <= 0 {
			time.Sleep(w.pollInterval)
			continue
		}

		batchSize := free
		if batchSize > maxBatchSize {
			batchSize = maxBatchSize
		}

		// Improvement 2: Try sticky fast-path first (low contention).
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

		atomic.AddInt64(&metricsWorkflowsClaimed, int64(len(wfs)))

		for _, wf := range wfs {
			log.Printf("[worker %s] Claimed workflow %s (%s v%d)",
				w.id, wf.ID, wf.DefName, wf.DefVersion)

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

	// ---- Assign trace ID ----
	traceID := generateTraceID()
	if true {
		if _, err := w.store.TraceWorkflow(context.Background(), wf.ID, traceID); err != nil {
			log.Printf("[worker %s] %s: failed to set trace_id: %v", w.id, wf.ID, err)
		}
	}

	// ---- Load WASM ----
	wasmBytes, err := w.loadWASM(wf.DefName, wf.DefVersion)
	if err != nil {
		log.Printf("[worker %s] %s: failed to load WASM: %v", w.id, wf.ID, err)
		w.store.FailWorkflow(context.Background(), wf.ID, w.id, err.Error(), nil)
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
		w.store.FailWorkflow(context.Background(), wf.ID, w.id, fmt.Sprintf("history load: %v", err), nil)
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
		w.store.FailWorkflow(context.Background(), wf.ID, w.id, fmt.Sprintf("create runtime: %v", err), nil)
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
				w.store.FailWorkflow(context.Background(), wf.ID, w.id, fmt.Sprintf("tenant pool: %v", err), nil)
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
	result, resultHistory, suspended, deferrals, queryState, err := engine.Replay(w.ctx, wasmBytes, entryPoint, inputJSON, history)
	if err != nil {
		log.Printf("[worker %s] %s: execution error: %v", w.id, wf.ID, err)
		w.store.FailWorkflow(context.Background(), wf.ID, w.id, err.Error(), nil)
		return
	}

	// ---- Handle result ----
	// Persist any new events.
	if len(resultHistory) > len(history) {
		newEvents := resultHistory[len(history):]
		if err := w.store.AppendEventHistoryBatch(w.ctx, wf.ID, newEvents); err != nil {
			if isConnectionError(err) {
				log.Printf("[worker %s] DB down saving events for %s — releasing", w.id, wf.ID)
				w.store.ReleaseWorkflow(context.Background(), wf.ID, w.id, wf.NextWakeAt)
				return
			}
			log.Printf("[worker %s] %s: save events error: %v", w.id, wf.ID, err)
			w.store.FailWorkflow(context.Background(), wf.ID, w.id, err.Error(), nil)
			return
		}
	}

	if suspended != nil {
		if suspended.Reason == "continue_as_new" {
			// ContinueAsNew: create a new run and complete the current one.
			log.Printf("[worker %s] %s: continue_as_new → starting new run", w.id, wf.ID)
			newRunID, _, err := w.store.StartNewRun(w.ctx, wf.DefName, wf.DefVersion, json.RawMessage(suspended.NewInput), "")
			if err != nil {
				log.Printf("[worker %s] %s: continue_as_new start failed: %v", w.id, wf.ID, err)
				w.store.FailWorkflow(context.Background(), wf.ID, w.id, fmt.Sprintf("continue_as_new: %v", err), nil)
				return
			}
			log.Printf("[worker %s] %s: continued as new run %s", w.id, wf.ID, newRunID)
			continueAsNewResult, _ := json.Marshal(map[string]interface{}{"continue_as_new": true, "new_run_id": newRunID})
			w.store.CompleteWorkflow(context.Background(), wf.ID, w.id, string(continueAsNewResult), nil)
			return
		}

		// Persist suspend state.
		log.Printf("[worker %s] %s: suspended (%s), waking at %s",
			w.id, wf.ID, suspended.Reason, suspended.SuspendUntil)
		if err := w.store.ReleaseWorkflow(w.ctx, wf.ID, w.id, suspended.SuspendUntil); err != nil {
			if isConnectionError(err) {
				log.Printf("[worker %s] DB down releasing %s", w.id, wf.ID)
				return
			}
			log.Printf("[worker %s] %s: release error: %v", w.id, wf.ID, err)
		}
		return
	}

	// Workflow completed. Run any registered defer callbacks in LIFO order.
	if len(deferrals) > 0 {
		w.runDefers(wasmBytes, deferrals)
	}

	log.Printf("[worker %s] %s: completed → %s", w.id, wf.ID, result)
	w.store.CompleteWorkflow(context.Background(), wf.ID, w.id, result, queryState)
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
			w.inflight.Range(func(key, value interface{}) bool {
				wfID := key.(string)
				alive, err := w.store.Heartbeat(w.ctx, wfID, w.id)
				if err != nil && isConnectionError(err) {
					log.Printf("[worker %s] Heartbeat failed for %s — DB appears down", w.id, wfID)
				} else if !alive {
					log.Printf("[worker %s] %s: lost ownership via heartbeat", w.id, wfID)
					w.inflight.Delete(key)
				}
				return true
			})
		}
	}
}

func (w *Worker) reaperLoop() {
	defer w.wg.Done()
	// Reap stale instances every 30 seconds.
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
			reaped, err := w.store.ReapStaleInstances(w.ctx, 30*time.Second)
			if err != nil {
				if isConnectionError(err) {
					log.Printf("[worker %s] Reaper: DB appears down", w.id)
			} else {
					log.Printf("[worker %s] Reaper: %v", w.id, err)
				}
				continue
			}
			if reaped > 0 {
				log.Printf("[worker %s] Reaper: reclaimed %d stale instances", w.id, reaped)
			}
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
			reaped, err := w.store.ReapExpiredConcurrencyKeys(w.ctx)
			if err != nil {
				if isConnectionError(err) {
					log.Printf("[worker %s] Concurrency key reaper: DB appears down", w.id)
			} else {
					log.Printf("[worker %s] Concurrency key reaper: %v", w.id, err)
				}
				continue
			}
			if reaped > 0 {
				log.Printf("[worker %s] Concurrency key reaper: removed %d expired keys", w.id, reaped)
			}
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
			w.scheduleMu.Lock()
			schedules, err := w.store.GetDueSchedules(w.ctx)
			if err != nil {
				w.scheduleMu.Unlock()
				if isConnectionError(err) {
					log.Printf("[worker %s] Scheduler: DB appears down", w.id)
			} else {
					log.Printf("[worker %s] Scheduler: %v", w.id, err)
				}
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
			candidates, err := w.store.GetCompactionCandidates(w.ctx, w.compactionThreshold, 10)
			if err != nil {
				log.Printf("[worker %s] compaction: error finding candidates: %v", w.id, err)
				continue
			}
			for _, wfID := range candidates {
				if err := host.CompactWorkflowHistory(w.ctx, w.store, wfID, w.compactionThreshold); err != nil {
					log.Printf("[worker %s] compaction: error compacting %s: %v", w.id, wfID, err)
				}
			}
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

	w.wasmMu.RLock()
	cached, ok := w.wasmCache[key]
	w.wasmMu.RUnlock()
	if ok {
		return cached, nil
	}

	wasmBytes, err := w.store.LoadWASM(w.ctx, defName, defVersion)
	if err != nil {
		return nil, err
	}

	w.wasmMu.Lock()
	w.wasmCache[key] = wasmBytes
	w.wasmMu.Unlock()

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
		w.store.FailWorkflow(context.Background(), wf.ID, w.id, errMsg, nil)
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
	b := make([]byte, 4)
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

// ---- HTTP API server ----

type apiServer struct {
	store  host.WorkflowStore
	worker *Worker
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
	s.writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleWorkflowsList handles GET /api/workflows (without trailing path).
func (s *apiServer) handleWorkflowsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, 405, "method not allowed")
		return
	}
	status := r.URL.Query().Get("status")
	limit := 100
	workflows, err := s.store.ListWorkflows(r.Context(), status, limit)
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
	var input struct {
		Input          json.RawMessage `json:"input"`
		EntryPoint     string          `json:"entry_point"`
		ConcurrencyKey string          `json:"concurrency_key"`
	}
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&input)
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
			s.store.FailWorkflow(context.Background(), runID, "", "concurrency key conflict: "+concurrencyKey, nil)
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
		json.NewDecoder(r.Body).Decode(&req)
	}
	if req.SignalName == "" {
		s.writeError(w, 400, "signal_name is required")
		return
	}
	if err := s.store.DeliverSignal(r.Context(), id, req.SignalName, req.Payload); err != nil {
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
		json.NewDecoder(r.Body).Decode(&req)
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
		body, rErr := io.ReadAll(r.Body)
		if rErr != nil {
			s.writeError(w, 400, "failed to read request body")
			return
		}
		payload = string(body)
	} else {
		payload = "{}"
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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

// handleMetrics serves Prometheus-format metrics.
func handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")

	active := atomic.LoadInt64(&metricsWorkflowsActive)
	completed := atomic.LoadInt64(&metricsWorkflowsCompleted)
	failed := atomic.LoadInt64(&metricsWorkflowsFailed)
	claimed := atomic.LoadInt64(&metricsWorkflowsClaimed)
	durableCalls := atomic.LoadInt64(&metricsDurableCallsTotal)
	replayTotalUs := atomic.LoadInt64(&metricsReplayDurationUs)
	replayCount := atomic.LoadInt64(&metricsReplayCount)
	pollWaitCount := atomic.LoadInt64(&metricsPollWaitCount)
	pollWaitTotalUs := atomic.LoadInt64(&metricsPollWaitTotalUs)

	fmt.Fprintf(w, "# HELP cleat_workflows_active Currently claimed workflow instances\n")
	fmt.Fprintf(w, "# TYPE cleat_workflows_active gauge\n")
	fmt.Fprintf(w, "cleat_workflows_active %d\n\n", active)

	fmt.Fprintf(w, "# HELP cleat_workflows_completed_total Workflows completed successfully\n")
	fmt.Fprintf(w, "# TYPE cleat_workflows_completed_total counter\n")
	fmt.Fprintf(w, "cleat_workflows_completed_total %d\n\n", completed)

	fmt.Fprintf(w, "# HELP cleat_workflows_failed_total Workflows that failed\n")
	fmt.Fprintf(w, "# TYPE cleat_workflows_failed_total counter\n")
	fmt.Fprintf(w, "cleat_workflows_failed_total %d\n\n", failed)

	fmt.Fprintf(w, "# HELP cleat_workflows_claimed_total Workflows claimed from the queue\n")
	fmt.Fprintf(w, "# TYPE cleat_workflows_claimed_total counter\n")
	fmt.Fprintf(w, "cleat_workflows_claimed_total %d\n\n", claimed)

	fmt.Fprintf(w, "# HELP cleat_calls_total DurableCall invocations\n")
	fmt.Fprintf(w, "# TYPE cleat_calls_total counter\n")
	fmt.Fprintf(w, "cleat_calls_total %d\n\n", durableCalls)

	fmt.Fprintf(w, "# HELP cleat_replay_duration_seconds Replay duration histogram\n")
	fmt.Fprintf(w, "# TYPE cleat_replay_duration_seconds summary\n")
	if replayCount > 0 {
		avgUs := replayTotalUs / replayCount
		fmt.Fprintf(w, "cleat_replay_duration_seconds_count %d\n", replayCount)
		fmt.Fprintf(w, "cleat_replay_duration_seconds_sum %.6f\n", float64(replayTotalUs)/1e6)
		fmt.Fprintf(w, "cleat_replay_duration_seconds{quantile=\"0.5\"} %.6f\n", float64(avgUs)/1e6)
	} else {
		fmt.Fprintf(w, "cleat_replay_duration_seconds_count 0\n")
		fmt.Fprintf(w, "cleat_replay_duration_seconds_sum 0\n")
	}
	fmt.Fprintf(w, "\n")

	fmt.Fprintf(w, "# HELP cleat_poll_wait_seconds Time spent waiting for work\n")
	fmt.Fprintf(w, "# TYPE cleat_poll_wait_seconds summary\n")
	if pollWaitCount > 0 {
		avgWaitUs := pollWaitTotalUs / pollWaitCount
		fmt.Fprintf(w, "cleat_poll_wait_seconds_count %d\n", pollWaitCount)
		fmt.Fprintf(w, "cleat_poll_wait_seconds_sum %.6f\n", float64(pollWaitTotalUs)/1e6)
		fmt.Fprintf(w, "cleat_poll_wait_seconds{quantile=\"0.5\"} %.6f\n", float64(avgWaitUs)/1e6)
	} else {
		fmt.Fprintf(w, "cleat_poll_wait_seconds_count 0\n")
		fmt.Fprintf(w, "cleat_poll_wait_seconds_sum 0\n")
	}
}

// idempotencyCleanupLoop periodically deletes expired idempotency keys
// from the database. Runs until ctx is cancelled.
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
