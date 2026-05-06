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
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/lib/pq"

	"github.com/rcownie/durable/internal/host"
)

func main() {
	dbURL := flag.String("db", "", "PostgreSQL connection URL (required)")
	concurrency := flag.Int("concurrency", 10, "Max concurrent workflow executions")
	heartbeatInterval := flag.Duration("heartbeat", 5*time.Second, "Heartbeat interval")
	pollInterval := flag.Duration("poll", 500*time.Millisecond, "Poll interval when no work")
	flag.Parse()

	if *dbURL == "" {
		*dbURL = os.Getenv("DATABASE_URL")
	}
	if *dbURL == "" {
		fmt.Fprintln(os.Stderr, "error: --db or DATABASE_URL is required")
		os.Exit(1)
	}

	workerID := generateWorkerID()
	log.Printf("[worker %s] Starting with concurrency=%d", workerID, *concurrency)

	db, err := sql.Open("postgres", *dbURL)
	if err != nil {
		log.Fatalf("[worker %s] Failed to connect to database: %v", workerID, err)
	}
	defer db.Close()

	db.SetMaxOpenConns(*concurrency + 5)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := host.NewPostgresStore(db)

	w := &Worker{
		id:                workerID,
		store:             store,
		concurrency:       *concurrency,
		heartbeatInterval: *heartbeatInterval,
		pollInterval:      *pollInterval,
		ctx:               ctx,
		cancel:            cancel,
		wasmCache:         make(map[string][]byte),
	}

	// Handle shutdown signals.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Printf("[worker %s] Shutting down...", workerID)
		cancel()
	}()

	w.Run()
	log.Printf("[worker %s] Shutdown complete", workerID)
}

type Worker struct {
	id                string
	store             *host.PostgresStore
	concurrency       int
	heartbeatInterval time.Duration
	pollInterval      time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	inflight  sync.Map // map[workflowID]*host.WorkflowInstance
	wasmCache map[string][]byte
	wasmMu    sync.RWMutex
}

func (w *Worker) Run() {
	// Background heartbeat goroutine.
	w.wg.Add(1)
	go w.heartbeatLoop()

	// Background zombie reaper goroutine.
	w.wg.Add(1)
	go w.reaperLoop()

	// Dispatch loop.
	w.wg.Add(1)
	go w.dispatchLoop()

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

		wf, err := w.store.ClaimWorkflow(w.ctx, w.id)
		if err != nil {
			if isConnectionError(err) {
				log.Printf("[worker %s] DB unreachable during claim, waiting...", w.id)
				w.waitForDB()
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

	// ---- Load WASM ----
	wasmBytes, err := w.loadWASM(wf.DefName, wf.DefVersion)
	if err != nil {
		log.Printf("[worker %s] %s: failed to load WASM: %v", w.id, wf.ID, err)
		w.store.FailWorkflow(context.Background(), wf.ID, w.id, err.Error())
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
		w.store.FailWorkflow(context.Background(), wf.ID, w.id, fmt.Sprintf("history load: %v", err))
		return
	}

	log.Printf("[worker %s] %s: loaded %d history events", w.id, wf.ID, len(history))

	// ---- Determine entry point ----
	entryPoint := determineEntryPoint(wf.Input)

	// ---- Create engine ----
	rt, err := host.NewRuntime(w.ctx)
	if err != nil {
		w.store.FailWorkflow(context.Background(), wf.ID, w.id, fmt.Sprintf("create runtime: %v", err))
		return
	}
	defer rt.Close(w.ctx)

	caller := &dbServiceCaller{store: w.store, workerID: w.id}
	engine := host.NewEngine(rt, caller,
		host.WithSignalStore(w.store),
		host.WithWorkflowState(&dbWorkflowState{version: wf.DefVersion}),
		host.WithWorkflowID(wf.ID),
		host.WithChildWorkflowStore(w.store),
	)

	// ---- Execute/Resume ----
	inputJSON := wf.Input
	result, resultHistory, suspended, deferrals, err := engine.Replay(w.ctx, wasmBytes, entryPoint, inputJSON, history)
	if err != nil {
		log.Printf("[worker %s] %s: execution error: %v", w.id, wf.ID, err)
		w.store.FailWorkflow(context.Background(), wf.ID, w.id, err.Error())
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
			w.store.FailWorkflow(context.Background(), wf.ID, w.id, err.Error())
			return
		}
	}

	if suspended != nil {
		if suspended.Reason == "continue_as_new" {
			// ContinueAsNew: create a new run and complete the current one.
			log.Printf("[worker %s] %s: continue_as_new → starting new run", w.id, wf.ID)
			newRunID, err := w.store.StartNewRun(w.ctx, wf.DefName, wf.DefVersion, json.RawMessage(suspended.NewInput))
			if err != nil {
				log.Printf("[worker %s] %s: continue_as_new start failed: %v", w.id, wf.ID, err)
				w.store.FailWorkflow(context.Background(), wf.ID, w.id, fmt.Sprintf("continue_as_new: %v", err))
				return
			}
			log.Printf("[worker %s] %s: continued as new run %s", w.id, wf.ID, newRunID)
			continueAsNewResult, _ := json.Marshal(map[string]interface{}{"continue_as_new": true, "new_run_id": newRunID})
			w.store.CompleteWorkflow(context.Background(), wf.ID, w.id, string(continueAsNewResult))
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
	w.store.CompleteWorkflow(context.Background(), wf.ID, w.id, result)
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

		if _, err := w.store.ClaimWorkflow(w.ctx, ""); err == nil || !isConnectionError(err) {
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
		w.store.FailWorkflow(context.Background(), wf.ID, w.id, errMsg)
	} else {
		w.store.ReleaseWorkflow(context.Background(), wf.ID, w.id, wf.NextWakeAt)
	}
}

// dbServiceCaller implements host.ServiceCaller for the worker.
type dbServiceCaller struct {
	store    *host.PostgresStore
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
