package engine

import (
	"encoding/json"
	"log/slog"
)

// decodePluginDeps parses a workflow_defs.plugin_deps blob.
//
// An unparseable blob is logged and treated as no dependencies, rather than
// returned as an error. That is deliberate and it is the weaker of the two
// options, so the reasoning is worth recording.
//
// Returning the error is the strictly correct behaviour and is what the other
// dropped-Unmarshal fixes in this area do. It cannot be done here without
// breaking existing SQL Server deployments. Until the write fix in this same
// change, MSSQLStore.DeployWorkflowDef passed the marshalled JSON as []byte,
// which go-mssqldb binds as VARBINARY; the implicit conversion into the column's
// NVARCHAR(MAX) reinterprets the UTF-8 bytes as UTF-16, so
//
//	{"llm":"1.2.0"}   round-tripped as   ≻汬≭∺⸱⸲∰}
//
// Every plugin_deps row written by SQL Server before this change is mangled.
// Making the read fail would turn a latent data bug into an outage: a
// GetWorkflowDef that errors means the workflow cannot be loaded at all. So the
// read stays permissive and self-heals on the next deploy of each definition,
// while the write is now correct.
//
// What changes is that it is no longer *silent*. The dropped error is the only
// reason this survived: every caller saw a plausible "this workflow declares no
// plugin dependencies" and nothing said otherwise.
func decodePluginDeps(log *slog.Logger, raw []byte, defName string, version int) map[string]string {
	deps := map[string]string{}
	if len(raw) == 0 {
		return deps
	}
	if err := json.Unmarshal(raw, &deps); err != nil {
		log.Warn("unreadable plugin_deps; treating the workflow as having none",
			"def_name", defName, "def_version", version, "error", err)
		return map[string]string{}
	}
	if deps == nil {
		deps = map[string]string{}
	}
	return deps
}
