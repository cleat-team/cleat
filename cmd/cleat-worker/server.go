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
	// store is the process-wide store, opened at startup against
	// engine.DefaultTenantUUID. It is NOT safe to serve authenticated requests
	// from: it is scoped to the default tenant, so every caller would read and
	// write that one tenant's data regardless of who they authenticated as.
	// Handlers must go through storeFor. See the storeFor doc comment.
	store       engine.WorkflowStore
	worker      *Worker
	maxBodySize int64
	db          *sql.DB

	// factory opens per-tenant stores. Every backend already implements the
	// tenant scoping this needs -- PostgreSQL sets cleat.tenant_id via
	// set_config so its RLS policies apply, SQL Server hands out a per-tenant
	// pool whose connector calls sp_set_session_context, and MySQL routes to a
	// per-tenant database. All three cache their pools, so opening a store per
	// request is a struct allocation and a map lookup, not a new connection.
	factory engine.StoreFactory

	// taskQueues is passed through to factory.OpenStore so a request-scoped
	// store polls the same queues as the process-wide one.
	taskQueues []string

	// requireAuth mirrors --require-auth. It decides what an unauthenticated
	// request means: with auth on, no tenant in context is a bug or a bypass
	// attempt and is refused; with auth off, there is only ever one tenant and
	// the default-tenant store is correct. See storeFor.
	requireAuth bool
}

// errNoTenant is returned by storeFor when a request carries no authenticated
// tenant and the server is configured to require one.
var errNoTenant = errors.New("request has no authenticated tenant")

// storeFor returns a WorkflowStore scoped to the tenant that authenticated
// this request.
//
// This is the fix for the defect where callers authenticated per-tenant and
// were then all served from one hardcoded scope: apiServer.store is opened once
// at startup against engine.DefaultTenantUUID, so a handler using it directly
// reads and writes the default tenant's rows no matter who is calling. The real
// row-level security underneath is not bypassed by being defeated -- it is
// bypassed by being handed the wrong tenant, which it then enforces faithfully.
//
// Failure is closed. When --require-auth is on, a request that reached a
// handler without a tenant in its context does not fall back to the default
// tenant; it is refused. The fallback is what the bug was.
//
// When --require-auth is off there is no tenant to scope to and only one tenant
// exists, so the process-wide store is returned deliberately. That is the single
// documented path to the default scope, rather than the previous situation where
// every handler took it silently.
func (s *apiServer) storeFor(r *http.Request) (engine.WorkflowStore, error) {
	tid, ok := auth.TenantIDFromContext(r.Context())
	if !ok {
		if s.requireAuth {
			return nil, errNoTenant
		}
		return s.store, nil
	}

	// A tenant is present but nothing can scope to it. Refusing is the only
	// safe answer: returning s.store here would serve one tenant's request
	// from another tenant's data, which is the exact defect this function
	// exists to close.
	if s.factory == nil {
		return nil, fmt.Errorf("no store factory configured, cannot scope request to tenant %s", tid)
	}

	st, _, err := s.factory.OpenStore(r.Context(), tid.String(), s.taskQueues...)
	if err != nil {
		return nil, fmt.Errorf("open store for tenant %s: %w", tid, err)
	}
	return st, nil
}

// tenantFor returns the tenant ID a request's writes should be attributed to:
// the authenticated tenant when there is one, and the default tenant otherwise
// (which storeFor only permits when --require-auth is off).
//
// Handlers that pass a tenant to the store as a *value* need this rather than
// storeFor alone. Scoping the store is not sufficient for those: StartNewRun
// takes a tenantID argument, so a scoped store still writes wherever that
// argument says.
func (s *apiServer) tenantFor(r *http.Request) string {
	if tid, ok := auth.TenantIDFromContext(r.Context()); ok {
		return tid.String()
	}
	return engine.DefaultTenantUUID
}

// scopedStore resolves the request's tenant-scoped store and writes the error
// response itself when it cannot, returning ok=false. Handlers use this so the
// refusal is uniform and cannot be forgotten at an individual call site.
//
// The status codes distinguish the two failures: a missing tenant is the
// caller's problem (401), while a factory that cannot open a store for a tenant
// that did authenticate is the server's (500).
func (s *apiServer) scopedStore(w http.ResponseWriter, r *http.Request) (engine.WorkflowStore, bool) {
	st, err := s.storeFor(r)
	if err != nil {
		if errors.Is(err, errNoTenant) {
			s.writeError(w, http.StatusUnauthorized, "authentication required")
			return nil, false
		}
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	return st, true
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
	st, ok := s.scopedStore(w, r)
	if !ok {
		return
	}
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
	workflows, err := st.ListWorkflows(r.Context(), filter)
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
	case len(parts) == 2 && parts[1] == "allowed-signals" && r.Method == http.MethodGet:
		// GET /api/workflows/:id/allowed-signals
		s.handleGetAllowedSignals(w, r, id)
	case len(parts) == 2 && parts[1] == "allowed-signals" && r.Method == http.MethodPut:
		// PUT /api/workflows/:id/allowed-signals
		s.handleSetAllowedSignals(w, r, id)
	case len(parts) == 3 && parts[1] == "update" && r.Method == http.MethodPost:
		// POST /api/workflows/:id/update/:name
		s.handleWorkflowUpdate(w, r, id, parts[2])
	default:
		s.writeError(w, 404, "not found")
	}
}

func (s *apiServer) handleGetWorkflow(w http.ResponseWriter, r *http.Request, id string) {
	st, ok := s.scopedStore(w, r)
	if !ok {
		return
	}
	// Check if this is a query state request.
	if key := r.URL.Query().Get("key"); key != "" {
		value, err := st.GetQueryState(r.Context(), id, key)
		if err != nil {
			s.writeError(w, 500, err.Error())
			return
		}
		s.writeJSON(w, 200, map[string]string{"key": key, "value": value})
		return
	}

	// Return full workflow info.
	wf, err := st.GetWorkflowByID(r.Context(), id)
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
	st, ok := s.scopedStore(w, r)
	if !ok {
		return
	}
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
	//
	// The authenticated tenant is authoritative and overrides both. Before this,
	// tenantID came from the request body unchecked, so any authenticated caller
	// could start a workflow in any tenant by naming it in the JSON -- a
	// cross-tenant *write*, which scoping the store alone does not close because
	// StartNewRun takes the tenant as an argument.
	//
	// A body value that disagrees is refused rather than quietly overridden: a
	// caller that asked for another tenant has either misconfigured something or
	// is probing, and silently writing somewhere other than where they asked is
	// its own bug. When auth is off there is no authenticated tenant to compare
	// against and the body value stands, which is how single-tenant and local
	// deployments address a tenant at all.
	tenantID := input.TenantID
	if tenantID == "" {
		tenantID = input.Namespace
	}
	if authTenant, ok := auth.TenantIDFromContext(r.Context()); ok {
		if tenantID != "" && tenantID != authTenant.String() {
			s.writeError(w, http.StatusForbidden, "tenant_id does not match the authenticated tenant")
			return
		}
		tenantID = authTenant.String()
	} else if tenantID == "" {
		tenantID = engine.DefaultTenantUUID
	}

	// Find the latest version of this workflow.
	versions, err := st.ListVersions(r.Context(), name)
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
	if routedVersion, routedErr := st.PickVersionByRouting(r.Context(), name); routedErr == nil && routedVersion > 0 {
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
	runID, alreadyExisted, err := st.StartNewRun(r.Context(), "", name, targetVersion, in, idempotencyKey, tenantID, input.Priority)
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
		if err := st.TraceWorkflow(r.Context(), runID, traceID); err != nil {
			slog.Warn("failed to set trace_id", "run_id", runID, "error", err)
		}
	}

	// If concurrency key is specified, try to acquire it for the new run.
	if concurrencyKey != "" {
		ttl := 30 * time.Minute
		acquired, err := st.AcquireConcurrencyKey(r.Context(), concurrencyKey, runID, ttl)
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
			// TerminateWorkflow is the unowned-writer primitive: it does not
			// fence on assigned_to or generation, and it bumps generation, so
			// it wins the race against a worker that claimed the run in the
			// window since StartNewRun — that worker's own fenced write then
			// returns ErrFenceLost and it stops.
			//
			// This used to say "it matches on id alone", and that stopped
			// being true in 3.86: MySQL and SQL Server now carry an explicit
			// `AND tenant_id`, and PostgreSQL's runs inside beginTxWithRLS
			// where the policy narrows it to one tenant. Nothing here breaks
			// -- runID was created by the StartNewRun above on this same
			// request-scoped store, so the tenant matches by construction --
			// but the property the sentence named is gone, and it is the
			// property a reader would rely on when moving this call. What is
			// still true is the fencing half, which is what makes it work
			// here.
			if err := st.TerminateWorkflow(context.Background(), runID, "concurrency key conflict: "+concurrencyKey); err != nil {
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
	// callerOwnsTarget, and it must run BEFORE the allowed-signals check below.
	// That check answers 403, which confirms the workflow exists -- fine for a
	// caller who owns it, an existence oracle for one who does not.
	//
	// Delivering to another tenant's id used to reach the store and fail with a
	// primary-key violation surfaced as a 500 (3.86): safe, because the MERGE's
	// ON clause is tenant-scoped, but a 500 distinguishable from the 201 an
	// unknown id produced. Same 404 for both now.
	st, ok := s.callerOwnsTarget(w, r, id)
	if !ok {
		return
	}
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
		callers, err := st.GetAllowedSignalCallers(r.Context(), id)
		if err != nil {
			s.writeError(w, 500, err.Error())
			return
		}
		if !signalCallerAllowed(callers, "*") {
			s.writeError(w, 403, "signal auth denied: external HTTP callers not in allowed_signals (add \"*\" to allow)")
			return
		}
	}
	if err := st.DeliverSignal(r.Context(), id, req.SignalName, payload); err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	s.writeJSON(w, 200, map[string]string{"status": "delivered"})
}

func (s *apiServer) handleCancel(w http.ResponseWriter, r *http.Request, id string) {
	// callerOwnsTarget, matching terminate and signal (3.101). Cancel was the
	// third route taking an id from the URL path and not checking it: 3.86
	// scoped RequestCancellation's UPDATE so nothing crossed, but the handler
	// reported 200 "cancellation_requested" for a workflow it had not
	// cancelled -- and for one that does not exist at all.
	//
	// Its two siblings turned out NOT to need this, which is why only one
	// route changes here. handleWorkflowRetry already reads the workflow back
	// through a tenant-scoped GetWorkflowByID and 404s when it is nil.
	// handleGetQueryState needs nothing: GetQueryState returns ("", nil) for no
	// rows on all three dialects, so a foreign id, an unknown id and a real
	// workflow with that key unset are already the same 200 with an empty
	// value -- indistinguishable, which is the property that matters.
	st, ok := s.callerOwnsTarget(w, r, id)
	if !ok {
		return
	}
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
	if err := st.RequestCancellation(r.Context(), id, req.Reason); err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	s.writeJSON(w, 200, map[string]string{"status": "cancellation_requested"})
}

func (s *apiServer) handleGetHistory(w http.ResponseWriter, r *http.Request, id string) {
	st, ok := s.scopedStore(w, r)
	if !ok {
		return
	}
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

	total, err := st.CountEventHistory(r.Context(), id)
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}

	history, err := st.LoadEventHistoryPaginated(r.Context(), id, offset, limit)
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
	st, ok := s.scopedStore(w, r)
	if !ok {
		return
	}
	key := r.URL.Query().Get("key")
	value, err := st.GetQueryState(r.Context(), id, key)
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	s.writeJSON(w, 200, map[string]string{"key": key, "value": value})
}

func (s *apiServer) handleGetDAG(w http.ResponseWriter, r *http.Request, id string) {
	st, ok := s.scopedStore(w, r)
	if !ok {
		return
	}
	// Look up the workflow instance to get def_name and def_version.
	wf, err := st.GetWorkflowByID(r.Context(), id)
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	if wf == nil {
		s.writeError(w, 404, "workflow not found")
		return
	}

	// Load the dag_spec from workflow_defs.
	spec, err := st.LoadDAGSpec(r.Context(), wf.DefName, wf.DefVersion)
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

// handleGetAllowedSignals handles GET /api/workflows/:id/allowed-signals
//
// Returns the list --require-signal-auth checks a caller against. Always a JSON
// array: an unset column reads back as [], not null, so a client can tell
// "denies everyone" from "the field is missing" without special-casing.
func (s *apiServer) handleGetAllowedSignals(w http.ResponseWriter, r *http.Request, id string) {
	st, ok := s.scopedStore(w, r)
	if !ok {
		return
	}
	callers, err := st.GetAllowedSignalCallers(r.Context(), id)
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	if callers == nil {
		callers = []string{}
	}
	s.writeJSON(w, 200, map[string]any{"allowed_signals": callers})
}

// handleSetAllowedSignals handles PUT /api/workflows/:id/allowed-signals
//
// The writer IMPROVEMENT-PLAN 3.15 is about. Until this existed, nothing in the
// product could populate workflow_instances.allowed_signals, so enabling
// --require-signal-auth denied every signal and the documented remedy -- "add
// \"*\" to allowed_signals" -- named something no interface could do.
//
// PUT rather than POST because it replaces the whole list, which is what the
// store method does and what makes the result of two concurrent grants
// predictable rather than order-dependent.
func (s *apiServer) handleSetAllowedSignals(w http.ResponseWriter, r *http.Request, id string) {
	st, ok := s.scopedStore(w, r)
	if !ok {
		return
	}
	var req struct {
		AllowedSignals []string `json:"allowed_signals"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodySize)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.writeError(w, 413, "request body too large")
			return
		}
		s.writeError(w, 400, "invalid JSON body")
		return
	}
	for _, c := range req.AllowedSignals {
		if c == "" {
			s.writeError(w, 400, "allowed_signals entries must be non-empty (use \"*\" to allow any caller)")
			return
		}
	}
	if err := st.SetAllowedSignalCallers(r.Context(), id, req.AllowedSignals); err != nil {
		// 404 for a workflow this tenant cannot see, which is the same answer
		// as for one that does not exist -- see engine.ErrWorkflowNotFound.
		if errors.Is(err, engine.ErrWorkflowNotFound) {
			s.writeError(w, 404, "workflow not found")
			return
		}
		s.writeError(w, 500, err.Error())
		return
	}
	s.writeJSON(w, 200, map[string]any{"allowed_signals": req.AllowedSignals})
}

// handleListPromises handles GET /api/workflows/:id/promises
func (s *apiServer) handleListPromises(w http.ResponseWriter, r *http.Request, id string) {
	st, ok := s.scopedStore(w, r)
	if !ok {
		return
	}
	promises, err := st.ListPromises(r.Context(), id)
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
	st, ok := s.scopedStore(w, r)
	if !ok {
		return
	}
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
	if err := st.ResolvePromise(r.Context(), id, promiseID, req.Result); err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	s.writeJSON(w, 200, map[string]string{"status": "resolved"})
}

// handleRejectPromise handles POST /api/workflows/:id/promises/:promiseId/reject
func (s *apiServer) handleRejectPromise(w http.ResponseWriter, r *http.Request, id, promiseID string) {
	st, ok := s.scopedStore(w, r)
	if !ok {
		return
	}
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
	if err := st.RejectPromise(r.Context(), id, promiseID, req.Reason); err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	s.writeJSON(w, 200, map[string]string{"status": "rejected"})
}

// handleWorkflowUpdate handles POST /api/workflows/:id/update/:name
func (s *apiServer) handleWorkflowUpdate(w http.ResponseWriter, r *http.Request, id, updateName string) {
	st, ok := s.scopedStore(w, r)
	if !ok {
		return
	}
	// Verify the workflow exists.
	wf, err := st.GetWorkflowByID(r.Context(), id)
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
	pending, pErr := st.GetPendingUpdateRequests(r.Context(), id)
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
	if err := st.CreateUpdateRequest(r.Context(), id, updateName, payload, promiseID); err != nil {
		s.writeError(w, 500, err.Error())
		return
	}

	// Create an associated promise record so the caller can poll for the result.
	if ps, ok := st.(engine.PromiseStore); ok {
		if pErr := ps.CreatePromise(r.Context(), id, "update:"+updateName, promiseID); pErr != nil {
			slog.Warn("failed to create promise for update", "worker_id", id, "update_name", updateName, "error", pErr)
		}
	}

	s.writeJSON(w, 202, map[string]string{"promise_id": promiseID})
}

// ---- Schedule API handlers ----

// handleSchedulesList handles GET /api/schedules and POST /api/schedules
func (s *apiServer) handleSchedulesList(w http.ResponseWriter, r *http.Request) {
	st, ok := s.scopedStore(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodPost {
		s.handleCreateSchedule(w, r)
		return
	}
	if r.Method != http.MethodGet {
		s.writeError(w, 405, "method not allowed")
		return
	}
	schedules, err := st.ListSchedules(r.Context())
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
	st, ok := s.scopedStore(w, r)
	if !ok {
		return
	}
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
		if err := st.SetScheduleEnabled(r.Context(), name, true); err != nil {
			s.writeError(w, 500, err.Error())
			return
		}
		s.writeJSON(w, 200, map[string]string{"status": "enabled"})
	case len(parts) == 2 && parts[1] == "disable" && r.Method == http.MethodPost:
		if err := st.SetScheduleEnabled(r.Context(), name, false); err != nil {
			s.writeError(w, 500, err.Error())
			return
		}
		s.writeJSON(w, 200, map[string]string{"status": "disabled"})
	case len(parts) == 1 && r.Method == http.MethodDelete:
		if err := st.DeleteSchedule(r.Context(), name); err != nil {
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
	st, ok := s.scopedStore(w, r)
	if !ok {
		return
	}
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
		Timezone   string          `json:"timezone"`
		Misfire    string          `json:"misfire_policy"`
		CatchUp    int             `json:"catch_up_limit"`
		Overlap    string          `json:"overlap_policy"`
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
	// Validate before storing, not after. A cron expression that does not
	// parse used to be accepted here and then silently fall back to firing
	// daily, and an unloadable timezone would silently fall back to UTC --
	// both at some later point, in the scheduler, where there is no longer
	// anyone to report them to. 400 at the door is the only place the caller
	// can be told.
	if err := engine.ValidateCronExpr(req.Cron); err != nil {
		s.writeError(w, 400, err.Error())
		return
	}
	if err := engine.ValidateTimezone(req.Timezone); err != nil {
		s.writeError(w, 400, err.Error())
		return
	}
	if err := engine.ValidateMisfirePolicy(req.Misfire); err != nil {
		s.writeError(w, 400, err.Error())
		return
	}
	if err := engine.ValidateOverlapPolicy(req.Overlap); err != nil {
		s.writeError(w, 400, err.Error())
		return
	}
	if req.CatchUp < 0 {
		s.writeError(w, 400, "catch_up_limit must not be negative")
		return
	}
	sch := engine.Schedule{
		Name:           req.Name,
		DefName:        req.DefName,
		EntryPoint:     req.EntryPoint,
		CronExpression: req.Cron,
		Input:          req.Input,
		Enabled:        true,
		Timezone:       req.Timezone,
		MisfirePolicy:  req.Misfire,
		CatchUpLimit:   req.CatchUp,
		OverlapPolicy:  req.Overlap,
	}
	if err := st.CreateSchedule(r.Context(), sch); err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	s.writeJSON(w, 201, map[string]string{"status": "created"})
}

// handleDefinitions handles GET /api/definitions
func (s *apiServer) handleDefinitions(w http.ResponseWriter, r *http.Request) {
	st, ok := s.scopedStore(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		s.writeError(w, 405, "method not allowed")
		return
	}

	defs, err := st.ListWorkflowDefs(r.Context(), "")
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}

	// Load memory stats for enrichment.
	memoryStats := make(map[string]*engine.WorkflowMemoryStats)
	if stats, err := st.LoadMemoryStats(r.Context()); err == nil {
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
		count, _ := st.CountActiveInstances(r.Context(), def.Name, def.Version)
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
	st, ok := s.scopedStore(w, r)
	if !ok {
		return
	}
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
		defs, err := st.ListWorkflowDefs(ctx, req.Name)
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

	if err := st.DeployWorkflowDef(ctx, def); err != nil {
		// The 409 arm that used to be here is gone with the shared namespace it
		// existed for: a name another tenant holds is no longer a conflict,
		// because each tenant has its own (tenant_id, name, version) row and a
		// deploy simply creates or updates the caller's own. IMPROVEMENT-PLAN
		// 3.77 / D7.
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
