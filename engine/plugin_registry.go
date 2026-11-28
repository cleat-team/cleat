package engine

// cleat#3600 (owner D52): the record of which plugin NAMES some worker has registered, at
// least once.
//
// admin.workers cannot carry this. A worker's row there is deleted on a clean shutdown
// (Deregister) and by every worker's expiry sweep (SweepExpired), so a question asked of it
// is "is some worker live right now", and a deploy that depended on that answer would start
// failing across a rolling restart or a maintenance window. This table is written at
// registration and read by the deploy and install checks, and NOTHING deletes from it.
//
// Keyed by name only: versions are a claim-time check, because only the claiming worker
// knows its own loaded set. Global, not per tenant, for the reason admin.workers is: a worker
// loads one plugin set for every tenant it serves.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

const maxRegisteredPluginNameLen = 128

// PluginSource says which of a set of plugin names are known. It is the seam between the
// check (CheckPluginDeps) and wherever the answer lives, so a test can supply the answer
// without a database and a different definition of "known" is one new implementation.
type PluginSource interface {
	// KnownPlugins returns, for each name asked about, whether it is known. A name that is
	// not in the returned map is not known.
	KnownPlugins(ctx context.Context, names []string) (map[string]bool, error)
}

// PluginNameRegistry is the database-backed PluginSource and the writer of the record. Like
// WorkerRegistry it is a small type over *sql.DB and not a WorkflowStore method, because the
// Store interface has dozens of implementations and this needs one table.
type PluginNameRegistry struct {
	DB      *sql.DB
	Dialect Dialect
}

var _ PluginSource = (*PluginNameRegistry)(nil)

func (d Dialect) registeredPluginsTable() string {
	if d == DialectMySQL {
		return "registered_plugins"
	}
	return "admin.registered_plugins"
}

// validRegisteredPluginName is the table's CHECK, applied before any statement runs: a
// rejected row inside a PostgreSQL transaction aborts the transaction, and registration
// runs inside one (RegisterUnderKeyGate).
func validRegisteredPluginName(name string) error {
	if name == "" || len(name) > maxRegisteredPluginNameLen {
		return fmt.Errorf("plugin name %q is not recordable: 1 to %d bytes", name, maxRegisteredPluginNameLen)
	}
	return nil
}

// recordPluginsOn records each name as seen now: first_seen_at is set once, last_seen_at is
// refreshed on every call. It runs on the caller's connection or transaction, which is how
// worker registration makes it part of the same span as the worker's own row.
//
// One statement per name rather than one batch, because the three dialects share no upsert
// syntax and a worker loads a handful of plugins.
func recordPluginsOn(ctx context.Context, q querier, d Dialect, names []string) error {
	for _, n := range names {
		if err := validRegisteredPluginName(n); err != nil {
			return fmt.Errorf("plugin registry: %w", err)
		}
	}
	tbl := d.registeredPluginsTable()
	for _, n := range names {
		var stmt string
		switch d {
		case DialectPostgres:
			stmt = "INSERT INTO " + tbl + " (plugin_name, first_seen_at, last_seen_at) VALUES (" +
				d.placeholder(1) + ", now(), now()) ON CONFLICT (plugin_name) DO UPDATE SET last_seen_at = now()"
		case DialectMySQL:
			stmt = "INSERT INTO " + tbl + " (plugin_name, first_seen_at, last_seen_at) VALUES (" +
				d.placeholder(1) + ", NOW(6), NOW(6)) ON DUPLICATE KEY UPDATE last_seen_at = NOW(6)"
		case DialectMSSQL:
			stmt = "MERGE " + tbl + " WITH (HOLDLOCK) AS target USING (VALUES (" + d.placeholder(1) +
				")) AS src (plugin_name) ON target.plugin_name = src.plugin_name " +
				"WHEN MATCHED THEN UPDATE SET last_seen_at = SYSUTCDATETIME() " +
				"WHEN NOT MATCHED THEN INSERT (plugin_name, first_seen_at, last_seen_at) " +
				"VALUES (src.plugin_name, SYSUTCDATETIME(), SYSUTCDATETIME());"
		default:
			panic("unknown dialect: " + d)
		}
		if _, err := q.ExecContext(ctx, stmt, n); err != nil {
			return fmt.Errorf("plugin registry: record %q: %w", n, err)
		}
	}
	return nil
}

// Record records names as seen now. See recordPluginsOn.
func (r *PluginNameRegistry) Record(ctx context.Context, names []string) error {
	return recordPluginsOn(ctx, r.DB, r.Dialect, names)
}

// knownPluginsOn reads which of names are in the table, on the caller's connection or
// transaction.
func knownPluginsOn(ctx context.Context, q querier, d Dialect, names []string) (map[string]bool, error) {
	known := make(map[string]bool, len(names))
	if len(names) == 0 {
		return known, nil
	}
	uniq := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			uniq = append(uniq, n)
		}
	}
	sort.Strings(uniq)
	ph := make([]string, len(uniq))
	args := make([]any, len(uniq))
	for i, n := range uniq {
		ph[i] = d.placeholder(i + 1)
		args[i] = n
	}
	stmt := "SELECT plugin_name FROM " + d.registeredPluginsTable() + " WHERE plugin_name IN (" + strings.Join(ph, ", ") + ")"
	rows, err := q.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("plugin registry: read: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("plugin registry: read: scan: %w", err)
		}
		known[n] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("plugin registry: read: %w", err)
	}
	return known, nil
}

// KnownPlugins implements PluginSource.
func (r *PluginNameRegistry) KnownPlugins(ctx context.Context, names []string) (map[string]bool, error) {
	return knownPluginsOn(ctx, r.DB, r.Dialect, names)
}
