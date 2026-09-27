package main

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/cleat-team/cleat/migration"
	"github.com/cleat-team/cleat/plugin"
)

// verifySchema is what a normal worker start does in place of migrating
// (cleat#2117). It returns an error -- whose text is the remediation -- when the
// schema is BEHIND this binary, and nil otherwise. It changes nothing in the database.
//
// The rule, for the core and the plugin schema alike:
//
//	behind  a migration this binary ships is not applied  -> error
//	equal                                                  -> nil
//	ahead   applied versions this binary does not ship     -> nil, and a warning
//
// See migration.SchemaState for why ahead starts: a rolling upgrade migrates to
// N+1 while workers on N are still running or restarting, so refusing there wedges
// the rollout on the workers it is trying to replace. Relying on migrations staying
// additive within a release line is the price, and the warning names both versions
// so the reliance is visible.
func verifySchema(ctx context.Context, runner *migration.Runner, db *sql.DB, dialect plugin.Dialect,
	plugins []*plugin.LoadedPlugin, schema string, warn func(msg string, args ...any)) error {
	st, err := runner.Verify(ctx)
	if err != nil {
		return fmt.Errorf("could not establish whether the schema is current: %w", err)
	}
	if st.Behind() {
		return &migration.SchemaBehindError{State: st}
	}
	if len(st.Ahead) > 0 && warn != nil {
		warn("the database schema is AHEAD of this worker: it has migrations applied that this binary does "+
			"not ship. This is expected while a rolling upgrade is in progress (the deploy step migrates to the "+
			"new version while workers on the old one are still running). It relies on migrations staying "+
			"additive within a release line.",
			"this_binary_latest_migration", st.LatestShipped,
			"database_latest_migration", st.LatestApplied,
			"applied_but_not_shipped", st.Ahead)
	}

	ps, err := plugin.VerifyMigrations(ctx, db, dialect, plugins, plugin.WithSchema(schema))
	if err != nil {
		return fmt.Errorf("could not establish whether the plugin schema is current: %w", err)
	}
	if ps.Behind() {
		return &plugin.PluginSchemaBehindError{State: ps}
	}
	return nil
}
