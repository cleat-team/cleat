// Command backend is the API and web layer for the b2b-saas-control-plane
// scenario.
//
// It is a thin front door onto the worker, plus the one thing the scenario
// needs that no tenant-scoped worker API can do at all: CREATING a tenant.
// cleat has exactly one sanctioned surface for that --
// `cleat-worker --create-tenant`/`--generate-api-key` (PostgreSQL only; see
// the README) -- and this process SHELLS OUT to it rather than importing
// auth.TenantStore directly. Two reasons, not one:
//
//   - auth/ is an internal package of the ROOT module. examples/ is its own,
//     separate Go module (go.work is what lets `go build` resolve it inside
//     THIS repo; a copy of this directory taken outside it could not).
//     Every other example's backend imports only cleat/backendkit, the
//     public SDK, for exactly this reason -- importing auth/ here would be
//     the first example to break that boundary.
//   - CreateTenant is deliberately CLI-only in this codebase (cleat#2534
//     Stage 1's own review: a plugin grant for it would, under
//     --tenant-isolation=role, create tenants no worker could serve -- see
//     that stage's PR for the mechanism). Calling the same binary an
//     operator would run, rather than a second Go implementation of what it
//     does, is what keeps that constraint from having two places to drift
//     apart in.
//
// Everything AFTER signup -- starting the provisioning run, reading its
// published state, reading the tenant's own lifecycle status -- goes through
// the ordinary tenant-scoped worker API, using the credential signup just
// minted. This process holds no other privilege over a tenant's own data --
// literally none, not merely by convention: it holds no credential the
// worker will accept for any of it, so every one of these calls carries the
// caller's own Bearer token through, the same way getTenantLifecycle always
// did (see getProvisioningStatus's doc comment for the bug this was until
// it also did).
//
// Run:
//
//	CLEAT_URL=http://localhost:8080 CLEAT_ADMIN_DB_URL=postgres://... \
//	  CLEAT_ORG_ID=<uuid from --create-org> \
//	  CLEAT_WASM_PATH=<path to the wasm 'cleat build' produced and the operator deployed> \
//	  go run ./examples/b2b-saas-control-plane/backend
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/cleat/backendkit"
)

// workflowName is the cleat.yaml `name:`. entryPoint is the WASM export --
// see cleat.yaml's own comment on why the two are not currently the same
// word as the built artifact's filename.
const (
	workflowName = "b2b-saas-control-plane"
	entryPoint   = "provision_tenant"
)

type server struct {
	// cleatURL is the base URL clientForTenant builds every request against.
	// This process holds no credential of its own that the worker will
	// accept for anything past signup -- see getProvisioningStatus's doc
	// comment for why an earlier, credential-less adminClient here was a
	// live bug rather than a legitimate "run id is the capability" surface.
	cleatURL string

	// admin shells out to cleat-worker's own --create-tenant/
	// --generate-api-key flags -- see the package doc comment for why this
	// is not a direct database connection.
	admin *tenantAdmin

	log *slog.Logger
}

func main() {
	addr := flag.String("listen", ":9091", "address to listen on")
	webDir := flag.String("web", "web", "directory holding index.html and app.js")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg := backendkit.Load()

	admin, err := newTenantAdmin()
	if err != nil {
		logger.Error("configure tenant admin", "err", err)
		os.Exit(1)
	}

	s := &server{
		cleatURL: cfg.CleatURL,
		admin:    admin,
		log:      logger,
	}

	mux := http.NewServeMux()
	webRoot, err := filepath.Abs(*webDir)
	if err != nil {
		logger.Error("resolve web directory", "dir", *webDir, "err", err)
		os.Exit(1)
	}
	mux.HandleFunc("GET /", s.serveFile(webRoot, "index.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /app.js", s.serveFile(webRoot, "app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /app.css", s.serveFile(webRoot, "app.css", "text/css; charset=utf-8"))

	mux.HandleFunc("POST /api/signup", s.signup)
	mux.HandleFunc("GET /api/provisioning/{runID}", s.getProvisioningStatus)
	mux.HandleFunc("GET /api/tenant/lifecycle", s.getTenantLifecycle)
	mux.HandleFunc("GET /api/config", s.config)

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

type signupRequest struct {
	BusinessName string `json:"business_name"`
	AdminEmail   string `json:"admin_email"`
	Plan         string `json:"plan"`

	SimulateWorkspaceFailure bool `json:"simulate_workspace_failure"`
}

// tenantNamePattern is what auth.TenantStore.CreateTenant's `name` argument
// must satisfy underneath --create-tenant -- a stable identifier, not the
// display name a business actually typed. Derived from BusinessName rather
// than asked for separately, so the form has one fewer field; collisions
// are refused by CreateTenant's own unique constraint, surfaced as-is below.
var tenantNamePattern = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(businessName string) string {
	slug := tenantNamePattern.ReplaceAllString(strings.ToLower(businessName), "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = "tenant"
	}
	// A random suffix, not a sequence: two businesses named "Acme" must not
	// collide, and this process holds no counter to disambiguate them with
	// anyway.
	return slug + "-" + uuid.NewString()[:8]
}

// signup is the one thing a tenant-scoped worker API cannot do for itself:
// create the tenant. Everything after this call runs AS the tenant it just
// created, through the ordinary worker API -- see provision.go's "Which
// tenant does this run as" for why.
//
// THE API KEY IS RETURNED EXACTLY ONCE, IN THIS RESPONSE. It is not
// recoverable afterward through this scenario -- cleat has no "show me my
// key again" surface, by design (auth/tenant_store.go stores only a hash).
// A production onboarding flow would deliver it out of band (email, a
// first-login flow); this demo returns it inline because there is no such
// channel to demonstrate.
func (s *server) signup(w http.ResponseWriter, r *http.Request) {
	var req signupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		backendkit.WriteValidationError(w, "body is not valid JSON")
		return
	}
	if strings.TrimSpace(req.BusinessName) == "" {
		backendkit.WriteValidationError(w, "business_name is required")
		return
	}

	ctx := r.Context()
	tenantName := slugify(req.BusinessName)
	tenantID, err := s.admin.createTenant(ctx, tenantName, req.BusinessName)
	if err != nil {
		s.log.Error("create tenant", "business_name", req.BusinessName, "err", err)
		backendkit.WriteInternalError(w)
		return
	}

	// workflow_defs is per-tenant (see deployWorkflowDef's doc comment) --
	// this tenant has no rows in it yet, so StartWorkflowWithOptions below
	// would 404 without this.
	if err := s.admin.deployWorkflowDef(ctx, tenantID); err != nil {
		s.log.Error("deploy workflow definition for tenant", "tenant_id", tenantID, "err", err)
		backendkit.WriteInternalError(w)
		return
	}

	rawKey, err := s.admin.generateAPIKey(ctx, tenantID)
	if err != nil {
		s.log.Error("mint api key", "tenant_id", tenantID, "err", err)
		backendkit.WriteInternalError(w)
		return
	}

	// Started AS THE NEW TENANT: a client built fresh, carrying only this
	// tenant's own key, never the operator credential this process also
	// holds. provisionAsTenant is the one place those two must not be
	// confused.
	tenantClient := s.clientForTenant(rawKey)
	input, err := json.Marshal(map[string]any{
		"tenant_id":                  tenantID.String(),
		"business_name":              req.BusinessName,
		"admin_email":                req.AdminEmail,
		"plan":                       req.Plan,
		"simulate_workspace_failure": req.SimulateWorkspaceFailure,
	})
	if err != nil {
		backendkit.WriteInternalError(w)
		return
	}
	res, err := tenantClient.StartWorkflowWithOptions(ctx, workflowName, input, backendkit.StartOptions{
		EntryPoint: entryPoint,
	})
	if err != nil {
		s.log.Error("start provisioning run", "tenant_id", tenantID, "err", err)
		backendkit.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}

	backendkit.WriteJSON(w, http.StatusCreated, map[string]any{
		"tenant_id": tenantID.String(),
		"api_key":   rawKey,
		"run_id":    res.ID,
	})
}

// getProvisioningStatus reads a provisioning run's published state, USING
// THE CALLER'S OWN TENANT CREDENTIAL -- not s.adminClient, which holds none.
//
// This doc comment used to claim the shape was examples/order-lifecycle's
// getOrder, "the run id is the capability" -- confirmed wrong running this
// scenario end to end: order-lifecycle's adminClient DOES carry a real
// credential (backend/main.go's authTransport, from CLEAT_API_KEY_FILE/
// CLEAT_API_KEY), because that scenario runs everything under one fixed
// operator tenant. "The run id is the capability" is true only WITHIN that
// one tenant's own scope, not across tenants -- and this scenario has no
// single fixed tenant to borrow that shape from: every signup mints a new
// one. Calling s.adminClient here sent no Authorization header at all, so
// cmd/cleat-worker/server.go's scopedStore refused every call with 401
// "authentication required" -- wrapped by backendkit's classifyError and
// then flattened to a bare 502 by this handler's own error branch, so the
// symptom at the caller was "Bad Gateway" with the real cause (401, no
// credential) never visible until s.log.Error was added here to catch it.
// RLS on workflow_instances would refuse a cross-tenant read even with a
// real but WRONG tenant's credential, so the operator's own key would not
// have worked either -- the caller's own key, extracted the same way
// getTenantLifecycle already does, is the only credential that can ever
// succeed here.
func (s *server) getProvisioningStatus(w http.ResponseWriter, r *http.Request) {
	key := bearerToken(r)
	if key == "" {
		backendkit.WriteError(w, http.StatusUnauthorized, "authorization required")
		return
	}
	tenantClient := s.clientForTenant(key)
	runID := r.PathValue("runID")
	detail, err := tenantClient.GetWorkflow(r.Context(), runID)
	if err != nil {
		s.log.Error("get workflow", "run_id", runID, "err", err)
		backendkit.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}
	state, err := tenantClient.GetWorkflowState(r.Context(), runID)
	if err != nil {
		s.log.Warn("no query state", "run_id", runID, "err", err)
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

// getTenantLifecycle proxies plugins/tenantlifecycle's own tenant-scoped
// route (Stage 3, cleat#2683) -- THIS PROCESS NEVER READS admin.tenants
// DIRECTLY for it. The caller's own API key, in the Authorization header,
// is what scopes the read to their tenant: this handler forwards it
// unmodified rather than substituting its own credential, so a caller can
// only ever see their own lifecycle status, never another tenant's --
// exactly the constraint cleat#2169 already enforces at the worker.
func (s *server) getTenantLifecycle(w http.ResponseWriter, r *http.Request) {
	key := bearerToken(r)
	if key == "" {
		backendkit.WriteError(w, http.StatusUnauthorized, "authorization required")
		return
	}
	tenantClient := s.clientForTenant(key)
	var out map[string]any
	if err := tenantClient.PluginRoute(r.Context(), http.MethodGet, "/api/tenant/lifecycle", nil, &out); err != nil {
		backendkit.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}
	backendkit.WriteJSON(w, http.StatusOK, out)
}

func (s *server) config(w http.ResponseWriter, r *http.Request) {
	backendkit.WriteJSON(w, http.StatusOK, map[string]any{
		"workflow": workflowName,
	})
}

// ---- helpers ----

// clientForTenant builds a backendkit.Client carrying exactly one tenant's
// own API key, never this process's own operator credential. Built fresh
// per request rather than cached: this process serves every tenant that
// ever signs up, so there is no single "the" tenant client to hold as a
// field the way order-lifecycle's single-tenant demo does.
func (s *server) clientForTenant(apiKey string) *backendkit.Client {
	c := backendkit.New(s.cleatURL)
	c.HTTPClient = &http.Client{Transport: &authTransport{key: apiKey}}
	return c
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimPrefix(h, prefix)
}

// ---- tenantAdmin: shells out to cleat-worker's own admin flags ----
//
// See the package doc comment for why this is a subprocess rather than a
// direct auth.TenantStore call. PostgreSQL only -- --create-tenant refuses
// on MySQL and SQL Server (auth.CreateTenant's own dialect gate), so this
// scenario does not attempt the other two dialects. See the README.
type tenantAdmin struct {
	workerBin string
	cleatBin  string
	wasmPath  string
	dbURL     string
	orgID     string
}

func newTenantAdmin() (*tenantAdmin, error) {
	dbURL := os.Getenv("CLEAT_ADMIN_DB_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("CLEAT_ADMIN_DB_URL is required -- this process creates tenants via cleat-worker --create-tenant, which needs a privileged connection (the same DSN the migrate step uses), not the lesser cleat_app one the worker itself runs on")
	}
	orgID := os.Getenv("CLEAT_ORG_ID")
	if orgID == "" {
		return nil, fmt.Errorf(`CLEAT_ORG_ID is required -- create one first with: cleat-worker --create-org <name> --db "$CLEAT_ADMIN_DB_URL"`)
	}
	if _, err := uuid.Parse(orgID); err != nil {
		return nil, fmt.Errorf("CLEAT_ORG_ID is not a valid UUID: %q: %w", orgID, err)
	}
	workerBin := os.Getenv("CLEAT_WORKER_BIN")
	if workerBin == "" {
		workerBin = "cleat-worker"
	}
	cleatBin := os.Getenv("CLEAT_BIN")
	if cleatBin == "" {
		cleatBin = "cleat"
	}
	// workflow_defs is keyed (tenant_id, name, version) with RLS forced on it
	// (migrations/postgres/001_schema.sql) -- there is no "visible to every
	// tenant" row. The operator's own initial `cleat deploy` (see the
	// README/scenario script) registers the definition under ITS tenant only,
	// so a freshly signed-up tenant has zero rows for it and every
	// StartWorkflowWithOptions call 404s with "workflow definition not
	// found" until this process deploys the SAME wasm again under the new
	// tenant's id. CLEAT_WASM_PATH is that file; there is no sane default
	// because the build output's name is a build-time decision (cleat#2692),
	// not this process's to guess.
	wasmPath := os.Getenv("CLEAT_WASM_PATH")
	if wasmPath == "" {
		return nil, fmt.Errorf("CLEAT_WASM_PATH is required -- this process re-deploys the provisioning workflow's wasm under each new tenant's own id (workflow_defs is per-tenant; see deployWorkflowDef's doc comment)")
	}
	return &tenantAdmin{workerBin: workerBin, cleatBin: cleatBin, wasmPath: wasmPath, dbURL: dbURL, orgID: orgID}, nil
}

// tenantIDLine and apiKeyLine pull the one field each command's human-
// readable "=== CLEAT ... ===" banner prints that this process actually
// needs. cleat-worker has no --json/machine-readable form of either
// command -- this is the awkward, real integration point, the same honesty
// examples/order-lifecycle/README.md gives its own webhook-source setup
// step for the same reason: the sanctioned surface prints for a human, and
// there is no other one to call instead.
var tenantIDLine = regexp.MustCompile(`(?m)^Tenant ID:\s*(\S+)`)
var apiKeyLine = regexp.MustCompile(`(?m)^Key:\s*(\S+)`)

func (a *tenantAdmin) createTenant(ctx context.Context, name, displayName string) (uuid.UUID, error) {
	out, err := a.run(ctx, "--create-tenant", name, "--tenant-display-name", displayName, "--org", a.orgID, "--db", a.dbURL)
	if err != nil {
		return uuid.Nil, err
	}
	m := tenantIDLine.FindStringSubmatch(out)
	if m == nil {
		return uuid.Nil, fmt.Errorf("--create-tenant printed no \"Tenant ID:\" line: %s", out)
	}
	return uuid.Parse(m[1])
}

func (a *tenantAdmin) generateAPIKey(ctx context.Context, tenantID uuid.UUID) (string, error) {
	out, err := a.run(ctx, "--generate-api-key", tenantID.String(), "--db", a.dbURL)
	if err != nil {
		return "", err
	}
	m := apiKeyLine.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("--generate-api-key printed no \"Key:\" line: %s", out)
	}
	return m[1], nil
}

// deployWorkflowDef registers the SAME wasm the operator's own `cleat
// deploy` used, again, under tenantID -- see newTenantAdmin's comment on
// wasmPath for why a per-tenant re-deploy is necessary at all. Same binary
// and same DSN class an operator would use (cleat, not cleat-worker: this
// is `cleat deploy`, not a tenant-lifecycle operation), consistent with the
// package doc comment's reasoning for shelling out rather than importing
// auth/ directly.
//
// --tenant MUST PRECEDE "deploy": it is a global flag on cleat's top-level
// FlagSet, not deploy's own -- see that flag's registration comment in
// cmd/cleat/main.go. `cleat deploy --tenant X` parses as deploy's OWN
// ExitOnError FlagSet, which does not know -tenant and exits 2 with "flag
// provided but not defined: -tenant"; confirmed live, not assumed, running
// this scenario end to end before this comment was written.
func (a *tenantAdmin) deployWorkflowDef(ctx context.Context, tenantID uuid.UUID) error {
	_, err := a.runBin(ctx, a.cleatBin,
		"--tenant", tenantID.String(),
		"deploy",
		"--db", a.dbURL,
		"--name", workflowName,
		a.wasmPath,
	)
	return err
}

func (a *tenantAdmin) run(ctx context.Context, args ...string) (string, error) {
	return a.runBin(ctx, a.workerBin, args...)
}

func (a *tenantAdmin) runBin(ctx context.Context, bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", bin, strings.Join(redactDBFlag(args), " "), err, out)
	}
	return string(out), nil
}

// redactDBFlag replaces the value following a "--db" argument with a
// placeholder before an argument list is ever formatted into an error --
// every caller's args include "--db", a.dbURL, and a.dbURL is a full
// PostgreSQL DSN carrying a password (CLEAT_ADMIN_DB_URL). Without this,
// createTenant/generateAPIKey/deployWorkflowDef's callers all s.log.Error
// this err, so a single failed signup would have put the ADMIN connection
// string, password included, into this process's logs -- the HTTP response
// stays generic (backendkit.WriteInternalError never sees err.Error()), but
// the log line does not, and examples are what readers copy (same class as
// cleat#2247). cmd's own output ("out") is left alone: it can contain a
// legitimate error from the invoked binary, not a copy of its own argv.
func redactDBFlag(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i, a := range out {
		if a == "--db" && i+1 < len(out) {
			out[i+1] = "REDACTED"
		}
	}
	return out
}

// serveFile mirrors examples/order-lifecycle/backend/main.go's helper
// exactly, including the reasoning in its comment there: the signature is
// (root, name, type) rather than (path) so nothing request-derived ever
// reaches the filesystem, visibly to a reader and to gosec's G703 alike.
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
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:")
		_, _ = w.Write(body)
	}
}

// authTransport attaches one tenant's API key. Matches
// examples/order-lifecycle/backend/main.go's authTransport exactly, except
// the key is chosen per client instance here rather than once at startup.
type authTransport struct{ key string }

func (t *authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Del("Authorization")
	r.Header.Del("Cookie")
	r.Header.Set("Authorization", "Bearer "+t.key)
	return http.DefaultTransport.RoundTrip(r)
}
