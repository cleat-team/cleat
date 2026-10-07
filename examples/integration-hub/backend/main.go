// Command backend is the API and web layer for the integration-hub scenario.
//
// It is a thin front door onto the worker: it starts syncs, lists them, reads
// their query state, delivers the inbound event the customer's system would
// send, and shows the connector's delivery log. It holds NO business logic,
// deliberately — the point of the scenario is that the integration IS the
// workflow, and a backend that duplicated any of the dispatch would be
// demonstrating the thing cleat exists to remove.
//
// It uses cleat/backendkit rather than hand-rolling HTTP against the worker.
// That module exists for exactly this layer; the CLI's own scaffold
// (cmd/cleat/templates/fullstack/proxy) is stdlib-by-design because it is a
// same-origin allowlist for a generated app, which is a different job.
//
// ONE READ DOES NOT GO THROUGH backendkit, and the reason is worth a reader's
// attention rather than a workaround: the delivery log and the connector list
// are a PLUGIN'S OWN HTTP ROUTES (plugins/notifications/routes.go), mounted on
// the worker's mux beside /api/*. backendkit's Client covers the /api/*
// resources and the plugin HOST-FUNCTION path (CallPlugin -> /api/plugins/…),
// and has no method for a plugin route. `pluginGET` below spells those two paths
// and reuses the client backendkit was configured with, so the API key, the
// timeouts and the tenant are still backendkit's — but the URL is not, and that
// is a gap in the library rather than a decision of this example's. Filed as
// cleat#2550, which carries the routes and the suggested shape.
//
// Run:
//
//	CLEAT_URL=http://localhost:8080 CLEAT_API_KEY_FILE=./.cleat-api-key \
//	CLEAT_SOURCE_ID=<id> go run ./examples/integration-hub/backend
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cleat-team/cleat/cleat/backendkit"
)

// workflowName is the cleat.yaml `name:`. Starting a run names this, and the
// entry point within it.
const (
	workflowName = "integration-hub"
	entryPoint   = "SyncCustomer"
)

// defaultEventType is what the form submits empty. It is the type the compose's
// connector subscribes to, so the default path is the one that delivers.
const defaultEventType = "contact.updated"

// ingestClient carries no credentials, on purpose -- see deliverInbound. It has
// a timeout because http.DefaultClient has none, and this process answers a
// browser.
var ingestClient = &http.Client{Timeout: 30 * time.Second}

type server struct {
	client *backendkit.Client
	log    *slog.Logger

	// sourceID is the webhook_sources row the CUSTOMER's system delivers into.
	// Created once per deployment — the README's setup step — and not something
	// this process may invent, because the ingest route resolves the tenant from
	// that row rather than from the request (cleat#1538).
	sourceID string

	// ingestSecret signs the inbound event. It is the secret the source was
	// registered with, and it lives here because the signature is computed with
	// it: a browser cannot hold this and must not.
	ingestSecret string

	// webhookID names the connector. Optional: when it is empty the first
	// webhook on the tenant is used, so a reader who does not want to copy an id
	// out of a curl response still gets a working page.
	webhookID string
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

	s := &server{
		client:       client,
		log:          logger,
		sourceID:     os.Getenv("CLEAT_SOURCE_ID"),
		ingestSecret: getenv("CLEAT_INGEST_SECRET", "whsec_local_dev"),
		webhookID:    os.Getenv("CLEAT_WEBHOOK_ID"),
	}

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

	mux.HandleFunc("POST /api/syncs", s.startSync)
	mux.HandleFunc("GET /api/syncs", s.listSyncs)
	mux.HandleFunc("GET /api/syncs/{id}", s.getSync)
	mux.HandleFunc("POST /api/inbound", s.deliverInbound)
	mux.HandleFunc("GET /api/deliveries", s.deliveries)
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
	CustomerID string          `json:"customer_id"`
	EventType  string          `json:"event_type"`
	Payload    json.RawMessage `json:"payload"`
	WebhookID  string          `json:"webhook_id"`
}

// startSync starts a run, and is the site where idempotency is the caller's to
// send and this server's to forward.
//
// The key comes from the client when it has one — the browser sends a fresh
// UUID per button press, so a double-click or a retried request under the same
// key returns the ORIGINAL run rather than dispatching the customer's event
// twice. That is reported back as `idempotent_replay`, present on both outcomes
// so a caller can read it unconditionally rather than inferring "original" from
// its absence.
func (s *server) startSync(w http.ResponseWriter, r *http.Request) {
	var req startRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		backendkit.WriteValidationError(w, "body is not valid JSON")
		return
	}
	if strings.TrimSpace(req.CustomerID) == "" {
		backendkit.WriteValidationError(w, "customer_id is required")
		return
	}
	if strings.TrimSpace(req.EventType) == "" {
		req.EventType = defaultEventType
	}
	if len(req.Payload) == 0 {
		req.Payload = json.RawMessage(`{}`)
	}

	webhookID := req.WebhookID
	if webhookID == "" {
		webhookID = s.webhookID
	}
	if webhookID == "" {
		var err error
		webhookID, err = s.firstWebhookID(r.Context())
		if err != nil {
			backendkit.WriteUpstreamError(w, err)
			return
		}
		if webhookID == "" {
			backendkit.WriteError(w, http.StatusConflict,
				"no connector is registered on this tenant: POST /webhooks first, or set CLEAT_WEBHOOK_ID")
			return
		}
	}
	if s.sourceID == "" {
		backendkit.WriteError(w, http.StatusConflict,
			"CLEAT_SOURCE_ID is not set: the ingest source is deployment setup, not something this process may invent (the tenant comes from that row)")
		return
	}

	input, err := json.Marshal(map[string]any{
		"source_id":   s.sourceID,
		"webhook_id":  webhookID,
		"customer_id": req.CustomerID,
		"event_type":  req.EventType,
		"payload":     req.Payload,
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
			"this Idempotency-Key was used for a different sync")
		return
	}
	if err != nil {
		s.log.Error("start failed", "customer", req.CustomerID, "err", err)
		backendkit.WriteUpstreamError(w, err)
		return
	}

	backendkit.WriteJSON(w, http.StatusAccepted, map[string]any{
		"id":                res.ID,
		"idempotent_replay": res.IdempotentReplay,
		"status":            res.Status,
	})
}

// deliverInbound stands in for the CUSTOMER'S SYSTEM.
//
// This is the rope end arriving rather than leaving, and it is here so the page
// is usable without a second terminal: the run parks until an event arrives for
// its source, so a UI that could only start a sync would show every run waiting
// forever.
//
// IT SIGNS SERVER-SIDE, and that is why it is in the backend at all. The ingest
// route is HMAC-verified (X-Hub-Signature-256) and auth-exempt; the secret is
// the source's, it lives on this side of the browser boundary, and a page that
// could compute the signature is a page that has been handed the secret.
//
// The event type goes in a HEADER, not the body: `webhook-ingest` reads
// X-Github-Event then X-Event-Type and falls back to the literal "webhook"
// (plugins/webhookingest/routes.go). A body field called event_type is carried
// through as payload and does not affect routing.
func (s *server) deliverInbound(w http.ResponseWriter, r *http.Request) {
	if s.sourceID == "" {
		backendkit.WriteError(w, http.StatusConflict, "CLEAT_SOURCE_ID is not set")
		return
	}
	var req struct {
		EventType string          `json:"event_type"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		backendkit.WriteValidationError(w, "body is not valid JSON")
		return
	}
	if strings.TrimSpace(req.EventType) == "" {
		req.EventType = defaultEventType
	}
	if len(req.Payload) == 0 {
		req.Payload = json.RawMessage(`{}`)
	}
	body, err := json.Marshal(map[string]any{
		"event_type": req.EventType,
		"payload":    req.Payload,
	})
	if err != nil {
		backendkit.WriteInternalError(w)
		return
	}

	target := s.client.BaseURL + "/ingest/" + url.PathEscape(s.sourceID)
	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		backendkit.WriteInternalError(w)
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Event-Type", req.EventType)
	httpReq.Header.Set("X-Hub-Signature-256", sign(body, s.ingestSecret))

	// DELIBERATELY NOT s.client.HTTPClient. That client carries the worker API
	// key on its transport, and the customer's system does not have one -- the
	// ingest route is AUTH-EXEMPT and verified by HMAC instead. Sending the key
	// anyway would work and would demonstrate the wrong thing: it would make the
	// scenario pass for a caller that could not exist, and it would hide a
	// regression that made the route require auth. This is the one call in the
	// file that is not a cleat API client's, because it is not a cleat API
	// client's call.
	resp, err := ingestClient.Do(httpReq)
	if err != nil {
		backendkit.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		backendkit.WriteError(w, http.StatusBadGateway,
			fmt.Sprintf("the ingest route refused the event: %s %s", resp.Status, strings.TrimSpace(string(msg))))
		return
	}

	backendkit.WriteJSON(w, http.StatusAccepted, map[string]any{
		"delivered":  true,
		"event_type": req.EventType,
		"source_id":  s.sourceID,
	})
}

// getSync returns the run plus its query state, in one round trip.
//
// The state is the interesting half and it is why the workflow publishes it: the
// stage of a live sync, and for a failed one WHICH step failed, come back
// without reading a single event-history row.
func (s *server) getSync(w http.ResponseWriter, r *http.Request) {
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

func (s *server) listSyncs(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	runs, err := s.client.ListWorkflows(r.Context(), status, 50)
	if err != nil {
		backendkit.WriteUpstreamError(w, err)
		return
	}
	backendkit.WriteJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

// deliveries is the connector's delivery log, read from the plugin's own route.
//
// PER-TENANT FILTERING IS ENFORCED BY THE WORKER, NOT BY THIS HANDLER, and that
// is the honest shape of it: `handleListDeliveries` takes the tenant from the
// request context — which the worker's auth middleware set from this process's
// API KEY — and verifies the webhook belongs to it before reading a row
// (plugins/notifications/routes.go). There is no tenant parameter on that route
// to pass through, and inventing one here would be a dropdown that filters
// nothing.
//
// So a `tenant` parameter EXISTS and is an assertion rather than a filter: it
// must name the tenant this backend is configured as, and a request naming
// another one is refused with the reason. A second tenant is a second key.
func (s *server) deliveries(w http.ResponseWriter, r *http.Request) {
	if want := r.URL.Query().Get("tenant"); want != "" && want != s.client.TenantID {
		backendkit.WriteError(w, http.StatusForbidden, fmt.Sprintf(
			"this backend holds one tenant's credentials (%q); %q would need its own key",
			s.client.TenantID, want))
		return
	}

	webhookID := r.URL.Query().Get("webhook_id")
	if webhookID == "" {
		webhookID = s.webhookID
	}
	if webhookID == "" {
		var err error
		webhookID, err = s.firstWebhookID(r.Context())
		if err != nil {
			backendkit.WriteUpstreamError(w, err)
			return
		}
	}
	if webhookID == "" {
		backendkit.WriteJSON(w, http.StatusOK, map[string]any{
			"tenant":     s.client.TenantID,
			"webhook_id": "",
			"deliveries": []any{},
		})
		return
	}

	path := "/webhooks/" + url.PathEscape(webhookID) + "/deliveries"
	if status := r.URL.Query().Get("status"); status != "" {
		path += "?status=" + url.QueryEscape(status)
	}
	var raw []json.RawMessage
	if err := s.pluginGET(r.Context(), path, &raw); err != nil {
		backendkit.WriteUpstreamError(w, err)
		return
	}
	if raw == nil {
		raw = []json.RawMessage{}
	}
	backendkit.WriteJSON(w, http.StatusOK, map[string]any{
		"tenant":     s.client.TenantID,
		"webhook_id": webhookID,
		"deliveries": raw,
	})
}

// firstWebhookID resolves the connector when none was configured.
//
// `GET /webhooks` is a plugin route, so it goes through pluginGET with the
// delivery log for the same reason — see this file's header.
func (s *server) firstWebhookID(ctx context.Context) (string, error) {
	var hooks []struct {
		ID string `json:"id"`
	}
	if err := s.pluginGET(ctx, "/webhooks", &hooks); err != nil {
		return "", err
	}
	if len(hooks) == 0 {
		return "", nil
	}
	return hooks[0].ID, nil
}

// config hands the front end the values it legitimately needs and nothing else.
// It is deliberately not a general settings endpoint: anything that reached the
// browser would be public.
func (s *server) config(w http.ResponseWriter, r *http.Request) {
	backendkit.WriteJSON(w, http.StatusOK, map[string]any{
		"workflow":      workflowName,
		"entry_point":   entryPoint,
		"tenant":        s.client.TenantID,
		"source_id":     s.sourceID,
		"default_event": defaultEventType,
	})
}

// ---- helpers ----

// pluginGET reads a worker route that is not part of the workflow API -- a
// plugin's own, registered in RegisterRoutes.
//
// IT USED TO SPELL THE REQUEST ITSELF, and the doc comment said so at length:
// Client covered /api/* and the plugin HOST-FUNCTION path (CallPlugin posts to
// /api/plugins/{plugin}/{function}) and had no method for the routes a plugin
// mounts, so this example built the URL by hand and reached into
// Client.HTTPClient to keep the auth transport. That was filed as cleat#2550,
// and `Client.PluginRoute` is the method it asked for.
//
// Kept as a named helper rather than inlined at its two call sites, because a
// reader of `deliveries` and `firstWebhookID` is better served by a name that
// says what is being read than by a generic `PluginRoute` at each.
func (s *server) pluginGET(ctx context.Context, path string, out any) error {
	return s.client.PluginRoute(ctx, http.MethodGet, path, nil, out)
}

// sign is the ingest route's HMAC. It is sha256 over the RAW BODY, hex, in the
// `sha256=<hex>` form GitHub uses and plugins/webhookingest verifies.
func sign(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
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

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
