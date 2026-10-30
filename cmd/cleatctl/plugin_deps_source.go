package main

import (
	"database/sql"

	"github.com/cleat-team/cleat/engine"
)

// currentDialect is the dialect main resolved for this invocation, in the engine's own
// terms. It is set once, before any subcommand runs. Empty means "not resolved" (a test, or
// a path that does not go through main), and deployPluginSource then returns no source, so
// a definition that declares plugin dependencies is refused rather than waved through.
var currentDialect engine.Dialect

// deployPluginSource is where a deploy learns which plugins are known (cleat#3600): the
// plugin-name registry in the database the command is connected to. nil when there is no
// connection or no dialect, which the check reads as "cannot check" and the override as "cannot
// record", never "all known" and never "allowed".
// A variable so a test over a mock store can supply the answer.
var deployPluginSource = func(db *sql.DB) engine.PluginRegistryAccess {
	if db == nil || currentDialect == "" {
		return nil
	}
	return &engine.PluginNameRegistry{DB: db, Dialect: currentDialect}
}
