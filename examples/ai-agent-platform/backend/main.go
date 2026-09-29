// Command backend is the API and web layer for the ai-agent-platform scenario.
//
// It is a thin front door onto the worker: it starts agent runs, lists them with
// their query state merged in, reads one run's state and final result, and
// publishes the human approval a parked run is waiting on. It holds NO business
// logic, deliberately — the agent loop, the per-run spend ceiling and the
// approval wait are all in the workflow (examples/ai-agent-platform/agent.go),
// and a backend that re-implemented any of them would be demonstrating the thing
// cleat exists to remove.
//
// It uses cleat/backendkit rather than hand-rolling HTTP against the worker.
// That module exists for exactly this layer; the CLI's own scaffold
// (cmd/cleat/templates/fullstack/proxy) is stdlib-by-design because it is a
// same-origin allowlist for a generated app, which is a different job.
//
// THE APPROVAL IS AN EVENT, NOT A SIGNAL, and that is the workflow's choice
// rather than this file's: agent.go waits with `event-triggers`' `await_event`
// host function, so the only thing that can wake it is a published event.
// plugins/eventtriggers/routes.go's POST /api/events/publish is a PLUGIN ROUTE
// mounted beside /api/*, and backendkit reaches those with `Client.PluginRoute`.
// The event id is CLIENT-generated and required — the route answers 400 "id is
// required" without one and 400 "invalid event id" unless `uuid.Parse` accepts
// it — so newUUID below makes one. See approveAgent for why the route's
// (tenant, event_type) matching means this endpoint has to read the run before
// it publishes.
//
// ONE READ THIS SCENARIO NEEDS HAS NO PATH HERE AT ALL, and it is a gap rather
// than a decision of this example's: what the agent wrote to `blobstore` cannot
// be read back through backendkit. `GET /blobs/{key...}`
// (plugins/blobstore/routes.go) answers with the artifact's RAW BYTES, while
// `PluginRoute` JSON-decodes a non-nil `out` — so the only ways to read one are
// to hand-roll the request and keep the client's auth transport (the shape
// cleat#2550 was filed about for a different route) or to add a raw-response
// method to the library. Neither belongs in a scenario whose artifact is also
// its return value: `AgentOutput.ArtifactKey` names where the bytes went and
// the page shows that, and the README's curl shows the read. Reported rather
// than worked around.
//
// Run, from THIS DIRECTORY (examples/ai-agent-platform) rather than from the
// repository root:
//
//	CLEAT_URL=http://localhost:8080 CLEAT_API_KEY_FILE=./.cleat-api-key \
//	  go run ./backend
//
// The directory is load-bearing, and that is why it is spelled out rather than
// left to the reader. `-web` defaults to the RELATIVE path `web`, resolved with
// filepath.Abs against the process's working directory: from the repository root
// it finds `web/`, which exists and belongs to the Svelte admin dashboard, so
// `GET /` would serve that page instead of this one -- and `GET /app.js` would
// 404, since the dashboard has no such file. That is why main() refuses to start
// unless the resolved directory holds every file it serves, and why the check is
// on all three rather than on index.html alone: the repository root HAS an
// index.html, so that one name clears the wrong directory and starts a process
// serving the dashboard's page — the failure the check exists for. app.js and
// app.css exist only in this example's directory, which is what makes the wrong
// working directory detectable instead of merely unlikely.
// examples/integration-hub/backend/main.go gives the same command
// repo-root-relative and has the same exposure, with no such check; its README
// says to cd first, which is the correct instruction.
//
// The backend is deliberately NOT a service in this example's compose file,
// exactly as in examples/integration-hub: the file stands up what an application
// connects TO, and this is the application.
package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cleat-team/cleat/cleat/backendkit"
)

// workflowName is the cleat.yaml `name:`. Starting a run names this, and the
// entry point within it — which is `run_agent`, the snake_case form of RunAgent,
// because a WASM export is addressed by the name in cleat.yaml and not by the Go
// function it was compiled from.
const (
	workflowName = "ai-agent-platform"
	entryPoint   = "run_agent"
)

// approvalEventType is the event_type agent.go's approval wait subscribes to
// (`h.PluginCall("event-triggers", "await_event", ...)` in requestApproval).
// Nothing else wakes it, and a publish under any other type is delivered to
// whoever else is listening and to this run never.
const approvalEventType = "agent.approval"

// The workflow's own defaults for the two numeric bounds, repeated here so the
// form and /api/config can show an operator what an empty field becomes.
// agent.go applies the same defaults to a zero (`if in.MaxSteps <= 0 { ... }`,
// `if in.BudgetUSD <= 0 { ... }`), so both ends answer identically and a caller
// that posts 0 or posts 8 gets the same run.
//
// Duplicated rather than imported: the guest package is a WASM module
// (agent.wasm) and this process only proxies for it, so linking
// `aiagentplatform` into the server would put the workflow in the wrong binary.
const (
	defaultMaxSteps  = 8
	defaultBudgetUSD = 0.05
)

// approvalWindowSeconds is how long a run waits for a human before giving up:
// agent.go's MaxApprovalPolls (15) times ApprovalPollMs (2000ms), which is 30.
// Duplicated for the same reason as the two defaults above, and it appears in
// exactly one string — the message an operator gets when they press Approve
// after the wait has expired.
const approvalWindowSeconds = 30

// listLimit bounds the run list, and therefore the fan-out in listAgents: each
// listed run costs two more upstream requests, so this is the ceiling on
// 1 + 2*listLimit requests per page poll.
const listLimit = 25

type server struct {
	client *backendkit.Client
	log    *slog.Logger
}

func main() {
	// 9090 is what this example's README tells a reader to open, and it is the
	// same default integration-hub uses. Changed together with that line or not
	// at all: a page the README points at and a port the binary does not bind is
	// the kind of breakage that reads as "the app is broken".
	addr := flag.String("listen", ":9090", "address to listen on")
	webDir := flag.String("web", "web", "directory holding index.html and app.js")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg := backendkit.Load()
	client := backendkit.New(cfg.CleatURL)
	client.TenantID = os.Getenv("CLEAT_TENANT_ID")
	if key := apiKey(); key != "" {
		// The worker's API is authenticated with a key that must never reach a
		// browser. It is read here, server-side, and attached by the client; the
		// front end never sees it and never sends an Authorization header.
		client.HTTPClient = &http.Client{Transport: &authTransport{key: key}}
	}

	s := &server{client: client, log: logger}

	mux := http.NewServeMux()
	// webRoot is resolved ONCE, here, and every path this process opens is
	// filepath.Join(webRoot, <a literal chosen below>). No value that arrives in
	// a request reaches the filesystem, which is the property that matters for
	// a static-file handler; see serveFile for why it is spelled this way.
	webRoot, err := filepath.Abs(*webDir)
	if err != nil {
		logger.Error("resolve web directory", "dir", *webDir, "err", err)
		os.Exit(1)
	}
	// Checked at startup because of what happens without the check: -web is a
	// relative path, so the WRONG working directory finds some other index.html
	// (the repository root's `web/` is the Svelte dashboard's) and the process
	// serves it happily while /app.js 404s. Nothing in that run says the
	// directory was the problem, and the reader is left debugging the page.
	// Exiting names the one thing that is certainly wrong before a port is bound.
	//
	// EVERY file served by a route below, not just index.html. Checking the one
	// name misses the case that motivated the check: `web/index.html` exists at
	// the repository root, so an index.html-only guard starts there and serves
	// the dashboard -- measured, before this loop replaced it. The other two
	// names exist only in this example's directory, so requiring them is what
	// turns "the wrong directory is plausible" into "the wrong directory is
	// refused".
	for _, name := range []string{"index.html", "app.js", "app.css"} {
		if _, err := os.Stat(filepath.Join(webRoot, name)); err != nil {
			logger.Error("the web directory is missing a file this process serves",
				"web", webRoot, "missing", name,
				"hint", "-web is relative to the working directory", "err", err)
			os.Exit(1)
		}
	}

	mux.HandleFunc("GET /", s.serveFile(webRoot, "index.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /app.js", s.serveFile(webRoot, "app.js", "text/javascript; charset=utf-8"))
	// A separate stylesheet rather than a <style> block, because the page is
	// served under `style-src 'self'` and an inline block would need
	// 'unsafe-inline' to render at all. Serving it is cheaper than widening the
	// policy for one page's convenience.
	mux.HandleFunc("GET /app.css", s.serveFile(webRoot, "app.css", "text/css; charset=utf-8"))

	mux.HandleFunc("POST /api/agents", s.startAgent)
	mux.HandleFunc("GET /api/agents", s.listAgents)
	mux.HandleFunc("GET /api/agents/{id}", s.getAgent)
	mux.HandleFunc("POST /api/agents/{id}/approve", s.approveAgent)
	mux.HandleFunc("GET /api/config", s.config)

	// An http.Server with timeouts rather than http.ListenAndServe, which has
	// none: a listener with no ReadHeaderTimeout holds a connection open for as
	// long as a client cares to dribble headers at it, and this process is
	// reachable from a browser. (gosec G114 flags the bare form for exactly
	// this.)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           backendkit.LoggingMiddleware(logger)(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	logger.Info("listening", "addr", *addr, "upstream", cfg.CleatURL, "workflow", workflowName)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("listen failed", "err", err)
		os.Exit(1)
	}
}

// ---- handlers ----

type startRequest struct {
	TenantID    string  `json:"tenant_id"`
	Task        string  `json:"task"`
	MaxSteps    int     `json:"max_steps"`
	BudgetUSD   float64 `json:"budget_usd"`
	ArtifactKey string  `json:"artifact_key"`
}

// startAgent starts one run, and is the site where idempotency is the caller's
// to send and this server's to forward.
//
// The key comes from the client when it has one — the browser sends a fresh
// UUID per button press, so a double-click or a retried request under the same
// key returns the ORIGINAL run rather than paying for a second one. That is
// reported back as `idempotent_replay`, present on both outcomes so a caller can
// read it unconditionally rather than inferring "original" from its absence.
// The cost of getting this wrong is not a duplicate row, it is a duplicate
// invoice, which is why the same mechanism integration-hub uses for a duplicate
// delivery is used here for a duplicate paid run.
func (s *server) startAgent(w http.ResponseWriter, r *http.Request) {
	var req startRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		backendkit.WriteValidationError(w, "body is not valid JSON")
		return
	}
	if strings.TrimSpace(req.Task) == "" {
		backendkit.WriteValidationError(w, "task is required")
		return
	}
	maxSteps := req.MaxSteps
	if maxSteps <= 0 {
		maxSteps = defaultMaxSteps
	}
	budget := req.BudgetUSD
	if budget <= 0 {
		budget = defaultBudgetUSD
	}

	// TWO TENANTS ARE IN PLAY HERE AND THEY ARE NOT THE SAME THING.
	//
	//   - the CLEAT tenant is the one this process's API KEY is scoped to. It
	//     namespaces rows in the worker, and backendkit puts it on the request
	//     ENVELOPE (StartWorkflowWithOptions sends Client.TenantID) rather than
	//     in the input. An envelope tenant_id that disagrees with the key is
	//     refused 403, "tenant_id does not match the authenticated tenant"
	//     (cmd/cleat-worker/server.go), so this handler never sets one.
	//
	//   - the input's tenant_id is AgentInput.TenantID: the CUSTOMER whose spend
	//     this run is bounded by. It is payload, and the worker does not check it
	//     against the key — the premise of the scenario is that one deployment
	//     serves many customers and each run carries its own ceiling (see
	//     AgentInput's doc comment in agent.go).
	//
	// They coincide in the single-customer deployment this example's compose
	// file stands up, which is why defaulting one to the other is right HERE and
	// wrong as a general rule: a backend serving several customers must take the
	// input tenant from the request and never from its own key, or every
	// customer's spend is reported as the first one's.
	tenant := strings.TrimSpace(req.TenantID)
	if tenant == "" {
		tenant = s.client.TenantID
	}
	if tenant == "" {
		// Refused here rather than accepted and failed a moment later inside the
		// workflow. AgentInput.TenantID is required -- agent.go returns
		// "tenant_id is required; the spend ceiling is per tenant and this run
		// cannot be attributed without one" -- so an empty one produces a run that
		// starts, fails on its first step, and reports a message about a field the
		// caller never saw. Both ways to arrive here are named in the error,
		// because they have different fixes: the request sent none, or this
		// process has no CLEAT_TENANT_ID to fall back on.
		backendkit.WriteValidationError(w,
			"tenant_id is required: send one, or set CLEAT_TENANT_ID on this backend")
		return
	}

	input, err := json.Marshal(map[string]any{
		"tenant_id":  tenant,
		"task":       req.Task,
		"max_steps":  maxSteps,
		"budget_usd": budget,
		// Empty is a valid value and not a missing one: agent.go writes the
		// artifact only when the key is non-empty, so a caller that wants a run
		// with no side effect can say so by leaving it out.
		"artifact_key": req.ArtifactKey,
	})
	if err != nil {
		backendkit.WriteInternalError(w)
		return
	}

	opts := backendkit.StartOptions{
		EntryPoint:     entryPoint,
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
	}
	res, err := s.client.StartWorkflowWithOptions(r.Context(), workflowName, input, opts)
	if errors.Is(err, backendkit.ErrIdempotencyKeyInputMismatch) {
		// Refused rather than answered with the first result: a key reused with
		// a different payload means the caller did something ELSE, and replaying
		// would tell them their new task was already running.
		backendkit.WriteError(w, http.StatusConflict,
			"this Idempotency-Key was used for a different run")
		return
	}
	if err != nil {
		s.log.Error("start failed", "task", req.Task, "err", err)
		backendkit.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}

	backendkit.WriteJSON(w, http.StatusAccepted, map[string]any{
		"id":                res.ID,
		"idempotent_replay": res.IdempotentReplay,
		"status":            res.Status,
	})
}

// agentRun is one row of the list, and it is deliberately flat: the page reads
// the run's own fields for the columns it paints from the engine (status) and
// the query state for everything the workflow publishes.
type agentRun struct {
	ID        string            `json:"id"`
	Status    string            `json:"status"`
	Task      string            `json:"task"`
	CreatedAt time.Time         `json:"created_at"`
	State     map[string]string `json:"state"`
}

// listAgents merges each run with its query state, which is where everything the
// table actually shows comes from: `steps`, `spent_usd` and `budget_usd` are all
// published by agent.go through SetQueryState and none of them is a column on
// the run. That is the same division integration-hub's listSyncs and getSync
// make, and it is the reason a UI can watch a run that is asleep for hours
// without reading a single event-history row.
//
// ListWorkflows CANNOT FILTER BY WORKFLOW, and that is the one place in this
// file that costs anything. The worker's list route takes `?def_name=`
// (WorkflowFilter.DefName, cmd/cleat-worker/server.go), but backendkit's typed
// method takes only (status, limit) and its WorkflowSummary carries no DefName
// field — so a tenant running a second workflow would see its runs in this
// table. The typed way to learn a run's definition is GetWorkflow, which does
// report DefName, and that is one request per run.
//
// The summary already carries id, status, input and created_at — everything this
// handler returns except the state — so that round trip buys exactly one thing:
// the def_name check. It makes this 1 + 2N requests per poll rather than 1 + N,
// bounded by listLimit. `PluginRoute` against "/api/workflows?def_name=..."
// would recover both the filter and the request, and would also be reading an
// /api/* resource without the typed method that exists for it, which is the
// worse of the two trades. Reported rather than worked around; see this file's
// header.
func (s *server) listAgents(w http.ResponseWriter, r *http.Request) {
	runs, err := s.client.ListWorkflows(r.Context(), r.URL.Query().Get("status"), listLimit)
	if err != nil {
		backendkit.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}

	out := make([]agentRun, 0, len(runs))
	for _, run := range runs {
		detail, err := s.client.GetWorkflow(r.Context(), run.ID)
		if err != nil {
			// One unreadable run does not fail the list: it is dropped with a
			// line in the log rather than taking the page down with it.
			s.log.Warn("skipping a run whose detail would not read", "id", run.ID, "err", err)
			continue
		}
		if detail.DefName != workflowName {
			continue
		}
		state, err := s.client.GetWorkflowState(r.Context(), run.ID)
		if err != nil {
			// Not fatal, and not a reason to drop the row: a run that has not
			// published anything yet has no state, and its task and status are
			// still worth listing.
			s.log.Warn("no query state", "id", run.ID, "err", err)
			state = map[string]string{}
		}
		out = append(out, agentRun{
			ID:        run.ID,
			Status:    run.Status,
			Task:      runTask(run.Input),
			CreatedAt: run.CreatedAt,
			State:     state,
		})
	}

	backendkit.WriteJSON(w, http.StatusOK, map[string]any{"runs": out})
}

// getAgent returns the run plus its query state, in one round trip.
//
// The state is the interesting half and it is why the workflow publishes it: the
// current tool, how many steps have been paid for, what has been spent of what
// ceiling, and for a failed run WHICH step failed, all come back without reading
// a single event-history row.
func (s *server) getAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	detail, err := s.client.GetWorkflow(r.Context(), id)
	if err != nil {
		backendkit.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}
	if detail.DefName != workflowName {
		// A run on this tenant that is not this workflow. 404 rather than 502:
		// the request names a resource this backend does not serve, which is
		// what the page would be handed for an id it never listed.
		backendkit.WriteNotFound(w)
		return
	}
	state, err := s.client.GetWorkflowState(r.Context(), id)
	if err != nil {
		// Not fatal: a run that has not published anything yet has no state, and
		// the detail is still worth returning.
		s.log.Warn("no query state", "id", id, "err", err)
		state = map[string]string{}
	}
	backendkit.WriteJSON(w, http.StatusOK, map[string]any{
		"id":     detail.ID,
		"status": detail.Status,
		"task":   runTask(detail.Input),
		// Result is the workflow's return value, which for this workflow is a
		// JSON DOCUMENT ENCODED AS A STRING: a WASM entry point hands back bytes
		// and RunAgent returns `string` (agent.go). It goes out exactly as the
		// engine stored it and the page parses it. Re-encoding it here would
		// require this backend to know AgentOutput's shape, and the point of the
		// proxy is that it does not — the page's rendering of a result and the
		// workflow's struct can then be changed together or not at all.
		"result":  detail.Result,
		"error":   detail.Error,
		"state":   state,
		"created": detail.CreatedAt,
		"updated": detail.UpdatedAt,
	})
}

// approveAgent is THE HUMAN DECISION, and it is the only handler here that
// writes to the worker rather than reading from it.
//
// THE GUARD BELOW IS THE WHOLE REASON THE STATE IS READ, and it is not
// politeness. An event is matched by (tenant_id, event_type) and by nothing
// else: `signalAwaiters` in plugins/eventtriggers selects awaiting runs by those
// two columns, and agent.go's `await_event` call passes no run filter because the
// host function has none. So this endpoint cannot say "approve run X" — it can
// only publish an event that wakes whichever run on THIS TENANT next polls for
// `agent.approval`. Refusing to publish unless the named run is the one waiting
// is the only run-scoping available at this layer, and it NARROWS the race
// rather than closing it: a second run that starts waiting between this read and
// the publish takes the decision instead. The `run_id` carried in the event data
// is an audit trail, not an address.
//
// Note which vocabulary is checked. `detail.Status` is the ENGINE's —
// running/failed/done — and a run parked on a human is `running` for its entire
// wait, so it says nothing about whether anyone is waiting. What says "waiting"
// is the workflow's own published status, which is why this reads the state.
func (s *server) approveAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req struct {
		Approved bool   `json:"approved"`
		Note     string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		backendkit.WriteValidationError(w, "body is not valid JSON")
		return
	}

	detail, err := s.client.GetWorkflow(r.Context(), id)
	if err != nil {
		backendkit.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}
	if detail.DefName != workflowName {
		backendkit.WriteNotFound(w)
		return
	}

	state, err := s.client.GetWorkflowState(r.Context(), id)
	if err != nil {
		// Fatal HERE, unlike in the two read handlers: the guard is the point,
		// and a run whose state will not read cannot be confirmed to be waiting.
		s.log.Error("no query state for an approval", "id", id, "err", err)
		backendkit.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}
	submitted := req.Approved
	if state["status"] != "awaiting_approval" {
		// 409 rather than 400: the request is well formed and would have been
		// accepted a few seconds earlier or later. The window is short — 15
		// polls two seconds apart — so an operator who was reading the reason
		// before deciding is the likeliest caller to be told this.
		backendkit.WriteError(w, http.StatusConflict, fmt.Sprintf(
			"run %s reads %q, not %q: nothing is waiting for this decision (the wait lasts about %d seconds)",
			id, state["status"], "awaiting_approval", approvalWindowSeconds))
		return
	}

	eventID, err := newUUID()
	if err != nil {
		s.log.Error("could not generate an event id", "err", err)
		backendkit.WriteInternalError(w)
		return
	}

	body := map[string]any{
		// The route requires a client-generated id: plugins/eventtriggers/
		// routes.go answers 400 "id is required" when it is empty and 400
		// "invalid event id" when uuid.Parse refuses it. It never substitutes
		// one of its own, because the id the publisher chose is the publish's
		// idempotency.
		"id":         eventID,
		"event_type": approvalEventType,
		"data": map[string]any{
			"approved": submitted,
			"note":     req.Note,
			// Read by nobody today — agent.go decodes {approved, note} and
			// ignores the rest. It is carried because this event IS the audit
			// trail for a decision a person made, and a trail that does not name
			// the run it was about is not one.
			"run_id": id,
		},
	}
	if err := s.client.PluginRoute(r.Context(), http.MethodPost, "/api/events/publish", body, nil); err != nil {
		backendkit.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}

	// 202 rather than 200: the event is published, and the run is woken by its
	// own NEXT poll of the approval wait — up to ApprovalPollMs (2s) later. A
	// response that said the decision had landed would be reporting the
	// publish's success as the run's, which is the difference the page has to
	// wait out.
	backendkit.WriteJSON(w, http.StatusAccepted, map[string]any{
		"published": true,
		"event_id":  eventID,
		"run_id":    id,
		"approved":  submitted,
		"note":      req.Note,
	})
}

// config hands the front end the values it legitimately needs and nothing else.
// It is deliberately not a general settings endpoint: anything that reached the
// browser would be public.
//
// The two defaults are here rather than in app.js so that the numbers the form
// starts with and the numbers a run is actually given have one source. A page
// that hard-coded them would be right until someone changed the workflow's.
func (s *server) config(w http.ResponseWriter, r *http.Request) {
	backendkit.WriteJSON(w, http.StatusOK, map[string]any{
		"workflow":            workflowName,
		"entry_point":         entryPoint,
		"tenant":              s.client.TenantID,
		"approval_event_type": approvalEventType,
		"default_budget_usd":  defaultBudgetUSD,
		"default_max_steps":   defaultMaxSteps,
	})
}

// ---- helpers ----

// runTask pulls the task out of a run's recorded input.
//
// The task is NOT a query-state key. agent.go publishes tenant_id, status,
// budget_usd, steps, spent_usd, total_tokens, current_tool, approval_reason,
// approval_polls, stopped_reason and failed_step — and no `task` — so the only
// place to learn what a run was asked to do is the input the engine stored
// alongside it. Both WorkflowSummary and WorkflowDetail carry that as raw JSON,
// which is why this decodes rather than reading a field off the run.
func runTask(input json.RawMessage) string {
	var in struct {
		Task string `json:"task"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		// A run whose input will not decode is still a run worth listing. An
		// empty task cell is a worse row than a titled one and a much better one
		// than a missing run.
		return ""
	}
	return in.Task
}

// newUUID returns a random RFC 4122 version 4 UUID in the canonical
// 36-character form.
//
// Hand-rolled rather than importing github.com/google/uuid, which this module
// ALREADY has — as an INDIRECT requirement (examples/go.mod, in the second
// require block, carrying the `// indirect` comment). Importing it directly
// moves that line into the direct block and drops the marker, and
// scripts/check-go-mod-tidy.sh runs `go mod tidy -diff` in every module on disk
// with no exemption list and exits non-zero on any diff. So the import would
// fail a check in a file nowhere near this one, for a dependency this file does
// not need. Measured rather than assumed: the two-arm probe in the PR that added
// this file shows the require line moving.
//
// What matters is the FORMAT, not the library — plugins/eventtriggers/routes.go
// rejects the publish with 400 "invalid event id" unless uuid.Parse accepts the
// string — and this is the whole of that format.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xx, RFC 4122
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// serveFile serves one of the three assets, by a name chosen at the call site.
//
// The signature is (root, name, type) rather than (path) deliberately. A handler
// that takes a ready-made path invites the next person to build one from the
// request, and the linter cannot tell the difference either: gosec reads
// `os.ReadFile(<flag-derived value>)` as a taint flow (G703) however the value
// was assembled. Joining a literal to a root resolved at startup is the shape
// where the answer is visible to a reader and to the analyzer.
func (s *server) serveFile(root, name, contentType string) http.HandlerFunc {
	path := filepath.Join(root, name)
	return func(w http.ResponseWriter, r *http.Request) {
		//nolint:gosec // G703: path is root + a literal fixed at startup; root is the operator's own -web flag, and nothing from the request reaches it.
		body, err := os.ReadFile(path)
		if err != nil {
			backendkit.WriteNotFound(w)
			return
		}
		w.Header().Set("Content-Type", contentType)
		// The front end loads one script and nothing else, so the policy can be
		// this narrow. It is what stops an injected string from becoming a
		// script tag.
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:")
		_, _ = w.Write(body)
	}
}

// authTransport attaches the worker API key. It is on the transport rather than
// in each call so that no handler can forget it, and so that the header is
// stripped from anything the browser sent.
type authTransport struct{ key string }

func (t *authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Del("Authorization")
	r.Header.Del("Cookie")
	r.Header.Set("Authorization", "Bearer "+t.key)
	return http.DefaultTransport.RoundTrip(r)
}

// apiKey reads the key from a file first. A file is preferred because an
// environment variable is visible to anything that can read the process
// environment, and a file can be chmod-ed.
func apiKey() string {
	if path := os.Getenv("CLEAT_API_KEY_FILE"); path != "" {
		//nolint:gosec // G703: the path is the operator's own CLEAT_API_KEY_FILE, which sits at the same trust level as the process's other configuration. Nothing a request carries reaches it.
		if b, err := os.ReadFile(path); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return os.Getenv("CLEAT_API_KEY")
}
