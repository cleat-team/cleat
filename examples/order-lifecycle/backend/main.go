// Command backend is the API and web layer for the order-lifecycle scenario.
//
// It is a thin front door onto the worker: it starts workflow runs, lists them,
// reads their query state, and delivers the approval signal. It holds NO
// business logic, deliberately — the point of the scenario is that the order
// pipeline IS the workflow, and a backend that duplicated any of the sequence
// would be demonstrating the thing cleat exists to remove.
//
// It uses cleat/backendkit rather than hand-rolling HTTP against the worker.
// That module exists for exactly this layer; the CLI's own scaffold
// (cmd/cleat/templates/fullstack/proxy) is stdlib-by-design because it is a
// same-origin allowlist for a generated app, which is a different job.
//
// Run:
//
//	CLEAT_URL=http://localhost:8080 CLEAT_API_KEY_FILE=./.cleat-api-key \
//	  go run ./examples/order-lifecycle/backend
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cleat-team/cleat/cleat/backendkit"
	orderlifecycle "github.com/cleat-team/cleat/examples/order-lifecycle"
)

// workflowName is the cleat.yaml `name:`. Starting a run names this, and the
// entry point within it.
const (
	workflowName = "order-lifecycle"
	entryPoint   = "PlaceOrder"
)

type server struct {
	client *backendkit.Client
	log    *slog.Logger
}

func main() {
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

	mux.HandleFunc("GET /", s.serveFile(webRoot, "index.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /app.js", s.serveFile(webRoot, "app.js", "text/javascript; charset=utf-8"))
	// A separate stylesheet rather than a <style> block, because the page is
	// served under `style-src 'self'` and an inline block would need
	// 'unsafe-inline' to render at all. Serving it is cheaper than widening the
	// policy for one page's convenience.
	mux.HandleFunc("GET /app.css", s.serveFile(webRoot, "app.css", "text/css; charset=utf-8"))

	mux.HandleFunc("POST /api/orders", s.startOrder)
	mux.HandleFunc("GET /api/orders", s.listOrders)
	mux.HandleFunc("GET /api/orders/{id}", s.getOrder)
	mux.HandleFunc("POST /api/orders/{id}/approve", s.approveOrder)
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
	OrderID    string `json:"order_id"`
	CustomerID string `json:"customer_id"`
	Email      string `json:"email"`

	// AmountCents is what the order is worth, and it is the field that decides
	// which branch the reader sees: anything above approvalThresholdCents parks
	// on the approval signal, anything below charges straight through. It is
	// caller-supplied rather than fixed because a page that could only submit
	// one value could demonstrate one of the two paths.
	AmountCents int `json:"amount_cents"`

	SimulatePaymentFailure      bool `json:"simulate_payment_failure"`
	SimulateFulfilmentFailure   bool `json:"simulate_fulfilment_failure"`
	SimulateCompensationFailure bool `json:"simulate_compensation_failure"`
}

// defaultAmountCents is the order an empty form submits -- two widgets, the
// same shape the unit tests use. Below the threshold, so the default path is
// the one that completes.
const defaultAmountCents = 5000

// startOrder starts a run, and is the site where idempotency is the caller's to
// send and this server's to forward.
//
// The key comes from the client when it has one — the browser sends a fresh
// UUID per button press, so a double-click or a retried request under the same
// key returns the ORIGINAL run rather than creating a second order. That is
// reported back as `idempotent_replay`, present on both outcomes so a caller can
// read it unconditionally rather than inferring "original" from its absence.
func (s *server) startOrder(w http.ResponseWriter, r *http.Request) {
	var req startRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		backendkit.WriteValidationError(w, "body is not valid JSON")
		return
	}
	if strings.TrimSpace(req.OrderID) == "" {
		backendkit.WriteValidationError(w, "order_id is required")
		return
	}
	amount := req.AmountCents
	if amount == 0 {
		amount = defaultAmountCents
	}
	if amount < 0 {
		backendkit.WriteValidationError(w, "amount_cents cannot be negative")
		return
	}

	input, err := json.Marshal(map[string]any{
		"order_id":                      req.OrderID,
		"customer_id":                   req.CustomerID,
		"email":                         req.Email,
		"items":                         []map[string]any{{"sku": "widget", "quantity": 1, "price_cents": amount}},
		"source_id":                     os.Getenv("CLEAT_WEBHOOK_SOURCE_ID"),
		"simulate_payment_failure":      req.SimulatePaymentFailure,
		"simulate_fulfilment_failure":   req.SimulateFulfilmentFailure,
		"simulate_compensation_failure": req.SimulateCompensationFailure,
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
		// Refused rather than answered with the first result: a key reused with a
		// different payload means the caller did something ELSE, and replaying
		// would tell them their new request succeeded.
		backendkit.WriteError(w, http.StatusConflict,
			"this Idempotency-Key was used for a different order")
		return
	}
	if err != nil {
		s.log.Error("start failed", "order", req.OrderID, "err", err)
		backendkit.WriteUpstreamError(w, err)
		return
	}

	backendkit.WriteJSON(w, http.StatusAccepted, map[string]any{
		"id":                res.ID,
		"idempotent_replay": res.IdempotentReplay,
		"status":            res.Status,
	})
}

// getOrder returns the run plus its query state, in one round trip.
//
// The state is the interesting half and it is why the workflow publishes it: the
// status of a live order, and for a failed one WHICH compensations ran, come
// back without reading a single event-history row.
func (s *server) getOrder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	detail, err := s.client.GetWorkflow(r.Context(), id)
	if err != nil {
		backendkit.WriteUpstreamError(w, err)
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
		"id":      detail.ID,
		"status":  detail.Status,
		"result":  detail.Result,
		"error":   detail.Error,
		"state":   state,
		"created": detail.CreatedAt,
	})
}

func (s *server) listOrders(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	runs, err := s.client.ListWorkflows(r.Context(), status, 50)
	if err != nil {
		backendkit.WriteUpstreamError(w, err)
		return
	}
	backendkit.WriteJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *server) approveOrder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Approve bool   `json:"approve"`
		Reason  string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	name, payload := "order_approved", `{"by":"operator"}`
	if !body.Approve {
		name = "order_rejected"
		reason := body.Reason
		if strings.TrimSpace(reason) == "" {
			reason = "rejected without a reason"
		}
		payload = reason
	}
	if err := s.client.SignalWorkflow(r.Context(), id, name, payload); err != nil {
		backendkit.WriteUpstreamError(w, err)
		return
	}
	backendkit.WriteJSON(w, http.StatusOK, map[string]any{"signal": name})
}

// config hands the front end the two values it legitimately needs and nothing
// else. It is deliberately not a general settings endpoint: anything that
// reached the browser would be public.
func (s *server) config(w http.ResponseWriter, r *http.Request) {
	backendkit.WriteJSON(w, http.StatusOK, map[string]any{
		"approval_threshold_cents": approvalThresholdCents,
		"default_amount_cents":     defaultAmountCents,
		"workflow":                 workflowName,
	})
}

// ---- helpers ----

// approvalThresholdCents is the workflow's own constant, imported rather than
// restated.
//
// The tempting shape here is a second `const 50_000` with a comment saying the
// two must agree, and that is worse than it looks: it makes agreement a thing
// somebody has to remember, and the failure when they forget is a page whose
// approval form silently stops appearing. The package is ordinary Go — `cleat
// build` transforms a copy of it, it does not replace it — so the backend can
// simply reference the constant the workflow actually branches on.
const approvalThresholdCents = orderlifecycle.ApprovalThresholdCents

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
