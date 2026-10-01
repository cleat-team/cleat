package engine

import (
	"encoding/json"
	"log/slog"
)

// decodeEntryPointSchemas parses a workflow_defs.entry_point_schemas blob.
//
// Mirrors decodePluginDeps: an unparseable blob is logged and treated as "no
// schemas", rather than failing the read -- the same reasoning applies here
// with less history behind it, since this column has had only one writer
// (cleatctl deploy) from the start, but a read that can fail on a column
// nothing but this codebase has ever written is still a worse failure mode
// than a workflow definition the caller cannot load at all.
func decodeEntryPointSchemas(log *slog.Logger, raw []byte, defName string, version int) map[string]EntryPointSchema {
	schemas := map[string]EntryPointSchema{}
	if len(raw) == 0 {
		return schemas
	}
	if err := json.Unmarshal(raw, &schemas); err != nil {
		log.Warn("unreadable entry_point_schemas; treating the workflow as having none",
			"def_name", defName, "def_version", version, "error", err)
		return map[string]EntryPointSchema{}
	}
	if schemas == nil {
		schemas = map[string]EntryPointSchema{}
	}
	return schemas
}
