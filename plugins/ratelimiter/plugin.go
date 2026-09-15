// Package ratelimiter provides per-tenant rate limiting middleware using a
// token bucket algorithm. Rate limits are stored in PostgreSQL and cached
// in memory for fast middleware checks. The background goroutine reloads
// configs from the database every 30 seconds.
package ratelimiter

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/cleat-team/cleat/plugin"
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

// New creates a new Plugin instance.
func New() plugin.Plugin {
	return &Plugin{}
}

// Config holds optional configuration for the rate-limiter plugin.
// The modes this plugin understands. They are constants because Init now
// REFUSES anything else, and a refusal that compares against a loose string
// literal is one typo away from refusing a mode that works.
const (
	modeMemory = "memory"
	modeDB     = "db"
)

type Config struct {
	// Mode selects the rate-limiting backend: "memory" (default) for
	// per-worker token buckets, or "db" for a DB-backed sliding-window
	// counter that enforces limits across all workers.
	Mode string `json:"mode"`
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

	// DB-backed mode
	mode string // "memory" (default) or "db"
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
// logger provided by the engine.
func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
	if env.Logger != nil {
		p.logger = env.Logger
	} else {
		p.logger = slog.Default()
	}

	p.db = env.DB
	p.dialect = env.Dialect
	p.buckets = make(map[string]*tokenBucket)
	p.mode = modeMemory

	if len(env.Config) > 0 {
		var cfg Config
		if err := json.Unmarshal(env.Config, &cfg); err != nil {
			p.logger.Error("rate-limiter: invalid config", "error", err)
			return err
		}
		if cfg.Mode != "" {
			switch cfg.Mode {
			case modeMemory, modeDB:
				p.mode = cfg.Mode
			default:
				// An unrecognised mode is REFUSED rather than ignored, and
				// this is the same defect as the one below reached through a
				// different door. middleware.go asks `p.mode == "db"` and
				// treats everything else as memory, so `"DB"`, `"database"`,
				// `"postgres"` and every other near-miss used to select
				// per-process limiting -- the exact outcome the operator was
				// trying to avoid by setting the field at all. cleat#1581.
				return fmt.Errorf("rate-limiter: unknown mode %q: expected %q or %q -- "+
					"an unrecognised mode would silently select per-process limiting, "+
					"which is what configuring this field is meant to prevent",
					cfg.Mode, modeMemory, modeDB)
			}
		}
	}

	// A CLUSTER-WIDE LIMIT THAT CANNOT BE HONOURED IS REFUSED, NOT DOWNGRADED.
	//
	// This used to log a Warn and set p.mode = "memory". A warning is not a
	// refusal: the worker started, and every tenant got a per-process limiter
	// while the configuration said otherwise. p.buckets is an in-process map,
	// so N workers served N times the configured rate -- and the operator who
	// wrote `mode: "db"` had said, in the only way the plugin offers, that
	// this was the thing not to do.
	//
	// Refusing is safe for everyone who is not already broken: it fires only
	// when a deployment EXPLICITLY asked for db mode, since the default is
	// memory and an absent config still yields memory. A deployment that never
	// mentions mode sees no change. cleat#1581.
	if p.mode == modeDB && p.db == nil {
		return fmt.Errorf("rate-limiter: mode %q requires a database and none is configured -- "+
			"refusing to start rather than falling back to per-process limits, which would "+
			"serve each worker the full configured rate", modeDB)
	}

	p.logger.Info("rate-limiter: initialized", "mode", p.mode)
	return nil
}
