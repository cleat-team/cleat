package engine

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// RLSBypassReason describes one way the current connection escapes Row-Level
// Security. A connection with no reasons is genuinely subject to the policies.
type RLSBypassReason struct {
	// Kind is a short machine-readable label: "superuser", "bypassrls",
	// "rls_disabled", "owner_not_forced", or "no_policies".
	Kind string
	// Detail is the human-readable explanation, including the object it
	// applies to where there is one.
	Detail string
}

// CheckRLSEnforced reports every reason the connection behind db would not
// have Row-Level Security applied to it.
//
// This matters more here than the phrase "defence in depth" would suggest.
// GetWorkflowByID and ListWorkflows carry no application-level tenant_id
// filter at all, so for those paths the RLS policies are not one layer of
// isolation among several -- they are the only one. A connection that bypasses
// them sees every tenant's workflows, and nothing in the Go code will stop it.
//
// PostgreSQL exempts a role from RLS in two ways, and neither can be closed
// from inside the schema:
//
//   - Superuser, and BYPASSRLS. Unconditional. There is no FORCE that applies
//     to a superuser; it is documented behaviour, not an oversight.
//   - Table ownership, unless the table has FORCE ROW LEVEL SECURITY.
//     001_schema.sql sets FORCE on every tenant-scoped table, so this one is
//     closed as long as the migrations are applied -- which is exactly why it
//     is checked rather than assumed.
//
// Every configuration cleat shipped connected as a superuser
// (docker-compose.cluster.yml uses POSTGRES_USER=cleat; CI and local
// development use `postgres`), so the policies were present, correct, tested,
// and bypassed in practice by every connection that ever ran against them.
// The schema baseline creates the role to connect as instead -- cleat_app. It
// is named rather than cited by file: the postgres migrations were compacted
// into a generated baseline in 0.3.0 (cleat#2416), and a comment that names a
// migration file is a comment that has to be found and corrected at the next
// compaction. This one was not, and told readers to apply a file that had been
// deleted (cleat#2553).
//
// A nil, empty slice means RLS is enforced. Errors are returned only for
// failures to interrogate the database.
func CheckRLSEnforced(ctx context.Context, db *sql.DB) ([]RLSBypassReason, error) {
	var reasons []RLSBypassReason

	var user string
	var super, bypass bool
	err := db.QueryRowContext(ctx, `
		SELECT current_user,
		       coalesce((SELECT rolsuper      FROM pg_roles WHERE rolname = current_user), false),
		       coalesce((SELECT rolbypassrls  FROM pg_roles WHERE rolname = current_user), false)
	`).Scan(&user, &super, &bypass)
	if err != nil {
		return nil, fmt.Errorf("check RLS: read role attributes: %w", err)
	}

	if super {
		reasons = append(reasons, RLSBypassReason{
			Kind: "superuser",
			Detail: fmt.Sprintf("the connecting role %q is a superuser, and PostgreSQL "+
				"never applies row-level security to a superuser", user),
		})
	}
	if bypass {
		reasons = append(reasons, RLSBypassReason{
			Kind:   "bypassrls",
			Detail: fmt.Sprintf("the connecting role %q has the BYPASSRLS attribute", user),
		})
	}

	// Every table that carries a policy must have RLS switched on, and must
	// either not be owned by this role or have FORCE set.
	rows, err := db.QueryContext(ctx, `
		SELECT c.relname,
		       c.relrowsecurity,
		       c.relforcerowsecurity,
		       pg_get_userbyid(c.relowner) = current_user AS owned_by_me
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public'
		  AND c.relkind = 'r'
		  AND EXISTS (SELECT 1 FROM pg_policy p WHERE p.polrelid = c.oid)
		ORDER BY c.relname
	`)
	if err != nil {
		return nil, fmt.Errorf("check RLS: read table policies: %w", err)
	}
	defer rows.Close()

	var withPolicies int
	for rows.Next() {
		var name string
		var enabled, forced, owned bool
		if err := rows.Scan(&name, &enabled, &forced, &owned); err != nil {
			return nil, fmt.Errorf("check RLS: scan table policies: %w", err)
		}
		withPolicies++

		if !enabled {
			reasons = append(reasons, RLSBypassReason{
				Kind: "rls_disabled",
				Detail: fmt.Sprintf("table public.%s has policies but row-level security "+
					"is not enabled on it, so the policies do nothing", name),
			})
			continue
		}
		if owned && !forced {
			reasons = append(reasons, RLSBypassReason{
				Kind: "owner_not_forced",
				Detail: fmt.Sprintf("table public.%s is owned by the connecting role %q and "+
					"does not have FORCE ROW LEVEL SECURITY, so its owner is exempt from "+
					"its own policies", name, user),
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("check RLS: iterate table policies: %w", err)
	}

	// A database with no policies at all passes every check above while
	// isolating nothing. Reporting "enforced" for it would be the exact
	// failure this function exists to catch, so it is a finding in its own
	// right.
	if withPolicies == 0 {
		reasons = append(reasons, RLSBypassReason{
			Kind: "no_policies",
			Detail: "no table in schema public has any row-level security policy: the " +
				"tenant isolation policies from migrations/postgres/001_schema.sql are " +
				"not present in this database",
		})
	}

	return reasons, nil
}

// rlsBypassFix is the instruction both refusals end with, and it is one string so they cannot
// drift apart.
//
// THE INSTRUCTION NAMES THE STATE AND THE OBJECT, NEVER A FILE. It used to
// say "apply migrations/postgres/005_app_role.sql", and that file was
// deleted by the 0.3.0 postgres compaction -- so the engine was printing a
// wrong instruction, at an operator, at the moment tenant isolation had
// already failed (cleat#2553). A runtime message that cites a migration
// path is a message with an expiry date, and this one expired.
const rlsBypassFix = "To fix: connect as the cleat_app role instead of the owner. The schema " +
	"baseline creates it NOLOGIN and without a password, on purpose, so give it " +
	"one -- ALTER ROLE cleat_app LOGIN PASSWORD '...' -- and point --db at it. " +
	"If the role does not exist at all, the schema baseline has not been applied. " +
	"Keep the owner DSN for --migrate-db, which still needs DDL rights."

// FormatRLSBypass renders reasons as an operator-facing message, including
// what to do about it. Returns "" for no reasons.
func FormatRLSBypass(reasons []RLSBypassReason) string {
	if len(reasons) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("this connection is not subject to row-level security, so tenant isolation is not enforced:")
	for _, r := range reasons {
		b.WriteString("\n  - ")
		b.WriteString(r.Detail)
	}
	b.WriteString("\n\nGetWorkflowByID and ListWorkflows have no application-level tenant filter, " +
		"so on this connection they return every tenant's data.")
	b.WriteString("\n\n")
	b.WriteString(rlsBypassFix)
	return b.String()
}

// RLSMultiTenantRefusal is the finding that a connection which is not subject to row-level security
// is, or would be, serving more than one tenant. cleat#3772, owner decision D82.
//
// ONE TENANT IS THE LINE, and the reason is what the policies are for on such a connection: they are
// the only thing keeping one tenant's rows from another's, and the connection does not apply them.
// With a single tenant there is nothing to keep apart, which is the single-tenant laptop
// configuration (--migrate-on-start on the superuser DSN with --require-auth=false) the tutorial
// documents and this leaves alone. cleat#3740 is what two tenants on such a connection did: a run
// executed the other tenant's module.
type RLSMultiTenantRefusal struct {
	// Role is the connecting role.
	Role string
	// Tenants is how many tenants the database holds. When Creating, it is the number it WOULD hold.
	Tenants int
	// Reasons are every reason CheckRLSEnforced gave. All of them count: each one means isolation is
	// not enforced on this connection.
	Reasons []RLSBypassReason
	// Creating says this is the refusal of creating a tenant, not of starting.
	Creating bool
}

// CountTenants returns how many tenants the database holds: every row of admin.tenants, SUSPENDED
// ones included, because a resume puts a suspended tenant back in service with no restart in between.
// A dropped tenant has no row (it leaves a tombstone) and so does not count. admin.tenants carries no
// row-level security (engine.TenantLister), so this answers on any connection.
func CountTenants(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM admin.tenants`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count tenants: %w", err)
	}
	return n, nil
}

// CurrentRole returns the role db connects as, for the refusal's message.
func CurrentRole(ctx context.Context, db *sql.DB) (string, error) {
	var role string
	if err := db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role); err != nil {
		return "", fmt.Errorf("read current role: %w", err)
	}
	return role, nil
}

// DecideRLSMultiTenant returns the refusal for a connection with these reasons and this many tenants,
// or nil when there is none: the connection is subject to the policies (no reasons), or there is at
// most one tenant. It is a pure function of its arguments so the line it draws can be tested without
// a database.
func DecideRLSMultiTenant(role string, reasons []RLSBypassReason, tenants int, creating bool) *RLSMultiTenantRefusal {
	if len(reasons) == 0 || tenants <= 1 {
		return nil
	}
	return &RLSMultiTenantRefusal{Role: role, Tenants: tenants, Reasons: reasons, Creating: creating}
}

// CreatedWarning renders what an operator is told when a tenant was just created from a connection that is
// not subject to row-level security and the database now holds more than one. It is a WARNING and not a
// refusal because the creating command is not what serves (cleat#3772): the risk is the workers already
// serving this database on a bypassing role, which keep running with more than one tenant until they
// restart, and whose next start will refuse.
func (r *RLSMultiTenantRefusal) CreatedWarning() string {
	var b strings.Builder
	fmt.Fprintf(&b, "WARNING: this tenant was created on a connection (role %q) that is not subject to row-level security, "+
		"and the database now holds %d tenants. Any worker serving this database on a role that bypasses row-level security "+
		"is now serving more than one tenant, with nothing keeping one tenant's data from another's, until it restarts; "+
		"its next start will refuse (cleat#3772). Why this connection is not subject to it:", r.Role, r.Tenants)
	for _, reason := range r.Reasons {
		b.WriteString("\n  - ")
		b.WriteString(reason.Detail)
	}
	b.WriteString("\n\n")
	b.WriteString(rlsBypassFix)
	b.WriteString(" See docs/explanation/postgresql-schema.md, the section on row-level security.")
	return b.String()
}

// Message renders the refusal for an operator: the role, the tenant count, every reason, that neither
// --require-auth=false nor -rls-check=off lifts it, and the fix.
func (r *RLSMultiTenantRefusal) Message() string {
	var b strings.Builder
	if r.Creating {
		fmt.Fprintf(&b, "refusing to create a tenant: this worker connects as %q, which is not subject to row-level security, "+
			"and creating it would make %d tenants share the database", r.Role, r.Tenants)
	} else {
		fmt.Fprintf(&b, "refusing to start: this worker connects as %q, which is not subject to row-level security, "+
			"and the database holds %d tenants", r.Role, r.Tenants)
	}
	b.WriteString(", so nothing on this connection keeps one tenant's data from another's. Why it is not subject to it:")
	for _, reason := range r.Reasons {
		b.WriteString("\n  - ")
		b.WriteString(reason.Detail)
	}
	b.WriteString("\n\nWith one tenant there is nothing to keep apart, and that configuration still starts. " +
		"--require-auth=false and --rls-check=off do not lift this refusal.")
	b.WriteString("\n\n")
	b.WriteString(rlsBypassFix)
	return b.String()
}
