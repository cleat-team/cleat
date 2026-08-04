package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"golang.org/x/time/rate"
)

//go:embed web/dist
var webDist embed.FS

// signalMaxBodySize is the maximum request body size for signal and update
// endpoints (64 KB). General endpoints use the configurable --max-body-size.
const signalMaxBodySize = 65536

// globalWorker is set during worker startup for access from HTTP handlers
// that cannot easily receive a *Worker parameter (e.g. handleMetrics).
var globalWorker *Worker

// fetchRequest is the JSON payload for DurableFetch calls.
type fetchRequest struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

type createDefRequest struct {
	Name        string            `json:"name"`
	Version     int               `json:"version,omitempty"`
	WASMBase64  string            `json:"wasm_bytes_base64"`
	EntryPoints []string          `json:"entry_points,omitempty"`
	PluginDeps  map[string]string `json:"plugin_deps,omitempty"`
}

// ---- HTTP API server ----

type apiServer struct {
	store       engine.WorkflowStore
	worker      *Worker
	maxBodySize int64
	db          *sql.DB
}

func (s *apiServer) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *apiServer) writeError(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, map[string]string{"error": msg})
}

func (s *apiServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	stale := s.worker.healthTracker.staleLoops()
	if len(stale) > 0 {
		s.writeJSON(w, 503, map[string]any{
			"ok":          false,
			"stale_loops": stale,
			"reason":      "background_loop_stuck",
		})
		return
	}
	if s.worker.memoryController != nil && s.worker.memoryController.Pressure() > 0 {
		s.writeJSON(w, 200, map[string]any{
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
	s.worker.inflight.Range(func(_, _ any) bool {
		count++
		return true
	})

	s.writeJSON(w, 202, map[string]any{
		"draining":  true,
		"in_flight": count,
	})
}

// handleDrainStatus returns the current drain status.
func (s *apiServer) handleDrainStatus(w http.ResponseWriter, r *http.Request) {
	draining := s.worker.draining.Load()

	count := 0
	s.worker.inflight.Range(func(_, _ any) bool {
		count++
		return true
	})

	resp := map[string]any{
		"draining":  draining,
		"in_flight": count,
	}

	if draining && count == 0 {
		s.worker.drainOnce.Do(func() {
			close(s.worker.drainCh)
			s.worker.cancel()
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
	filter := engine.WorkflowFilter{
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
		workflows = []engine.WorkflowInstance{}
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
	case len(parts) == 2 && parts[1] == "retry" && r.Method == http.MethodPost:
		// POST /api/workflows/:id/retry
		s.handleWorkflowRetry(w, r, id)
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
	if s.worker.draining.Load() {
		s.writeError(w, 503, "worker is draining; cannot accept new workflows")
		return
	}
	if s.worker.memoryController != nil && !s.worker.memoryController.CanAcceptAPIWorkflows() {
		s.writeError(w, 503, "worker is under memory pressure; cannot accept new workflows")
		return
	}
	if s.worker.maxQueued > 0 {
		depth, err := s.worker.store.QueueDepth(r.Context())
		if err == nil && depth >= int64(s.worker.maxQueued) {
			s.writeError(w, 503, fmt.Sprintf("queue full (%d ready, max %d); retry later", depth, s.worker.maxQueued))
			return
		}
		_ = err
	}

	var input struct {
		Input          json.RawMessage `json:"input"`
		EntryPoint     string          `json:"entry_point"`
		ConcurrencyKey string          `json:"concurrency_key"`
		TenantID       string          `json:"tenant_id"`
		Namespace      string          `json:"namespace"` // deprecated; use tenant_id
		Priority       int             `json:"priority"`
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

	// Resolve tenant_id: prefer tenant_id, fall back to deprecated namespace.
	tenantID := input.TenantID
	if tenantID == "" {
		tenantID = input.Namespace
	}
	if tenantID == "" {
		tenantID = engine.DefaultTenantUUID
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

	// Check A/B routing rules: if a routing rule matches, use the specified
	// version instead of the default latest.
	targetVersion := versions[0] // default: latest
	if routedVersion, routedErr := s.store.PickVersionByRouting(r.Context(), name); routedErr == nil && routedVersion > 0 {
		targetVersion = routedVersion
		slog.InfoContext(r.Context(), "A/B routing applied",
			"workflow", name,
			"routed_version", routedVersion,
			"latest_version", versions[0],
		)
	}

	// Inject entry point into input if provided.
	in := input.Input
	if input.EntryPoint != "" {
		var originalInput map[string]any
		json.Unmarshal(input.Input, &originalInput)
		in, _ = json.Marshal(map[string]any{
			"input":         originalInput,
			"__entry_point": input.EntryPoint,
		})
	}

	// Support Concurrency-Key header or JSON body field (Feature 5).
	concurrencyKey := r.Header.Get("Cleat-Concurrency-Key")
	if concurrencyKey == "" {
		concurrencyKey = input.ConcurrencyKey
	}

	// Support Idempotency-Key header for exactly-once semantics.
	idempotencyKey := r.Header.Get("Idempotency-Key")
	// Redact sensitive fields in the input before storing.
	in = json.RawMessage(engine.Redact(string(in)))
	runID, alreadyExisted, err := s.store.StartNewRun(r.Context(), "", name, targetVersion, in, idempotencyKey, tenantID, input.Priority)
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	if alreadyExisted {
		s.writeJSON(w, 200, map[string]string{"workflow_id": runID, "already_started": "true"})
		return
	}

	// Propagate W3C trace context from incoming request.
	if traceID := extractTraceIDFromTraceParent(r.Header.Get("traceparent")); traceID != "" {
		if err := s.store.TraceWorkflow(r.Context(), runID, traceID); err != nil {
			slog.Warn("failed to set trace_id", "run_id", runID, "error", err)
		}
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
			// Key already held by another workflow — reject the new run and
			// return conflict.
			//
			// This must not use FailWorkflow. That is the owning worker's
			// terminal write, fenced on `assigned_to = $2 AND generation = $7`,
			// and the run StartNewRun just inserted is 'ready' with
			// assigned_to NULL — so the UPDATE matched zero rows for any
			// arguments this caller could pass, returned ErrFenceLost, and
			// the error was discarded. The client got a 409 saying the run
			// was rejected while the run stayed claimable, and the next
			// worker to poll executed it. The HTTP layer is the only
			// enforcement point for Cleat-Concurrency-Key; ClaimWorkflows
			// does not consult concurrency_keys. See
			// engine/fence_lost_callers_test.go.
			//
			// TerminateWorkflow is the unowned-writer primitive: it matches
			// on id alone and bumps generation, so it also wins the race
			// against a worker that claimed the run in the window since
			// StartNewRun — that worker's own fenced write then returns
			// ErrFenceLost and it stops.
			if err := s.store.TerminateWorkflow(context.Background(), runID, "concurrency key conflict: "+concurrencyKey); err != nil {
				// The run is live and will execute despite the 409 below.
				// Report it rather than letting the client believe the key
				// was enforced.
				slog.ErrorContext(r.Context(), "concurrency key conflict: could not reject the losing run, it remains runnable",
					"run_id", runID, "concurrency_key", concurrencyKey, "error", err)
				s.writeError(w, 500, "workflow already running with key "+concurrencyKey+", and the losing run could not be rejected: "+err.Error())
				return
			}
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
	payload = engine.Redact(payload)
	// Check signal authorization for HTTP API callers.
	// External callers have no defName; they must be allowed via "*" wildcard.
	if s.worker.requireSignalAuth != nil && *s.worker.requireSignalAuth {
		callers, err := s.store.GetAllowedSignalCallers(r.Context(), id)
		if err != nil {
			s.writeError(w, 500, err.Error())
			return
		}
		if !signalCallerAllowed(callers, "*") {
			s.writeError(w, 403, "signal auth denied: external HTTP callers not in allowed_signals (add \"*\" to allow)")
			return
		}
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
	offset := 0
	limit := 1000
	if v, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && v >= 0 {
		offset = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = v
	}
	if limit > 1000 {
		limit = 1000
	}

	total, err := s.store.CountEventHistory(r.Context(), id)
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}

	history, err := s.store.LoadEventHistoryPaginated(r.Context(), id, offset, limit)
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	if history == nil {
		history = []engine.EventRecord{}
	}

	w.Header().Set("X-Total-Count", strconv.Itoa(total))
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
	var dagData map[string]any
	if err := json.Unmarshal(spec, &dagData); err != nil {
		s.writeError(w, 500, "invalid dag_spec JSON: "+err.Error())
		return
	}

	response := map[string]any{
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
		promises = []engine.PromiseInfo{}
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
	payload = engine.Redact(payload)

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
	if ps, ok := s.store.(engine.PromiseStore); ok {
		if pErr := ps.CreatePromise(r.Context(), id, "update:"+updateName, promiseID); pErr != nil {
			slog.Warn("failed to create promise for update", "worker_id", id, "update_name", updateName, "error", pErr)
		}
	}

	s.writeJSON(w, 202, map[string]string{"promise_id": promiseID})
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
		schedules = []engine.Schedule{}
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
	sch := engine.Schedule{
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
	memoryStats := make(map[string]*engine.WorkflowMemoryStats)
	if stats, err := s.store.LoadMemoryStats(r.Context()); err == nil {
		for i := range stats {
			memoryStats[stats[i].DefName] = &stats[i]
		}
	}

	type defResponse struct {
		Name            string                      `json:"name"`
		Version         int                         `json:"version"`
		ABIVersion      int                         `json:"abi_version"`
		MinVersion      int                         `json:"min_version"`
		CreatedAt       time.Time                   `json:"created_at"`
		Deprecated      bool                        `json:"deprecated"`
		ActiveInstances int                         `json:"active_instances"`
		Memory          *engine.WorkflowMemoryStats `json:"memory,omitempty"`
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

func (s *apiServer) handleCreateDefinition(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 10*1024*1024)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body too large"})
		return
	}

	var req createDefRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}

	if req.Name == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	if req.WASMBase64 == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "wasm_bytes_base64 is required"})
		return
	}

	wasmBytes, err := base64.StdEncoding.DecodeString(req.WASMBase64)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid base64: " + err.Error()})
		return
	}

	ctx := r.Context()

	// Auto-increment version if not specified
	version := req.Version
	if version <= 0 {
		defs, err := s.store.ListWorkflowDefs(ctx, req.Name)
		if err == nil {
			for _, d := range defs {
				if d.Version >= version {
					version = d.Version + 1
				}
			}
		}
		if version <= 0 {
			version = 1
		}
	}

	def := &engine.WorkflowDef{
		Name:       req.Name,
		Version:    version,
		WASMBytes:  wasmBytes,
		ABIVersion: 1,
		PluginDeps: req.PluginDeps,
	}

	if err := s.store.DeployWorkflowDef(ctx, def); err != nil {
		slog.Error("deploy workflow def failed", "error", err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to deploy: " + err.Error()})
		return
	}

	// Invalidate WASM cache
	s.worker.wasmCache.remove(fmt.Sprintf("%s:%d", def.Name, def.Version))

	s.writeJSON(w, http.StatusCreated, map[string]any{
		"name":        def.Name,
		"version":     def.Version,
		"plugin_deps": def.PluginDeps,
		"created":     true,
	})
}

func (s *apiServer) inflightCount() int {
	count := 0
	s.worker.inflight.Range(func(_, _ any) bool { count++; return true })
	return count
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

// getPluginDB returns the plugin DB adapter, using pluginDB if available
// (separate pool), falling back to the main db otherwise.
func getPluginDB(db, pluginDB *sql.DB) *engine.SQLDBAdapter {
	if pluginDB != nil {
		return &engine.SQLDBAdapter{DB: pluginDB}
	}
	return &engine.SQLDBAdapter{DB: db}
}

// getPluginReadOnlyDB returns the read-only plugin DB adapter, using pluginDB
// if available (separate pool), falling back to the main db otherwise.
func getPluginReadOnlyDB(db, pluginDB *sql.DB) *engine.ReadOnlyDB {
	if pluginDB != nil {
		return &engine.ReadOnlyDB{Inner: pluginDB}
	}
	return &engine.ReadOnlyDB{Inner: db}
}

// ---- Rate limiter ----

// ipRateLimiter provides per-IP token-bucket rate limiting using a sync.Map
// with periodic cleanup of stale entries.
type rateLimiterEntry struct {
	limiter  *rate.Limiter
	lastUsed time.Time
}

type ipRateLimiter struct {
	mu     sync.Mutex
	limits map[string]*rateLimiterEntry
	rate   rate.Limit
	burst  int
	stopCh chan struct{}
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

// keyedRateLimiter provides per-key token-bucket rate limiting. Each key
// (tenant UUID, API key hash, etc.) gets its own rate.Limiter with the given
// rate and burst. Includes periodic cleanup of stale entries.
type keyedRateLimiter struct {
	mu     sync.Mutex
	limits map[string]*rateLimiterEntry
	stopCh chan struct{}
}

func newKeyedRateLimiter() *keyedRateLimiter {
	rl := &keyedRateLimiter{
		limits: make(map[string]*rateLimiterEntry),
		stopCh: make(chan struct{}),
	}
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				rl.mu.Lock()
				now := time.Now()
				for key, entry := range rl.limits {
					if now.Sub(entry.lastUsed) > time.Hour {
						delete(rl.limits, key)
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

func (rl *keyedRateLimiter) stop() {
	close(rl.stopCh)
}

// allow checks whether the key has a token available. If the key has no
// existing limiter, one is created with the given rate and burst.
func (rl *keyedRateLimiter) allow(key string, r rate.Limit, burst int) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	entry, ok := rl.limits[key]
	if !ok {
		entry = &rateLimiterEntry{
			limiter: rate.NewLimiter(r, burst),
		}
		rl.limits[key] = entry
	}
	entry.lastUsed = time.Now()
	return entry.limiter.Allow()
}

// write429 sends a JSON 429 Too Many Requests response.
func write429(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "1")
	w.WriteHeader(http.StatusTooManyRequests)
	json.NewEncoder(w).Encode(map[string]any{
		"error":          msg,
		"retry_after_ms": 1000,
	})
}

// rateLimitMiddleware returns an HTTP middleware that rate-limits requests
// per client IP, and optionally per tenant. On rate limit exceeded, it returns
// HTTP 429 with a JSON error body and a Retry-After header.
func rateLimitMiddleware(ipLim *ipRateLimiter, tenantLim *keyedRateLimiter, tenantRate rate.Limit, tenantBurst int) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Tier 1: IP-based rate limit (always active).
			if !ipLim.allow(clientIP(r)) {
				write429(w, "rate limit exceeded")
				return
			}
			// Tier 2: per-tenant rate limit (only when configured).
			if tenantRate > 0 {
				if tid, ok := auth.TenantIDFromContext(r.Context()); ok {
					if !tenantLim.allow(tid.String(), tenantRate, tenantBurst) {
						write429(w, "tenant rate limit exceeded")
						return
					}
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ---- Throughput gauge state ----
var (
	lastReplayStepCount int64
	lastFreshStepCount  int64
	lastFreshCallCount  int64
	lastThroughputTime  time.Time
)

// updateThroughputGauges computes the current replay and fresh step throughput
// from the atomic step counters in the host package and sets the gauges.
func updateThroughputGauges() {
	now := time.Now()
	elapsed := now.Sub(lastThroughputTime).Seconds()
	if elapsed < 1 {
		return
	}
	replayCur := engine.ReplayStepCount()
	freshStepCur := engine.FreshStepCount()
	freshCallCur := engine.FreshCallCount()
	if lastThroughputTime.IsZero() {
		lastReplayStepCount = replayCur
		lastFreshStepCount = freshStepCur
		lastFreshCallCount = freshCallCur
		lastThroughputTime = now
		return
	}
	replayDelta := float64(replayCur - lastReplayStepCount)
	freshCallDelta := float64(freshCallCur - lastFreshCallCount)
	if globalWorker != nil && globalWorker.Metrics != nil {
		globalWorker.Metrics.SetReplayThroughput(context.Background(), replayDelta/elapsed)
		globalWorker.Metrics.SetFreshThroughput(context.Background(), freshCallDelta/elapsed)
		globalWorker.Metrics.SetFreshStepCount(context.Background(), freshStepCur)
		globalWorker.Metrics.SetReplayStepCount(context.Background(), replayCur)
	}
	lastReplayStepCount = replayCur
	lastFreshStepCount = freshStepCur
	lastFreshCallCount = freshCallCur
	lastThroughputTime = now
}
