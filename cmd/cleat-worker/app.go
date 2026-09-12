package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/cleat-team/cleat/engine"
)

// registerRoutes attaches all API routes to the given mux.
//
// This is the ONLY route table. It used to be one of two: main() registered its
// own set inline and reached this function not at all, so `/api/instances/...`
// and every `/api/admin/instances/...` route -- instance state, event history,
// force-complete, force-fail, re-replay, step resolve -- existed on the table
// the tests drove and on no table the binary served. Seven endpoints, one of
// them documented in CHANGELOG.md as a shipped operator feature, none of them
// reachable since app.go was written (ce48f18). `--enable-admin-api` gated a
// handler nothing routed to.
//
// Nothing reported it because of the SPA fallback below, which answered every
// unmatched path -- `/api/` included -- with 200 and index.html. A JSON client
// asking for a missing endpoint got HTML and a parse error naming nothing,
// rather than a 404 naming the path.
func registerRoutes(mux *http.ServeMux, api *apiServer) *http.ServeMux {
	mux.HandleFunc("/healthz", api.handleHealthz)
	mux.HandleFunc("/metrics", handleMetrics)
	mux.HandleFunc("/api/admin/drain", api.handleDrain)
	mux.HandleFunc("/api/admin/retention/sweep", api.handleRetentionSweep)
	// Schedule routes before workflow routes so /api/schedules is not caught
	// by /api/workflows/.
	mux.HandleFunc("/api/schedules/", api.handleSchedules)
	mux.HandleFunc("/api/schedules", api.handleSchedulesList)
	mux.HandleFunc("/api/workflows/", api.handleWorkflows)
	mux.HandleFunc("/api/workflows", api.handleWorkflowsList)
	mux.HandleFunc("/api/dead-letters/", api.handleDeadLetters)
	mux.HandleFunc("/api/dead-letters", api.handleDeadLettersList)

	// Workflow definitions.
	mux.HandleFunc("GET /api/definitions", api.handleDefinitions)
	mux.HandleFunc("POST /api/definitions", api.handleCreateDefinition)

	// Version management.
	//
	// api.scopedStore, not api.store: store is the process-wide connection
	// opened at boot against the default tenant, and passing it here served
	// every caller's version read and -- worst -- POST
	// /api/versions/<name>/<v>/purge from the default tenant's data regardless
	// of who authenticated.
	engine.RegisterVersionHandler(mux, api.scopedStore)

	// Instance inspection endpoints (always on behind auth).
	mux.HandleFunc("/api/instances/", api.handleInstancesRoutes)

	// Admin API endpoints. Destructive operations are additionally gated
	// behind --enable-admin-api at request time in handleAdminRoutes (see
	// api_admin.go), so the route itself can always be registered.
	mux.HandleFunc("/api/admin/instances/", api.handleAdminRoutes)

	// Plugin discovery, when the binary loaded plugins.
	if api.plugins != nil {
		mux.Handle("/api/plugins", api.plugins)
	}

	// The SPA catch-all, when the binary embeds one.
	if api.spa != nil {
		mux.Handle("/", apiAware404(api.spa))
	}
	return mux
}

// apiAware404 wraps the SPA handler so that an unmatched path under /api/ is a
// JSON 404 rather than the single-page app.
//
// Only UNMATCHED paths reach here: ServeMux prefers the longest matching
// pattern, so every registered /api/ route above still wins. What falls through
// is a path no handler claims -- a typo, a client built against a newer server,
// or a route that was never registered at all, which is the case that hid seven
// endpoints for the life of this file.
func apiAware404(spa http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
			return
		}
		spa.ServeHTTP(w, r)
	})
}

// ---- Dead Letter Queue handlers ----

func (s *apiServer) handleDeadLettersList(w http.ResponseWriter, r *http.Request) {
	st, ok := s.scopedStore(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		s.writeError(w, 405, "method not allowed")
		return
	}
	// Paging, mirroring handleWorkflowsList, which mirrors
	// handleGetInstanceEvents: both parameters read from the query, a server
	// ceiling applied rather than assumed, and the total sent as a header.
	//
	// Before this, Limit was hard-coded to 100 and Offset was never read -- the
	// sentence cleat#1182 wrote about /api/workflows, still true one endpoint
	// over (cleat#1166). A cap pretending to be a default is worse than a small
	// cap: a bare array of exactly 100 is indistinguishable from a store
	// holding exactly 100.
	//
	// It matters more here than it did there -- though less than cleat#1166's
	// headline said, and less than this comment said when #1232 added it.
	//
	// --completed-workflow-retention-days never touches a dead-lettered run,
	// correctly: it is the run an operator most wants to inspect afterwards.
	// But a SECOND knob deletes them -- --dead-letter-retention-days
	// (cleat#1023), which removes the row with its event_history, signals and
	// promises -- and it DEFAULTS TO 0, meaning off.
	//
	// So "the one class retention never deletes" is wrong. The true statement
	// is weaker and still sufficient: unbounded growth is a property of the
	// DEFAULT CONFIGURATION rather than of the design, which makes this the
	// listing most likely to be long on a deployment nobody has tuned.
	//
	// Both #1166 and cleat#1227 reached the wrong version from the same
	// sentence: DeleteCompletedWorkflows excludes dead_lettered with a comment
	// reading "it has its own lifecycle and its own deletion path", and two
	// readers took that as an explanation for the omission rather than as a
	// pointer to a path that exists. The refutation was inside the thing being
	// cited, which is why neither of us went looking.
	q := r.URL.Query()
	filter := engine.WorkflowFilter{Status: "dead_lettered", Limit: 100}
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
		filter.Limit = v
	}
	if filter.Limit > 1000 {
		filter.Limit = 1000
	}
	if v, err := strconv.Atoi(q.Get("offset")); err == nil && v >= 0 {
		filter.Offset = v
	}

	total, err := st.CountWorkflows(r.Context(), filter)
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	workflows, err := st.ListWorkflows(r.Context(), filter)
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}

	// Header rather than an envelope: the body stays a bare array, so no
	// existing caller breaks.
	w.Header().Set("X-Total-Count", strconv.Itoa(total))
	if workflows == nil {
		workflows = []engine.WorkflowInstance{}
	}
	s.writeJSON(w, 200, workflows)
}

func (s *apiServer) handleDeadLetters(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/dead-letters/")
	if path == "" || path == "/" {
		s.handleDeadLettersList(w, r)
		return
	}
	parts := strings.Split(strings.TrimSuffix(path, "/"), "/")
	if len(parts) < 1 {
		s.writeError(w, 400, "bad request")
		return
	}

	id := parts[0]

	switch {
	case len(parts) == 2 && parts[1] == "reprocess" && r.Method == http.MethodPost:
		s.handleDeadLetterReprocess(w, r, id)
	case len(parts) == 2 && parts[1] == "terminate" && r.Method == http.MethodPost:
		s.handleDeadLetterTerminate(w, r, id)
	default:
		s.writeError(w, 404, "not found")
	}
}

func (s *apiServer) handleDeadLetterReprocess(w http.ResponseWriter, r *http.Request, id string) {
	st, ok := s.scopedStore(w, r)
	if !ok {
		return
	}
	// Fetch the dead-lettered workflow instance.
	wf, err := st.GetWorkflowByID(r.Context(), id)
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	if wf == nil {
		s.writeError(w, 404, "workflow not found")
		return
	}
	if wf.Status != "dead_lettered" {
		s.writeError(w, 400, "workflow is not dead-lettered, status="+wf.Status)
		return
	}

	// Create a new run from the dead-lettered workflow's definition and input.
	versions, verr := st.ListVersions(r.Context(), wf.DefName)
	if verr != nil {
		s.writeError(w, 500, verr.Error())
		return
	}
	if len(versions) == 0 {
		s.writeError(w, 404, "workflow definition not found")
		return
	}

	// s.tenantFor, not engine.DefaultTenantUUID: reprocessing a dead-lettered
	// workflow re-created it under the default tenant regardless of whose
	// workflow it was, so a tenant's own retry moved its run into another
	// tenant's scope.
	// Reprocess honours Idempotency-Key for the same reason start does: a lost
	// response followed by a retry would otherwise re-drive work that already
	// failed partway, so partial side effects get repeated (cleat#1167). The
	// key is a client-supplied token, unique per (key_hash, tenant_id) -- the
	// caller never invents an id in the server's namespace.
	//
	// With no header this stays as it was: a new run per call. Deriving a key
	// from `id` would protect callers that send nothing, but it would also
	// refuse a DELIBERATE second re-drive -- fix the downstream, re-drive
	// again -- and answering that with the first run is worse than the
	// duplicate this guards against. Making reprocess idempotent by identity
	// removes an operation and needs to be its own decision.
	idempotencyKey := r.Header.Get("Idempotency-Key")
	runID, alreadyExisted, serr := st.StartNewRun(r.Context(), "", wf.DefName, versions[0], wf.Input, idempotencyKey, s.tenantFor(r), 0)
	if serr != nil {
		s.writeError(w, 500, serr.Error())
		return
	}
	if alreadyExisted {
		s.writeJSON(w, 200, map[string]string{"workflow_id": runID, "already_started": "true"})
		return
	}

	s.writeJSON(w, 201, map[string]string{"id": runID})
}

func (s *apiServer) handleDeadLetterTerminate(w http.ResponseWriter, r *http.Request, id string) {
	// callerOwnsTarget, not scopedStore. TerminateWorkflow's UPDATE carries
	// `AND tenant_id` since 3.86, so a foreign id already changed nothing --
	// but it changed nothing and returned 200, which reads to the caller as
	// "terminated" and to an operator as a workflow that ignored a terminate.
	// This answers 404 instead, and answers the SAME 404 for an id that does
	// not exist anywhere, which is the point: distinguishing the two would
	// turn this route into an oracle for which workflow ids are real.
	st, ok := s.callerOwnsTarget(w, r, id)
	if !ok {
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, int64(1<<10)) // 1 KB
		// The error was DISCARDED here until cleat#1337, and this is the only
		// request-body decode in the worker that did not branch on it. The
		// consequence was not that the operator's note went missing: it is that
		// the note is written straight into workflow_instances.error_msg by
		// TerminateWorkflow, unconditionally and in both of its branches, so an
		// empty reason OVERWRITES the failure message recording why the run
		// dead-lettered in the first place. Measured on one row: 270 bytes of
		// host error before, 0 after, and a 200 in between.
		//
		// The body that did it was 25 bytes of truncated JSON, nowhere near the
		// 1 KB cap above -- the cap is one way in, not the defect.
		//
		// io.EOF is carved out because an EMPTY body is a supported call: a
		// terminate with no reason at all. TestHandleDeadLetterTerminate_NoBody
		// has asserted 200 for a nil body since before this handler had any
		// error handling, and http.NoBody decodes to exactly io.EOF. A
		// truncated body is io.ErrUnexpectedEOF, which errors.Is(err, io.EOF)
		// does NOT match -- verified, because the whole fix turns on those two
		// being distinguishable.
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			// 400 rather than 413 for the oversized case, deliberately. Whether
			// an oversized body should be a 413 here is cleat#1338's decision
			// and it covers seven other sites with this exact shape; answering
			// it for one endpoint would make that decision twice.
			s.writeError(w, 400, "invalid JSON: "+err.Error())
			return
		}
	}
	if err := st.TerminateWorkflow(r.Context(), id, req.Reason); err != nil {
		// 3.92: the store now reports a terminate that matched nothing rather
		// than returning nil. callerOwnsTarget above has already answered 404
		// for an id this tenant does not own, so reaching here means the row
		// went away between the two -- still a 404, and the same one, because
		// ErrWorkflowNotFound does not distinguish "gone" from "not yours".
		if errors.Is(err, engine.ErrWorkflowNotFound) {
			s.writeError(w, 404, "not found")
			return
		}
		s.writeError(w, 500, err.Error())
		return
	}
	s.writeJSON(w, 200, map[string]string{"status": "terminated"})
}

func (s *apiServer) handleWorkflowRetry(w http.ResponseWriter, r *http.Request, id string) {
	st, ok := s.scopedStore(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		s.writeError(w, 405, "method not allowed")
		return
	}
	wf, err := st.GetWorkflowByID(r.Context(), id)
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	if wf == nil {
		s.writeError(w, 404, "workflow not found")
		return
	}
	if wf.Status != "dead_lettered" {
		s.writeError(w, 400, "workflow is not dead-lettered, status="+wf.Status)
		return
	}
	if err := st.RetryWorkflow(r.Context(), id); err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	s.writeJSON(w, 200, map[string]string{"id": id, "status": "retried"})
}
