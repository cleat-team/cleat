package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/internal/tenantdrop"
)

// handleTenantDropPlan serves GET /api/admin/tenants/{tenant}/drop-plan (cleat#3552, B3b-1b): what dropping
// the tenant would remove, and the two facts a drop will require, without removing anything.
//
// It is `cleatctl drop-tenant`'s preview as an API, for the service that provisioned the tenant. The two
// read differently because the worker is not handed a superuser connection: the tenant's DATA is counted in
// the tenant's own scope through its own store (so a count cannot see another tenant's rows), and the
// admin.* tables, which that scope may not read, on the admin connection. The split was measured, not
// assumed; see internal/tenantdrop/plan.go.
//
// Who may call, the same as the suspend route: an admin-scoped operator key for any tenant; any other
// operator key only for a tenant its principal created; a tenant key never. A tenant that does not exist,
// one the caller did not create and one made offline all answer the same 404.
//
// It reads rows it has no tenant context for, so the answer is deliberately limited to counts: table names
// and numbers, never a row.
func (s *apiServer) handleTenantDropPlan(w http.ResponseWriter, r *http.Request, tenant string) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	creator, ok := s.provisionActor(w, r)
	if !ok {
		return
	}
	tid, err := uuid.Parse(tenant)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "tenant must be a UUID")
		return
	}
	if tenant == engine.DefaultTenantUUID {
		// The same refusal drop-tenant makes, said before any count is read: a plan for something that can
		// never be dropped would invite a request for the drop.
		s.writeError(w, http.StatusBadRequest, "the default tenant cannot be dropped")
		return
	}

	// The header first, and the creator check before ANYTHING else runs. Counting first would make a caller
	// with no claim to the tenant pay for the counts (four-plus queries against one for an absent tenant)
	// and see a 500 where an absent tenant answers 404, so the "same 404" below would be a promise the
	// timing and the error behaviour broke.
	header, err := s.provisioner.DropPlanHeader(r.Context(), tid)
	if err != nil {
		if errors.Is(err, auth.ErrTenantNotFound) {
			s.writeError(w, http.StatusNotFound, "not found")
			return
		}
		slog.Error("drop plan: reading the tenant failed", "tenant", tenant, "error", err)
		s.writeError(w, http.StatusInternalServerError, "reading the drop plan failed")
		return
	}
	op, _ := auth.OperatorFromContext(r.Context())
	if !op.IsAdmin() && (header.CreatedBy == "" || header.CreatedBy != creator) {
		s.writeError(w, http.StatusNotFound, "not found")
		return
	}
	facts, err := s.provisioner.DropPlanFacts(r.Context(), tid)
	if err != nil {
		slog.Error("drop plan: reading the admin counts failed", "tenant", tenant, "error", err)
		s.writeError(w, http.StatusInternalServerError, "reading the drop plan failed")
		return
	}

	st, ok := s.scopedStoreForTenant(w, r, tenant)
	if !ok {
		return
	}
	counter, ok := st.(engine.TenantRowCounter)
	if !ok {
		s.writeError(w, http.StatusNotImplemented, "this store cannot plan a drop")
		return
	}

	plan, err := buildDropPlan(r.Context(), counter, st, tenant, header, facts)
	if err != nil {
		slog.Error("drop plan: counting failed", "tenant", tenant, "error", err)
		s.writeError(w, http.StatusInternalServerError, "reading the drop plan failed")
		return
	}
	s.writeJSON(w, http.StatusOK, plan)
}

// buildDropPlan counts what a drop of tenant would remove. counter is the tenant-scoped store; facts is the
// admin-connection half.
//
// Which dialect is in use is read from what the store offers, not from a flag: only the SQL Server store can
// derive its table list from the catalogue, and it is the one that does. That list already contains the
// admin.* and plugin tables, so on SQL Server there is nothing for the admin half to add.
func buildDropPlan(ctx context.Context, counter engine.TenantRowCounter, store engine.WorkflowStore, tenant string, header auth.DropPlanHeader, facts auth.DropPlanFacts) (tenantdrop.Plan, error) {
	plan := tenantdrop.Plan{TenantID: tenant, Name: header.Name, Suspended: header.Suspended}

	var queries []tenantdrop.Table
	var plugin []tenantdrop.PluginTable
	unsettled := tenantdrop.Table{Label: "unsettled runs", Query: tenantdrop.UnsettledPostgres}
	if d, ok := store.(interface {
		DerivedTenantTables(context.Context) ([]tenantdrop.Table, error)
	}); ok {
		derived, err := d.DerivedTenantTables(ctx)
		if err != nil {
			return plan, err
		}
		// A retained table is reported as retained and not as removed, even if a catalogue sweep would
		// have listed it: the plan must not say a drop deletes what it keeps.
		for _, q := range derived {
			if !tenantdrop.IsRetained(q.Label) {
				queries = append(queries, q)
			}
		}
		unsettled.Query = tenantdrop.UnsettledMSSQL
	} else {
		queries = tenantdrop.PostgresDataTables()
		plugin = facts.PluginTables
	}
	// The table counts and the unsettled count are read in one transaction, but at READ COMMITTED each
	// statement still takes its own snapshot, so a run that starts or finishes while they are read can make
	// the counts disagree with each other by one. The plan is a snapshot taken statement by statement, not a
	// moment; a drop re-checks what it requires for itself.
	queries = append(queries, unsettled)

	counts, err := counter.CountTenantRows(ctx, queries)
	if err != nil {
		return plan, err
	}
	for i := 0; i < len(queries)-1; i++ {
		plan.Tables = append(plan.Tables, tenantdrop.RowCount{Table: queries[i].Label, Rows: counts[i]})
		plan.TotalRows += counts[i]
	}
	plan.UnsettledRuns = counts[len(counts)-1]
	for _, a := range facts.AdminTables {
		plan.Tables = append(plan.Tables, a)
		plan.TotalRows += a.Rows
	}

	// Plugin tables one at a time, each in its own transaction. A plugin owns the grants on its table, so
	// the tenant's scope may not be able to read one; that must not take the rest of the plan with it, and
	// it must not be reported as an empty table. It is listed as uncounted instead.
	for _, p := range plugin {
		q := tenantdrop.PluginCountQuery(p)
		c, err := counter.CountTenantRows(ctx, []tenantdrop.Table{q})
		if err != nil {
			slog.Warn("drop plan: a registered plugin table could not be counted", "table", q.Label, "error", err)
			plan.UncountedPluginTables = append(plan.UncountedPluginTables, q.Label)
			continue
		}
		plan.PluginTables = append(plan.PluginTables, tenantdrop.RowCount{Table: q.Label, Rows: c[0]})
		plan.TotalRows += c[0]
	}
	plan.SkippedPluginTables = facts.SkippedPluginTables
	plan.Retained = append([]tenantdrop.RetainedTable(nil), tenantdrop.Retained...)
	return plan, nil
}
