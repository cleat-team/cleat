// Package tenantlifecycle sweeps for tenants whose trial has expired and
// suspends them. cleat#2534.
//
// IT OWNS NO PART OF SUSPENSION ITSELF. Suspending a tenant is
// plugin.Environment.SetTenantSuspended, a host-owned grant this plugin
// calls -- writing admin.tenants directly from a plugin's own cross-tenant
// statement fails closed (that table carries no grant for the cleat_sweep
// role a bypass runs under). What this plugin owns is deciding WHEN: its own
// tenant_trials table, one row per tenant with a trial in flight, populated
// only by cleatctl set-tenant-trial (never by a workflow -- see background.go
// for why a guest-writable expiry defeats its own purpose) and swept by
// Run below.
package tenantlifecycle

import (
	"context"
	"log/slog"

	"github.com/cleat-team/cleat/plugin"
)

func init() {
	plugin.Register(plugin.PluginInfo{
		Name:        "tenant-lifecycle",
		Version:     "0.1.0",
		Description: "Suspend a tenant whose trial has expired",
		Author:      "cleat",
	}, func() plugin.Plugin {
		return &Plugin{}
	})
}

// New creates a new Plugin instance.
func New() plugin.Plugin {
	return &Plugin{}
}

// Plugin implements the trial-expiry sweep.
type Plugin struct {
	db      plugin.PluginDB
	logger  *slog.Logger
	dialect plugin.Dialect
	env     *plugin.Environment
}

// Info returns plugin metadata for discovery and documentation.
func (p *Plugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{
		Name:        "tenant-lifecycle",
		Version:     "0.1.0",
		Description: "Suspend a tenant whose trial has expired",
		Author:      "cleat",
	}
}

// Init initialises the plugin with the given environment.
func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
	if env.Logger != nil {
		p.logger = env.Logger
	} else {
		p.logger = slog.Default()
	}
	p.db = env.DB
	p.env = env
	p.dialect = env.Dialect
	p.logger.Info("tenant-lifecycle: initialized")
	return nil
}
