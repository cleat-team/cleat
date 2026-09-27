package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// runEgressAllow manages a tenant's egress allowlist.
//
// cleat#1565. The enforcement half is in the worker: a guest-initiated
// http.fetch reaches only hosts on this list, and an EMPTY LIST PERMITS
// NOTHING. Without a way to write the list, that policy would be a table
// nobody could populate except by hand-written SQL -- which is the state
// tenant_settings was in for months (cleat#1187), and the reason this ships in
// the same change rather than after it.
//
//	cleatctl egress-allow list   <tenant-uuid>
//	cleatctl egress-allow add    <tenant-uuid> <host> [host...]
//	cleatctl egress-allow remove <tenant-uuid> <host> [host...]
//
// Entry forms, and the command REFUSES anything else rather than storing a
// value the worker will silently never match:
//
//	example.com     exact host, case-insensitive
//	.example.com    any host ending in .example.com; the APEX is excluded
func runEgressAllow(ctx context.Context, db *sql.DB, d dialect, args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: cleatctl egress-allow <list|add|remove> <tenant-uuid> [host...]")
		os.Exit(2)
	}
	sub, tenantArg := args[0], args[1]
	tenant, err := uuid.Parse(tenantArg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "not a tenant UUID: %q\n", tenantArg)
		os.Exit(2)
	}
	hosts := args[2:]

	switch sub {
	case "list":
		entries, err := egressList(ctx, db, d, tenant.String())
		if err != nil {
			fmt.Fprintf(os.Stderr, "reading the allowlist: %v\n", err)
			os.Exit(1)
		}
		if len(entries) == 0 {
			// Said explicitly. An empty listing and "this tenant may reach
			// nothing" look identical, and only one of them is what an
			// operator expects to have configured.
			fmt.Printf("tenant %s has an EMPTY egress allowlist: its workflows may fetch nothing.\n", tenant)
			return
		}
		fmt.Printf("tenant %s may fetch %d host(s):\n", tenant, len(entries))
		for _, e := range entries {
			fmt.Printf("  %s\n", e)
		}
	case "add":
		if len(hosts) == 0 {
			fmt.Fprintln(os.Stderr, "add needs at least one host")
			os.Exit(2)
		}
		for _, h := range hosts {
			norm, err := normaliseEgressHost(h)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				os.Exit(2)
			}
			if err := egressAdd(ctx, db, d, tenant.String(), norm); err != nil {
				fmt.Fprintf(os.Stderr, "adding %q: %v\n", norm, err)
				os.Exit(1)
			}
			fmt.Printf("added %s\n", norm)
		}
		fmt.Println("Workers pick this up within their allowlist cache TTL (30s by default).")
	case "remove":
		if len(hosts) == 0 {
			fmt.Fprintln(os.Stderr, "remove needs at least one host")
			os.Exit(2)
		}
		for _, h := range hosts {
			norm, _ := normaliseEgressHost(h) // removal of an invalid entry is still a removal
			n, err := egressRemove(ctx, db, d, tenant.String(), norm)
			if err != nil {
				fmt.Fprintf(os.Stderr, "removing %q: %v\n", norm, err)
				os.Exit(1)
			}
			if n == 0 {
				fmt.Printf("%s was not on the list\n", norm)
				continue
			}
			fmt.Printf("removed %s\n", norm)
		}
		fmt.Println("Workers stop honouring it within their allowlist cache TTL (30s by default).")
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q; want list, add or remove\n", sub)
		os.Exit(2)
	}
}

// normaliseEgressHost lowercases and validates one entry.
//
// Refusing here rather than storing it is the point: a scheme, a port or a
// path in this column is a row the worker will never match, and the operator
// would see "refused by policy" with the host sitting right there in the
// listing.
func normaliseEgressHost(h string) (string, error) {
	n := strings.ToLower(strings.TrimSpace(h))
	n = strings.TrimSuffix(n, ".")
	switch {
	case n == "" || n == ".":
		return "", fmt.Errorf("empty host")
	case strings.Contains(n, "://"):
		return "", fmt.Errorf("%q looks like a URL; give a host, not a scheme", h)
	case strings.ContainsAny(n, "/?#"):
		return "", fmt.Errorf("%q contains a path; give a host only", h)
	case strings.Contains(n, ":"):
		return "", fmt.Errorf("%q contains a port; the allowlist is per host, not per port", h)
	case strings.Contains(n, "*"):
		return "", fmt.Errorf("%q uses a wildcard; write .example.com for subdomains", h)
	}
	return n, nil
}

// The statements, as plugin.Query values with a per-dialect arm.
//
// Three constraints meet here and only this shape satisfies all of them:
//
//   - TestEveryInlineStatementParsesOnPostgres PREPAREs inline SQL against the
//     real schema, so a statement assembled by concatenation is a fragment it
//     cannot parse. The table name has to be written out.
//   - MySQL has no schemas, so its table is unprefixed -- and that statement
//     is not valid against PostgreSQL, where no such relation exists.
//   - That same guard prunes a plugin.Query's MySQL and MSSQL arms by KEY,
//     precisely because they are never sent to PostgreSQL.
//
// So the Default arm is checked, the MySQL arm is correctly exempt, and
// nothing is added to a baseline.
var (
	egressListSQL = plugin.Query{
		Default: `SELECT host FROM admin.tenant_egress_allow WHERE tenant_id = $1`,
		MySQL:   `SELECT host FROM tenant_egress_allow WHERE tenant_id = ?`,
	}
	egressInsertSQL = plugin.Query{
		Default: `INSERT INTO admin.tenant_egress_allow (tenant_id, host) VALUES ($1, $2)`,
		MySQL:   `INSERT INTO tenant_egress_allow (tenant_id, host) VALUES (?, ?)`,
	}
	egressDeleteSQL = plugin.Query{
		Default: `DELETE FROM admin.tenant_egress_allow WHERE tenant_id = $1 AND host = $2`,
		MySQL:   `DELETE FROM tenant_egress_allow WHERE tenant_id = ? AND host = ?`,
	}
)

// egressStmt resolves the arm for this dialect and rebinds placeholders and
// args together. The MySQL arm is already hand-written with ? in ascending
// argument order, so RebindArgs finds no $N to reorder and passes both
// through unchanged -- the same no-op Rebind alone used to be for it.
func egressStmt(q plugin.Query, d dialect, args ...any) (string, []any, error) {
	return plugin.RebindArgs(q.For(d.query), d.query, args)
}

func egressList(ctx context.Context, db *sql.DB, d dialect, tenant string) ([]string, error) {
	stmt, stmtArgs, err := egressStmt(egressListSQL, d, tenant)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, stmt, stmtArgs...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	sort.Strings(out)
	return out, rows.Err()
}

func egressAdd(ctx context.Context, db *sql.DB, d dialect, tenant, host string) error {
	// Remove-then-insert rather than an upsert: the three dialects spell
	// ON CONFLICT / ON DUPLICATE KEY / MERGE three different ways, and a row
	// whose only columns are the key and a timestamp has nothing to merge.
	if _, err := egressRemove(ctx, db, d, tenant, host); err != nil {
		return err
	}
	stmt, stmtArgs, err := egressStmt(egressInsertSQL, d, tenant, host)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, stmt, stmtArgs...)
	return err
}

func egressRemove(ctx context.Context, db *sql.DB, d dialect, tenant, host string) (int64, error) {
	stmt, stmtArgs, err := egressStmt(egressDeleteSQL, d, tenant, host)
	if err != nil {
		return 0, err
	}
	res, err := db.ExecContext(ctx, stmt, stmtArgs...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
