package main

import (
	"fmt"
	"io"

	"github.com/cleat-team/cleat/plugin"
)

// runListPlugins prints the plugins linked into this binary and returns the
// process exit code.
//
// It exists because "which plugins does this worker have" had no answer short of
// reading main.go's import block, and the answer surprised everyone who checked:
// exactly one. IMPROVEMENT-PLAN.md §3.315 records the measurement --
// event-triggers, event-store, webhook-ingest and kafka-connect are all built,
// tested, and documented as features, and linked by nothing. Their Migrations()
// therefore never run, so their tables exist in no deployed database, and
// `await_event` is unreachable from a workflow.
//
// A plugin is registered by an init() in its own package, which runs only if the
// package is linked into the binary. `--plugin-config` supplies configuration and
// cannot change that. **So the import block in main.go is the feature set**, and
// until this flag existed there was no way to read it off a built binary --
// which is why the gap survived: the plugins' own tests import them directly and
// pass, and a test that constructs the thing under test cannot tell you the
// product never constructs it.
//
// It calls Discover rather than List because Discover is what the worker itself
// calls at startup, and Discover's topological sort can fail on a dependency
// cycle. Surfacing that here, before a database is opened, is strictly better
// than discovering it during boot.
func runListPlugins(out io.Writer) int {
	loaded, err := plugin.Discover()
	if err != nil {
		fmt.Fprintf(out, "list-plugins: FAIL: %v\n", err)
		fmt.Fprintf(out, "\nDiscover() failed, which means the registered plugins do not form a\n"+
			"valid dependency order. The worker calls the same function at startup.\n")
		return 1
	}

	fmt.Fprintf(out, "list-plugins: %d plugin(s) linked into this binary\n", len(loaded))
	for _, lp := range loaded {
		info := lp.Plugin.Info()
		version := info.Version
		if version == "" {
			version = "(no version)"
		}
		fmt.Fprintf(out, "  %-24s %-10s %s\n", info.Name, version, info.Description)
	}

	if len(loaded) == 0 {
		fmt.Fprintf(out, "\nNo plugins are linked. This is a legitimate configuration -- a worker\n"+
			"that runs workflows and no plugins -- but if you expected some, the cause\n"+
			"is a missing blank import in cmd/cleat-worker/main.go rather than\n"+
			"configuration.\n")
	}
	return 0
}
