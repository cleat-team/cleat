package main

import (
	"encoding/json"
	"go/types"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/internal/analyzer"
	"github.com/cleat-team/cleat/internal/jsonschema"
	"github.com/cleat-team/cleat/wasm"
)

// buildEntryPointSchemas computes, for every entry point in result, the JSON
// Schema pair internal/jsonschema derives from its Go signature (cleat#1980).
//
// Keyed by WASM EXPORT name (wasm.ToSnakeCase of the short Go name), not the
// Go source name -- the same choice exportedEntryPointNames makes for
// wasm.Metadata.EntryPoints, and for the same reason given there: it is the
// name a caller (or, here, cleatctl deploy and the eventual OpenAPI endpoint)
// can actually look a workflow up by, not a Go-only identifier nothing
// outside this build process ever sees.
func buildEntryPointSchemas(result *analyzer.AnalysisResult) map[string]engine.EntryPointSchema {
	if result.TargetPkg == nil {
		return nil
	}
	qual := types.RelativeTo(result.TargetPkg.Types)
	schemas := map[string]engine.EntryPointSchema{}
	for _, epName := range result.EntryPoints {
		fd := result.Funcs[epName]
		if fd == nil || fd.Type == nil {
			continue
		}
		paramsJSON, err := json.Marshal(jsonschema.EntryPointParamSchema(fd, qual))
		if err != nil {
			continue
		}
		resultJSON, err := json.Marshal(jsonschema.EntryPointResultSchema(fd, qual))
		if err != nil {
			continue
		}
		exportName := wasm.ToSnakeCase(analyzer.ShortName(epName))
		schemas[exportName] = engine.EntryPointSchema{Params: paramsJSON, Result: resultJSON}
	}
	if len(schemas) == 0 {
		return nil
	}
	return schemas
}
