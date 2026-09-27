// Package auditlog provides a comprehensive audit trail of all API access.
// It records every HTTP request, creates queryable audit events, and has
// a background retention cleanup.
package auditlog

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

func init() {
	plugin.Register(plugin.PluginInfo{
		Name:        "audit-log",
		Version:     "0.1.0",
		Description: "Comprehensive audit trail of all API access",
		Author:      "cleat",
	}, func() plugin.Plugin {
		return &Plugin{}
	})
}

// New creates a new Plugin instance.
func New() plugin.Plugin {
	return &Plugin{}
}

// queuedAuditEvent is the internal type used for deferred audit writes.
// It is distinct from auditEvent in routes.go which is used for JSON serialization.
type queuedAuditEvent struct {
	// id and ts are fixed when the request finishes, so a retry is idempotent by id and the row
	// says when the request happened (see queue.go).
	id         uuid.UUID
	ts         time.Time
	tenantID   uuid.UUID
	userID     string
	method     string
	path       string
	statusCode int
	ipAddress  string
	userAgent  string
	duration   time.Duration
}

// Plugin implements audit trail recording and querying.
type Plugin struct {
	db      plugin.PluginDB
	mux     *http.ServeMux
	logger  *slog.Logger
	config  Config
	dialect plugin.Dialect
	buffer  chan queuedAuditEvent // bounded queue of events waiting for a worker

	// q is the queue's accounting and shutdown state (queue.go).
	q queueState
	// eventsLost reports each lost event to the host; nil when nobody is counting.
	eventsLost func(pluginName, reason string, n int64)

	// headsSeen remembers the tenants whose chain head row this process has ensured
	// exists, so the steady-state append does not repeat the insert. See ensureHead.
	headsSeen sync.Map

	// now is the clock retention measures its cutoff from; nil means time.Now. A test
	// sets it, because the rows it wants expired are hashed and cannot be back-dated.
	now func() time.Time
}

// Config controls audit-log behaviour.
type Config struct {
	RetentionDays int `json:"retention_days"` // default 90

	// The queue (queue.go). Each is used only when positive; the defaults are the ones the
	// owner chose on cleat#2168: an enqueue waits up to 1s for room, a failed append is retried
	// for up to 60s, and shutdown drains for up to 10s.
	BufferSize      int `json:"audit_buffer_size"`       // events waiting for a worker; default 1000
	Workers         int `json:"audit_workers"`           // appends in parallel; default 4
	EnqueueWaitMs   int `json:"audit_enqueue_wait_ms"`   // how long a full queue holds a request; default 1000
	RetryDeadlineMs int `json:"audit_retry_deadline_ms"` // how long an event is retried; default 60000
	ShutdownDrainMs int `json:"audit_shutdown_drain_ms"` // how long shutdown drains; default 10000
}

// Info returns plugin metadata for discovery and documentation.
func (p *Plugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{
		Name:        "audit-log",
		Version:     "0.1.0",
		Description: "Comprehensive audit trail of all API access",
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
	p.mux = env.Mux
	p.dialect = env.Dialect

	// Parse config. If no config provided, use safe defaults.
	if len(env.Config) > 0 {
		if err := json.Unmarshal(env.Config, &p.config); err != nil {
			return fmt.Errorf("audit-log: invalid config: %w", err)
		}
	}
	if p.config.RetentionDays <= 0 {
		p.config.RetentionDays = 90
	}

	p.logger.Info("audit-log: initialized",
		"retention_days", p.config.RetentionDays,
	)

	p.eventsLost = env.EventsLost
	p.buffer = make(chan queuedAuditEvent, p.config.bufferSize())
	return nil
}

// RegisterRoutes registers HTTP routes for querying audit events.
func (p *Plugin) RegisterRoutes(mux plugin.Router) error {
	if mux == nil {
		return fmt.Errorf("audit-log: nil mux")
	}
	mux.HandleFunc("GET /audit/events", p.handleQueryEvents)
	mux.HandleFunc("GET /audit/export", p.handleExport)
	mux.HandleFunc("GET /audit/verify", p.handleVerify)
	return nil
}
