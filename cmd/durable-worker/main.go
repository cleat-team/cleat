// Command durable-worker is a production worker daemon for executing durable
// workflows. It polls PostgreSQL for runnable workflow instances using
// SELECT ... FOR UPDATE SKIP LOCKED, loads WASM modules, replays event history,
// and drives execution. It handles workflow suspension (sleep, await signals),
// heartbeating, and database failover.
//
// Build:
//
//	go build -o durable-worker ./cmd/durable-worker/
//
// Run:
//
//	durable-worker --db "postgres://user:pass@localhost/cleat?sslmode=disable"
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
	"io/fs"
	"log"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	_ "github.com/lib/pq"

	"github.com/rcownie/durable/internal/host"
	"github.com/rcownie/durable/internal/plugin"
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
		pluginRegistry = host.NewPluginRegistry()
		plugList       []*plugin.LoadedPlugin
		plugHandler    http.Handler
		plugMux        *http.ServeMux
		bgWg          sync.WaitGroup
	)

	var store host.WorkflowStore
	var db *sql.DB
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
			fmt.Fprintln(os.Stderr, "error: --db or DATABASE_URL is required")
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
	plugList, err = plugin.LoadAll(ctx, pluginEnv)
	if err != nil {
		log.Fatalf("[worker %s] plugin: %v", workerID, err)
	}

	if err := plugin.RunMigrations(ctx, db, nil, plugList); err != nil {
		log.Fatalf("[worker %s] plugin migrations: %v", workerID, err)
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
				registry:   pluginRegistry,
				pluginName: lp.Plugin.Info().Name,
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
	pluginRegistry    *host.PluginRegistry

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	inflight  sync.Map // map[workflowID]*host.WorkflowInstance
	wasmCache map[string][]byte
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

	for {
		select {
		case <-w.ctx.Done():
			return
		default:
		}

		// Limit concurrency.
		count := 0
		w.inflight.Range(func(_, _ interface{}) bool {
			count++
			return true
		})
		if count >= w.concurrency {
			time.Sleep(w.pollInterval)
			continue
		}

		wf, err := w.store.ClaimWorkflow(w.ctx, w.id, w.namespace)
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

		if wf == nil {
			time.Sleep(w.pollInterval)
			continue
		}

		log.Printf("[worker %s] Claimed workflow %s (%s v%d)",
			w.id, wf.ID, wf.DefName, wf.DefVersion)

		w.inflight.Store(wf.ID, wf)
		w.wg.Add(1)
		go w.executeWorkflow(wf)
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
		host.WithChildWorkflowStore(w.store),
		host.WithPluginRegistry(w.pluginRegistry),
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

func (c *dbServiceCaller) Call(ctx context.Context, service, operation, requestJSON string) (string, error) {
	// In production, this would make actual HTTP/gRPC calls to the target services.
	// For now, this is a placeholder — it returns an error indicating the service
	// endpoint needs to be configured.
	return "", fmt.Errorf("service %s.%s not configured: no endpoint registered", service, operation)
}

// dbWorkflowState implements host.WorkflowState.
type dbWorkflowState struct {
	version    int
	minVersion int
}

func (s *dbWorkflowState) Version() int    { return s.version }
func (s *dbWorkflowState) MinVersion() int { return s.minVersion }

// hostPluginRegistryAdapter bridges plugin.FuncRegistry to host.PluginRegistry.
type hostPluginRegistryAdapter struct {
	registry   *host.PluginRegistry
	pluginName string
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

	fmt.Fprintf(w, "# HELP durable_workflows_active Currently claimed workflow instances\n")
	fmt.Fprintf(w, "# TYPE durable_workflows_active gauge\n")
	fmt.Fprintf(w, "durable_workflows_active %d\n\n", active)

	fmt.Fprintf(w, "# HELP durable_workflows_completed_total Workflows completed successfully\n")
	fmt.Fprintf(w, "# TYPE durable_workflows_completed_total counter\n")
	fmt.Fprintf(w, "durable_workflows_completed_total %d\n\n", completed)

	fmt.Fprintf(w, "# HELP durable_workflows_failed_total Workflows that failed\n")
	fmt.Fprintf(w, "# TYPE durable_workflows_failed_total counter\n")
	fmt.Fprintf(w, "durable_workflows_failed_total %d\n\n", failed)

	fmt.Fprintf(w, "# HELP durable_workflows_claimed_total Workflows claimed from the queue\n")
	fmt.Fprintf(w, "# TYPE durable_workflows_claimed_total counter\n")
	fmt.Fprintf(w, "durable_workflows_claimed_total %d\n\n", claimed)

	fmt.Fprintf(w, "# HELP durable_durable_calls_total DurableCall invocations\n")
	fmt.Fprintf(w, "# TYPE durable_durable_calls_total counter\n")
	fmt.Fprintf(w, "durable_durable_calls_total %d\n\n", durableCalls)

	fmt.Fprintf(w, "# HELP durable_replay_duration_seconds Replay duration histogram\n")
	fmt.Fprintf(w, "# TYPE durable_replay_duration_seconds summary\n")
	if replayCount > 0 {
		avgUs := replayTotalUs / replayCount
		fmt.Fprintf(w, "durable_replay_duration_seconds_count %d\n", replayCount)
		fmt.Fprintf(w, "durable_replay_duration_seconds_sum %.6f\n", float64(replayTotalUs)/1e6)
		fmt.Fprintf(w, "durable_replay_duration_seconds{quantile=\"0.5\"} %.6f\n", float64(avgUs)/1e6)
	} else {
		fmt.Fprintf(w, "durable_replay_duration_seconds_count 0\n")
		fmt.Fprintf(w, "durable_replay_duration_seconds_sum 0\n")
	}
	fmt.Fprintf(w, "\n")

	fmt.Fprintf(w, "# HELP durable_poll_wait_seconds Time spent waiting for work\n")
	fmt.Fprintf(w, "# TYPE durable_poll_wait_seconds summary\n")
	if pollWaitCount > 0 {
		avgWaitUs := pollWaitTotalUs / pollWaitCount
		fmt.Fprintf(w, "durable_poll_wait_seconds_count %d\n", pollWaitCount)
		fmt.Fprintf(w, "durable_poll_wait_seconds_sum %.6f\n", float64(pollWaitTotalUs)/1e6)
		fmt.Fprintf(w, "durable_poll_wait_seconds{quantile=\"0.5\"} %.6f\n", float64(avgWaitUs)/1e6)
	} else {
		fmt.Fprintf(w, "durable_poll_wait_seconds_count 0\n")
		fmt.Fprintf(w, "durable_poll_wait_seconds_sum 0\n")
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
