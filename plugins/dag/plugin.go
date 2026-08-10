// Package dag provides the host-side half of the DAG composition model:
// DAGSpec/TaskSpec (a JSON-serializable DAG description) and ParseSpec,
// which structurally validates a spec without needing the cleat/ SDK. It
// also registers "dag" as a discoverable plugin.
//
// The guest-side half -- the runtime DAG type, TaskContext, Execute,
// ExecuteWithOptions, and the registry-based LoadFromJSON that wires task
// functions and builds an executable graph -- lives in
// github.com/cleat-team/cleat/cleat/dagrun, inside the cleat/ module.
//
// That split exists because cmd/cleat (a production, `go install`-able
// binary in the root module) only ever needed the spec/validation half:
// `cleat dag validate` and the `run`/`generate` code generators call
// ParseSpec (directly, or indirectly via LoadFromJSON's old signature) and
// discard everything but the error. Nothing in the root module ever
// constructed a runtime DAG or touched TaskContext. But TaskContext.H is
// handed to every user-written task body, which can legitimately call any
// cleat.HostCalls method, not just the two (ChildWorkflowWithOptions,
// AwaitAnyChild) this package's own scheduler calls -- so TaskContext
// cannot be narrowed to a small interface without breaking real callers
// (examples/dag calls ctx.H.DurableCall directly). The only import-cycle-
// safe place for a type that legitimately needs the full cleat.HostCalls
// is the module that already defines it: cleat/. Keeping DAGSpec/TaskSpec
// here and the runtime in cleat/dagrun means cmd/cleat depends on neither
// cleat.HostCalls nor cleat/dagrun, and the root module never imports the
// cleat/ SDK module at all.
package dag

import (
	"context"
	"log/slog"

	"github.com/cleat-team/cleat/plugin"
)

// New creates a new Plugin instance.
func New() plugin.Plugin {
	return &Plugin{}
}

// Plugin registers the dag library as a loadable plugin.
type Plugin struct {
	logger *slog.Logger
}

func init() {
	plugin.Register(plugin.PluginInfo{
		Name:        "dag",
		Version:     "0.1.0",
		Description: "DAG composition model -- execute workflows as directed acyclic graphs built on child workflow primitives",
		Author:      "cleat",
	}, func() plugin.Plugin { return &Plugin{} })
}

// Info returns plugin metadata for discovery and documentation.
func (p *Plugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{
		Name:        "dag",
		Version:     "0.1.0",
		Description: "DAG composition model -- execute workflows as directed acyclic graphs built on child workflow primitives",
		Author:      "cleat",
	}
}

// Init initializes the plugin with the given environment.
func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
	if env.Logger != nil {
		p.logger = env.Logger
	} else {
		p.logger = slog.Default()
	}
	p.logger.Info("dag plugin initialized")
	return nil
}
