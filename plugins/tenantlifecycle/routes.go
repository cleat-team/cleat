package tenantlifecycle

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/plugin"
)

// RegisterRoutes registers HTTP handlers for the tenant-lifecycle plugin.
//
// TENANT-SCOPED ONLY, DELIBERATELY. cleat#2169 (owner decision, 2026-09-28)
// settled that a cross-tenant HTTP surface is wanted eventually but not now
// -- today no caller can act on another tenant, and this plugin does not
// change that. There is no route here that lists, creates, or acts on any
// tenant other than the caller's own; a tenant reads its own trial status
// and nothing else. That also means this route needs no GetTenant-style
// grant: it reads only tenant_trials, which this plugin already owns and
// already scopes by tenant_id, the same as every other tenant-facing route
// in this codebase (see plugins/eventtriggers/routes.go).
func (p *Plugin) RegisterRoutes(mux plugin.Router) error {
	if mux == nil {
		return fmt.Errorf("tenant-lifecycle: nil mux")
	}
	mux.HandleFunc("GET /api/tenant/lifecycle", p.handleGetLifecycleStatus)
	return nil
}

// ---- types ----

// lifecycleStatusResponse reports the caller's own trial state.
//
// NO "suspended" FIELD. Suspension itself lives on admin.tenants, a core
// table this plugin has no read grant for -- SetTenantSuspended (Stage 1)
// is write-only and host-owned, and no plugin in this tree queries
// admin.tenants directly outside its own tests (checked:
// `grep -rln admin.tenants plugins/*/*.go` before writing this file). Adding
// a read grant here for one field this route does not strictly need would be
// scope creep past what cleat#2534's Stage 3 asked for; a tenant that wants
// to know if it is suspended already finds out the moment it tries to start
// a workflow (403), which is the existing, working signal.
type lifecycleStatusResponse struct {
	// HasTrial is false when the tenant has no tenant_trials row at all --
	// distinct from an empty 200 with zero values, which would be
	// indistinguishable from "trial expires at the Unix epoch".
	HasTrial  bool      `json:"has_trial"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	Handled   bool      `json:"handled"`
}

// ---- helpers ----

func (p *Plugin) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (p *Plugin) writeError(w http.ResponseWriter, status int, msg string) {
	p.writeJSON(w, status, map[string]string{"error": msg})
}

// ---- GET /api/tenant/lifecycle ----

func (p *Plugin) handleGetLifecycleStatus(w http.ResponseWriter, r *http.Request) {
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
		p.writeError(w, 401, "tenant required")
		return
	}

	var expiresAt time.Time
	var handled bool
	err := plugin.ScanRow(
		p.db.QueryRow(r.Context(), queryOwnTrial.For(p.dialect), tid),
		&expiresAt, &handled,
	)
	if errors.Is(err, sql.ErrNoRows) {
		p.writeJSON(w, 200, lifecycleStatusResponse{HasTrial: false})
		return
	}
	if err != nil {
		p.logger.Error("tenant-lifecycle: query own trial", "error", err)
		p.writeError(w, 500, "failed to query trial status")
		return
	}

	p.writeJSON(w, 200, lifecycleStatusResponse{
		HasTrial:  true,
		ExpiresAt: expiresAt,
		Handled:   handled,
	})
}

// queryOwnTrial reads the caller's own tenant_trials row. $1/?/@p1 is the
// caller's own tenant id, resolved from the request's API key by
// auth.TenantIDFromRequest -- never a path or query parameter, which would
// let a caller ask about a tenant that is not its own.
//
// ONE STRING, NO PER-DIALECT COPIES (Query.For falls back to Default when
// MySQL/MSSQL are unset). Three byte-identical copies would leave the MSSQL
// one untested in isolation: MSSQL's own security policy masks a broken
// predicate (see the routes_test.go falsification), so a mutation applied
// only to a separate MSSQL string would pass every test unnoticed. A single
// shared string means the predicate is falsified wherever it is actually
// exercised (PostgreSQL and MySQL, per that same test's comment) and MSSQL
// runs the identical, proven text -- cleat-review on #2683.
var queryOwnTrial = plugin.Query{
	Default: `SELECT expires_at, handled FROM tenant_trials WHERE tenant_id = $1`,
}
