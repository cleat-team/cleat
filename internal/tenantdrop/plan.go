package tenantdrop

import (
	"fmt"
	"strings"
)

// The pieces of a drop plan that are not a table list (cleat#3552, B3b-1b).
//
// A plan says what dropping a tenant would remove and whether the tenant is ready to be dropped,
// without removing anything. It is cleatctl's drop-tenant preview made available to the worker.
// It cannot be cleatctl's code run in the worker, because the two connect differently: cleatctl is
// handed a superuser connection that RLS does not apply to, and the worker is not. So the plan is
// assembled from two reads, split along a line that was MEASURED rather than assumed (PostgreSQL,
// the RLS role, a tenant-scoped transaction): 24 of the 27 PostgresTables count there, and the
// three that do not are admin.tenant_api_keys, admin.tenant_roles and admin.tenant_egress_allow,
// which that role may not read.
//
//   - the tenant's DATA (everything outside the admin schema) is counted in that tenant's own
//     scope, through its own store, so a count cannot see another tenant's rows;
//   - the admin.* tables are counted on the admin connection, which has the privilege and where
//     they carry no row-level security.

// PostgresDataTables is PostgresTables without the admin.* entries: what a tenant's own scope can
// count.
func PostgresDataTables() []Table {
	var out []Table
	for _, t := range PostgresTables {
		if !strings.HasPrefix(t.Label, "admin.") {
			out = append(out, t)
		}
	}
	return out
}

// PostgresAdminTables is the admin.* entries of PostgresTables: what the admin connection counts.
func PostgresAdminTables() []Table {
	var out []Table
	for _, t := range PostgresTables {
		if strings.HasPrefix(t.Label, "admin.") {
			out = append(out, t)
		}
	}
	return out
}

// UnsettledPostgres counts a tenant's runs that have not reached a terminal status. PostgreSQL
// keeps a run's status on workflow_leases (it is mirrored there; workflow_instances has no status
// column), SQL Server and MySQL on workflow_instances itself, which is why there are two.
//
// The settled set is spelled out, not built from a constant: engine's settledStatusList is the
// canonical spelling, and engine's TestTheDropPlanSpellsSettledLikeTheEngine fails if either of
// these drifts from it.
const UnsettledPostgres = `SELECT count(*) FROM workflow_leases WHERE tenant_id = $1 AND status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled')`

// UnsettledMSSQL is UnsettledPostgres for SQL Server.
const UnsettledMSSQL = `SELECT count(*) FROM workflow_instances WHERE tenant_id = @p1 AND status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled')`

// PluginTable is a table a plugin registered in admin.plugin_tables as holding per-tenant rows.
type PluginTable struct {
	Schema string
	Table  string
}

// Label is the schema-qualified name as the plan reports it.
func (p PluginTable) Label() string { return p.Schema + "." + p.Table }

// PluginCountQuery counts a tenant's rows in a plugin table, PostgreSQL only. The identifiers come
// from the registry, which a plugin writes, so they are quoted as identifiers rather than trusted;
// the same is true of admin.drop_tenant's format('%I.%I').
func PluginCountQuery(p PluginTable) Table {
	return Table{
		Label: p.Label(),
		Query: fmt.Sprintf(`SELECT count(*) FROM %s.%s WHERE tenant_id = $1`, quoteIdent(p.Schema), quoteIdent(p.Table)),
	}
}

// quoteIdent renders a PostgreSQL identifier the way quote_ident does: double quotes, with an
// embedded double quote doubled.
func quoteIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

// RetainedTable is a table that a drop deliberately LEAVES holding rows that name the tenant, and the
// reason. A plan that lists only what is removed leaves a reader to infer that everything else is kept, and
// an inference is not a statement: the retained list is the statement, and a test pins that nothing is on
// both lists.
type RetainedTable struct {
	Table  string `json:"table"`
	Reason string `json:"reason"`
}

// Retained is every table a tenant drop keeps. A table is added here when something decides its rows must
// outlive the tenant, and it must then be absent from PostgresTables and from what SQL Server's catalogue
// sweep deletes -- which on SQL Server means it has no column named tenant_id, because admin.drop_tenant
// there deletes from every table that does (migrations/mssql/003_procedures.sql). The tombstone is keyed by
// dropped_tenant_id for exactly that reason.
//
// The second entry a reader may expect, admin.plugin_dep_overrides, is added by whichever of cleat#3604
// and the tenant-drop change lands second: the plan cannot name a table that is not yet on develop.
var Retained = []RetainedTable{
	{
		Table: "admin.tenant_tombstones",
		Reason: "the record this drop leaves: who dropped the tenant, when, and where its own audit chain ended. " +
			"It is written in the drop's transaction and is what stops the id being reused",
	},
}

// IsRetained reports whether a table label names a retained table.
func IsRetained(label string) bool {
	for _, r := range Retained {
		if r.Table == label {
			return true
		}
	}
	return false
}

// RowCount is one table's row count in a plan.
type RowCount struct {
	Table string `json:"table"`
	Rows  int64  `json:"rows"`
}

// Plan is what dropping a tenant would remove, and whether it may be dropped now.
type Plan struct {
	TenantID  string `json:"tenant_id"`
	Name      string `json:"name"`
	Suspended bool   `json:"suspended"`

	// UnsettledRuns is the number of the tenant's runs that have not reached a terminal status. A
	// drop requires it to be zero, because dropping removes a run that is still executing.
	UnsettledRuns int64 `json:"unsettled_runs"`

	// Tables lists every table counted, zeros included: a table that is absent from the list and a
	// table that is empty must not look alike.
	Tables    []RowCount `json:"tables"`
	TotalRows int64      `json:"total_rows"`

	// PluginTables are the registered per-tenant plugin tables that exist (PostgreSQL). On SQL
	// Server they are in Tables, because that dialect derives its table list from the catalogue.
	PluginTables []RowCount `json:"plugin_tables,omitempty"`

	// UncountedPluginTables are registered plugin tables that exist but that the tenant's scope could not
	// count (for example a table whose grants never reached the tenant role). They are listed rather than
	// reported as zero, because a table that could not be read is not an empty one, and rather than
	// failing the whole plan, because one mis-granted plugin table would then hide everything else.
	// admin.drop_tenant still deletes their rows.
	UncountedPluginTables []string `json:"uncounted_plugin_tables,omitempty"`

	// SkippedPluginTables are registry rows that name a table that does not exist. admin.drop_tenant
	// skips them with a warning, so the plan says so instead of counting zero.
	SkippedPluginTables []string `json:"skipped_plugin_tables,omitempty"`

	// Retained are the tables a drop keeps, with the reason. Always present, never counted: what is kept is
	// not part of total_rows, which is what the drop removes.
	Retained []RetainedTable `json:"retained"`
}
