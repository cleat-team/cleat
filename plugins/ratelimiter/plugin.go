// Package ratelimiter provides per-tenant rate limiting middleware using a
// token bucket algorithm. Rate limits are stored in PostgreSQL and cached
// in memory for fast middleware checks. The background goroutine reloads
// configs from the database every 30 seconds.
package ratelimiter

import (
	"context"
	"log/slog"
	"sync"

	"github.com/rcownie/cleat/internal/plugin"
)

func init() {
	plugin.Register(plugin.PluginInfo{
		Name:        "rate-limiter",
		Version:     "0.1.0",
		Description: "Per-tenant rate limiting middleware",
		Author:      "cleat",
	}, func() plugin.Plugin {
		return &Plugin{}
	})
}

// Plugin implements per-tenant rate limiting with a token bucket algorithm.
// It stores rate limit configurations in PostgreSQL and maintains an
// in-memory cache of token buckets for fast middleware checks.
type Plugin struct {
db      plugin.PluginDB
	logger  *slog.Logger
	dialect plugin.Dialect

	mu      sync.Mutex
	buckets map[string]*tokenBucket // key: "tenantUUID/limit_key"
}

// Info returns plugin metadata for discovery and documentation.
func (p *Plugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{
		Name:        "rate-limiter",
		Version:     "0.1.0",
		Description: "Per-tenant rate limiting middleware",
		Author:      "cleat",
	}
}

// Init initializes the plugin with the given environment. It sets up the
// in-memory token bucket cache and stores references to the database and
// logger provided by the host.
func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
	if env.Logger != nil {
		p.logger = env.Logger
	} else {
		p.logger = slog.Default()
	}

	p.db = env.DB
	p.dialect = env.Dialect
	p.buckets = make(map[string]*tokenBucket)

	p.logger.Info("rate-limiter: initialized")
	return nil
}
