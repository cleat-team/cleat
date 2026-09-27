// Package tenantquota bounds how much a tenant CONSUMES, as opposed to how
// long any one invocation may run.
//
// cleat#1569. tenant_settings holds four per-tenant ceilings and all four are
// durations: guest execution time, wall clock for one invocation, host retry
// backoff, and whole-execution wall clock. Each is stateless -- min(tenant,
// operator) evaluated at execution time -- and none of them can express "this
// plan includes 10,000 runs a month", which is the unit every product built on
// cleat actually sells.
//
// # Why a plugin rather than core
//
// The enforcement point is an HTTP request, and plugin middleware already
// wraps it. cmd/cleat-worker/main.go registers the CORE route table on the
// plugin mux (`mux := plugMux`, then `registerRoutes(mux, api)`) and serves
// `plugHandler`, which is that mux wrapped in every plugin's Middleware. So a
// plugin sees POST /api/workflows/:name/start exactly as it sees its own
// routes.
//
// That is easy to read the other way -- `plugHandler = p.Middleware(plugHandler)`
// looks like it wraps only plugin routes -- and reading it that way is what
// makes this look like a core change needing migrations on three dialects and
// store methods per dialect. It is not.
//
// # What it deliberately does not do
//
// Not a rate limiter. plugins/ratelimiter bounds requests per second with the
// same bucketed-counter shape; this bounds consumption over a window orders of
// magnitude longer, with a write rate low enough that contention is not the
// design problem it is there.
package tenantquota

import (
	"context"
	"log/slog"

	"github.com/cleat-team/cleat/plugin"
)

// ResourceWorkflowStarts is the first and currently only metered resource.
//
// Chosen because it is the only candidate that needs no new reporting from
// anything else: a start is already an HTTP request this plugin can see. Token
// and spend quotas need the llm plugin to report usage per call, which it does
// not do today, so those are a second change with a dependency rather than the
// first one.
const ResourceWorkflowStarts = "workflow_starts"

func init() {
	plugin.Register(plugin.PluginInfo{
		Name:        "tenant-quota",
		Version:     "0.1.0",
		Description: "Per-tenant consumption quotas over a rolling window",
		Author:      "cleat",
	}, func() plugin.Plugin {
		return &Plugin{}
	})
}

// New creates a new Plugin instance.
func New() plugin.Plugin { return &Plugin{} }

// Plugin counts what a tenant consumes and, where a quota says so, refuses
// further consumption.
type Plugin struct {
	db      plugin.PluginDB
	logger  *slog.Logger
	dialect plugin.Dialect
}

// Info returns plugin metadata for discovery and documentation.
func (p *Plugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{
		Name:        "tenant-quota",
		Version:     "0.1.0",
		Description: "Per-tenant consumption quotas over a rolling window",
		Author:      "cleat",
	}
}

// Init wires the plugin to its database and logger.
//
// NO CONFIG, AND NO MODE. ratelimiter has a memory mode and a db mode, and
// cleat#1581 is the cost of that choice: an unrecognised mode silently selected
// per-process limiting, which is the outcome configuring the field was meant to
// prevent. A quota is meaningless per-process -- N workers would each grant the
// full allowance -- so there is no in-memory variant to select wrongly.
//
// A missing database is refused rather than degraded, for the same reason
// #1588 gives about a cluster-wide limit that cannot be honoured: starting
// without one would leave every quota silently unenforced while the operator's
// configuration said otherwise.
func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
	if env.Logger != nil {
		p.logger = env.Logger
	} else {
		p.logger = slog.Default()
	}
	p.db = env.DB
	p.dialect = env.Dialect

	if p.db == nil {
		return errNoDatabase
	}
	p.logger.Info("tenant-quota: initialized")
	return nil
}
