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
// migrations/postgres/005_app_role.sql adds the role to connect as instead.
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
	b.WriteString("\n\nTo fix: apply migrations/postgres/005_app_role.sql, give the cleat_app role " +
		"a password (ALTER ROLE cleat_app LOGIN PASSWORD '...'), and point --db at it. " +
		"Keep the owner DSN for --migrate-db, which still needs DDL rights.")
	return b.String()
}
