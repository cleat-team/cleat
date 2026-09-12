package plugin

// Migration.Down finally has a caller. cleat#1290.
//
// Eighteen plugins populate it across twenty-nine sites, fourteen of them
// assert it is non-empty in their own tests across twenty-one assertions, and
// until now NOTHING read it: `git grep '\.Down' -- '*.go'` returned only
// plugin.DownloadWASM, a substring match. Plugin authors have been writing
// rollback SQL that no code path could execute, and their test suites have been
// enforcing that they keep doing it.
//
// Note the method used to establish that, because the obvious one is wrong:
// `git grep -E '\.Down\b'` matches NOTHING under git grep, which does not
// support \b -- so the absence of readers reads identically to a broken
// pattern. The control was the same pattern against .UpMySQL, which is
// demonstrably read, and also returned zero.
//
// WHAT THIS DELIBERATELY REFUSES TO DO is as important as what it does.
// Reversing some of a plugin's migrations and not the rest leaves a schema that
// is neither the old shape nor the new one, and no later run can tell which
// half it is looking at. So the whole operation is checked before any statement
// executes, and a gap is a refusal rather than a partial teardown.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// DownResult reports what a reversal did.
type DownResult struct {
	// Reversed lists the versions whose Down ran, newest first -- the order
	// they were applied in, backwards.
	Reversed []int

	// TenantScopedTables lists the tables those migrations had declared. Not
	// "tables dropped": the Down SQL is the plugin author's, and whether it
	// drops every table it created is their contract, not this function's
	// claim.
	TenantScopedTables []string
}

// RunDownMigrations reverses a plugin's applied migrations, newest first.
//
// others is the rest of the loaded plugin set, used for the collision check
// below. Pass the full set; the target is skipped by name.
//
// Every reversal runs in its own transaction, with the tracking row deleted in
// the same one: a Down that succeeds and a tracking row that survives would
// make the migration un-re-appliable, and the reverse would make it run twice.
// ON A PINNED SESSION, exactly as RunMigrations runs. cleat#1307/#1362.
//
// The first version used db.QueryRowContext and db.BeginTx directly, on
// whatever pool connection came back. That was wrong in a way local runs could
// not show: RunMigrations does its work on a session with
// `SET search_path = <schema>, pg_temp`, so plugin_migrations and every plugin
// table live in the CONFIGURED schema -- while a pool connection resolves
// `"$user", public`. Locally the role is postgres with no schema of that name,
// so the two coincide. In CI the role is `cleat` against a database with a
// `cleat` schema, so this read a different plugin_migrations, found nothing
// applied, and returned "nothing to do" -- which is why all three tests failed
// with the reversal silently doing nothing rather than with an error.
//
// Using the same helper rather than repeating the pin also inherits the
// advisory lock, so a reversal cannot interleave with another worker applying
// migrations.
func RunDownMigrations(ctx context.Context, db *sql.DB, dialect Dialect, target *LoadedPlugin, others []*LoadedPlugin, opts ...MigrationOption) (*DownResult, error) {
	if db == nil {
		return nil, fmt.Errorf("plugin: RunDownMigrations needs a database")
	}
	if target == nil || target.Plugin == nil {
		return nil, fmt.Errorf("plugin: RunDownMigrations needs a plugin")
	}
	name := target.Plugin.Info().Name

	var cfg migrationOptions
	for _, opt := range opts {
		opt(&cfg)
	}
	// Defaulted and validated exactly as RunMigrations does. Passing an empty
	// schema through makes pluginMigrationSession emit
	// `CREATE SCHEMA IF NOT EXISTS ` and fail with a syntax error at column 29
	// -- which is how this omission announced itself.
	if cfg.schema == "" {
		cfg.schema = "public"
	}
	if !plainIdentifier.MatchString(cfg.schema) {
		return nil, fmt.Errorf(
			"plugin: schema %q is not a plain identifier", cfg.schema)
	}
	session, release, err := pluginMigrationSession(ctx, db, dialect, cfg.schema)
	if err != nil {
		return nil, err
	}
	defer release()

	// HasMigrations, the same assertion RunMigrations makes. A plugin without
	// it declares no schema, so there is nothing to reverse and that is not an
	// error.
	withMigs, ok := target.Plugin.(HasMigrations)
	if !ok {
		return &DownResult{}, nil
	}
	migs := append([]Migration(nil), withMigs.Migrations()...)
	if len(migs) == 0 {
		return &DownResult{}, nil
	}
	// Newest first. Applied order reversed, so a migration that depends on an
	// earlier one is undone before the thing it depends on.
	sort.Slice(migs, func(i, j int) bool { return migs[i].Version > migs[j].Version })

	// ---- Phase 1: decide, touching nothing ----
	//
	// Both checks below are refusals, and both run before any statement, so a
	// refusal leaves the schema exactly as it was.

	var applied []Migration
	for _, m := range migs {
		var exists bool
		if err := session.QueryRowContext(ctx, checkPluginMigrationSQL(dialect), name, m.Version).Scan(&exists); err != nil {
			return nil, fmt.Errorf("plugin %s: check migration %d: %w", name, m.Version, err)
		}
		if exists {
			applied = append(applied, m)
		}
	}
	if len(applied) == 0 {
		return &DownResult{}, nil
	}

	// A GAP IS A REFUSAL. An applied migration with no Down cannot be reversed,
	// and reversing the ones around it would leave the schema in a state no
	// later run can identify.
	//
	// BUT ONLY WHERE THERE IS DDL TO UNDO, and that exception is not
	// hypothetical: kvstore v2 is `{Version: 2, TenantScoped: ["kv_store"]}` --
	// no Up, no Down, existing only to mark an existing table tenant-scoped
	// (cleat#1277). RunMigrations records it as applied regardless, so a check
	// that asked only "is Down empty" refused kvstore outright. Found by
	// running this against the real plugin set rather than against fixtures:
	// every fixture I wrote had an Up.
	//
	// A migration that created nothing has nothing to drop. Requiring Down for
	// it would make one plugin permanently un-uninstallable for declaring a
	// property rather than a table.
	var gaps []string
	for _, m := range applied {
		if !declaresDDL(m) {
			continue
		}
		if strings.TrimSpace(m.Down) == "" {
			gaps = append(gaps, fmt.Sprintf("%d", m.Version))
		}
	}
	if len(gaps) > 0 {
		return nil, fmt.Errorf(
			"plugin %s: cannot reverse: version(s) %s are applied and declare no Down. "+
				"Reversing the rest would leave a schema that is neither shape, so nothing "+
				"has been changed. Add Down for those versions, or drop the plugin's tables "+
				"by hand", name, strings.Join(gaps, ", "))
	}

	// AND A SHARED TABLE IS A REFUSAL. Plugin tables live in one flat namespace
	// (cleat#1288), so two plugins can declare the same name and this function
	// cannot tell whose rows a DROP would take. admin.plugin_tables exists to
	// answer exactly that and is never populated -- RegisterPluginTables has no
	// production caller -- so the check is made against the loaded set, which
	// is the information actually available.
	claimed := map[string][]string{}
	for _, m := range applied {
		for _, t := range m.TenantScoped {
			claimed[t] = append(claimed[t], name)
		}
	}
	for _, o := range others {
		if o == nil || o.Plugin == nil || o.Plugin.Info().Name == name {
			continue
		}
		otherMigs, ok := o.Plugin.(HasMigrations)
		if !ok {
			continue
		}
		for _, m := range otherMigs.Migrations() {
			for _, t := range m.TenantScoped {
				if _, ours := claimed[t]; ours {
					return nil, fmt.Errorf(
						"plugin %s: cannot reverse: it declares table %q, and so does plugin "+
							"%s. Dropping it would take the other plugin's rows with it. "+
							"Nothing has been changed (cleat#1288)",
						name, t, o.Plugin.Info().Name)
				}
			}
		}
	}

	// ---- Phase 2: execute ----
	res := &DownResult{}
	for _, m := range applied {
		// Untrack a no-DDL migration without executing anything: there is no
		// Down to run, and leaving the row would make it un-re-appliable.
		if !declaresDDL(m) {
			if _, err := session.ExecContext(ctx, deletePluginMigrationSQL(dialect), name, m.Version); err != nil {
				return res, fmt.Errorf("plugin %s: untracking %d: %w", name, m.Version, err)
			}
			res.Reversed = append(res.Reversed, m.Version)
			continue
		}
		// Down and its untracking go through the SESSION, not a fresh
		// transaction from the pool: a pool transaction would resolve
		// unqualified names against a different search_path and drop nothing,
		// which is the bug this whole function had. RunMigrations applies each
		// migration on the session for the same reason.
		if _, err := session.ExecContext(ctx, m.Down); err != nil {
			return res, fmt.Errorf("plugin %s: reversing %d: %w\n\nVersions already reversed: %v",
				name, m.Version, err, res.Reversed)
		}
		if _, err := session.ExecContext(ctx, deletePluginMigrationSQL(dialect), name, m.Version); err != nil {
			return res, fmt.Errorf("plugin %s: untracking %d: %w", name, m.Version, err)
		}
		res.Reversed = append(res.Reversed, m.Version)
		res.TenantScopedTables = append(res.TenantScopedTables, m.TenantScoped...)
	}
	return res, nil
}

// deletePluginMigrationSQL removes a tracking row so the migration can be
// applied again. Mirrors insertPluginMigrationSQL's dialect handling.
func deletePluginMigrationSQL(d Dialect) string {
	switch d {
	case DialectMySQL:
		return `DELETE FROM plugin_migrations WHERE plugin_name = ? AND version = ?`
	case DialectMSSQL:
		return `DELETE FROM plugin_migrations WHERE plugin_name = @p1 AND version = @p2`
	default:
		return `DELETE FROM plugin_migrations WHERE plugin_name = $1 AND version = $2`
	}
}

// declaresDDL reports whether a migration creates anything a Down could undo.
//
// A migration may legitimately carry only TenantScoped -- declaring that an
// existing table's rows belong to a tenant, which applyTenantScoping turns into
// row-level security. kvstore v2 is exactly that shape. Such a migration has no
// schema to reverse, so it needs no Down and must not block a reversal.
func declaresDDL(m Migration) bool {
	return strings.TrimSpace(m.Up) != "" ||
		strings.TrimSpace(m.UpMySQL) != "" ||
		strings.TrimSpace(m.UpMSSQL) != ""
}
